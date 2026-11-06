package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/bmcreations/loupe/internal/target"
)

// maxPresetName and maxPresetSettings bound what one preset can hold. A
// preset is a handful of device settings with a name a person typed; these
// keep a caller from making the server store or replay something larger.
const (
	maxPresetName     = 64
	maxPresetSettings = 32
)

// Preset is a named set of device settings, in the same id -> value shape a
// PUT /api/settings "changes" body uses, so applying one is that PUT.
type Preset struct {
	Name     string         `json:"name"`
	Settings map[string]any `json:"settings"`
}

// PresetsResponse is what GET, PUT and DELETE /api/presets answer with: every
// saved preset, sorted by name.
type PresetsResponse struct {
	Presets []Preset `json:"presets"`
}

// PresetApplyResponse is what a successful POST /api/presets/apply answers
// with: the device's settings read back after applying, and the preset's
// settings this target does not have or cannot change, which were left out
// rather than failing the rest. A preset saved from a foldable emulator
// carries posture; applied to a phone, that one is skipped.
type PresetApplyResponse struct {
	Settings []target.Setting `json:"settings"`
	Skipped  []string         `json:"skipped"`
}

// presetStore keeps presets in memory and, once a path is set, in
// presets.json beside handsoff.json.
type presetStore struct {
	mu      sync.Mutex
	path    string
	presets map[string]Preset
}

// SetPresetsPath loads the presets from path and keeps writing them there.
func (s *Server) SetPresetsPath(path string) error { return s.presets.load(path) }

func (p *presetStore) load(path string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.path = path
	var f PresetsResponse
	if err := readJSONFile(path, &f); err != nil {
		return err
	}
	for _, pr := range f.Presets {
		if validatePreset(pr) == nil {
			p.set(pr)
		}
	}
	return nil
}

// set stores pr. Called with mu held.
func (p *presetStore) set(pr Preset) {
	if p.presets == nil {
		p.presets = map[string]Preset{}
	}
	p.presets[pr.Name] = pr
}

// list returns every preset sorted by name. Called with mu held.
func (p *presetStore) list() []Preset {
	out := make([]Preset, 0, len(p.presets))
	for _, pr := range p.presets {
		out = append(out, pr)
	}
	slices.SortFunc(out, func(a, b Preset) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// save writes the presets. Called with mu held.
func (p *presetStore) save() error {
	if p.path == "" {
		return nil
	}
	return writeJSONFile(p.path, PresetsResponse{Presets: p.list()})
}

func (p *presetStore) get(name string) (Preset, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pr, ok := p.presets[name]
	return pr, ok
}

// validatePreset checks a preset's shape. Whether each value suits its
// setting is the target's call, made when the preset is applied: the same
// preset can be valid on one device and partly skipped on another.
func validatePreset(pr Preset) error {
	if strings.TrimSpace(pr.Name) != pr.Name || pr.Name == "" {
		return fmt.Errorf("name must be non-empty with no leading or trailing space")
	}
	if utf8.RuneCountInString(pr.Name) > maxPresetName {
		return fmt.Errorf("name is longer than %d characters", maxPresetName)
	}
	if len(pr.Settings) == 0 {
		return fmt.Errorf("settings must not be empty")
	}
	if len(pr.Settings) > maxPresetSettings {
		return fmt.Errorf("a preset holds at most %d settings", maxPresetSettings)
	}
	for id := range pr.Settings {
		if id == "" || len(id) > 64 {
			return fmt.Errorf("setting id %q is not valid", id)
		}
	}
	return nil
}

// handlePresets lists (GET), saves (PUT, replacing one with the same name)
// and deletes (DELETE ?name=) presets. Presets belong to this server, not to
// a target, so none of these take ?target=.
func (s *Server) handlePresets(w http.ResponseWriter, r *http.Request) {
	p := s.presets
	switch r.Method {
	case http.MethodGet:
	case http.MethodPut:
		var pr Preset
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSettingsBody)).Decode(&pr); err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
			return
		}
		if err := validatePreset(pr); err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
			return
		}
		p.mu.Lock()
		p.set(pr)
		err := p.save()
		p.mu.Unlock()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "save presets: " + err.Error()})
			return
		}
		s.trace.Record(map[string]any{"kind": "presets", "action": "save", "name": pr.Name})
	case http.MethodDelete:
		name := r.URL.Query().Get("name")
		p.mu.Lock()
		_, ok := p.presets[name]
		var err error
		if ok {
			delete(p.presets, name)
			err = p.save()
		}
		p.mu.Unlock()
		if !ok {
			writeJSON(w, http.StatusNotFound, ErrorResponse{Error: fmt.Sprintf("no preset %q", name)})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "save presets: " + err.Error()})
			return
		}
		s.trace.Record(map[string]any{"kind": "presets", "action": "delete", "name": name})
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
		return
	}
	p.mu.Lock()
	resp := PresetsResponse{Presets: p.list()}
	p.mu.Unlock()
	writeJSON(w, http.StatusOK, resp)
}

// handlePresetApply applies ?name= to ?target= through the same path as PUT
// /api/settings, so a failure partway rolls back every setting it changed.
// Settings the target does not list, or lists as unsupported, are skipped
// and named in the answer; a preset with nothing left to apply is 409.
func (s *Server) handlePresetApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
		return
	}
	name := r.URL.Query().Get("name")
	pr, ok := s.presets.get(name)
	if !ok {
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: fmt.Sprintf("no preset %q", name)})
		return
	}
	t, ok := s.agentTarget(w, r)
	if !ok {
		return
	}
	ds, ok := t.(target.DeviceSettings)
	if g, gated := t.(target.SettingsGate); ok && gated {
		ctx, cancel := context.WithTimeout(r.Context(), agentTimeout)
		ok = g.HasSettings(ctx)
		cancel()
	}
	if !ok {
		writeJSON(w, http.StatusNotImplemented, ErrorResponse{
			Error: fmt.Sprintf("target %q has no device settings", t.ID()),
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), agentTimeout)
	current, err := ds.Settings(ctx)
	cancel()
	if err != nil {
		s.traceAgent(r, t.ID(), "preset-apply", err)
		writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: err.Error()})
		return
	}
	changes, skipped := presetChanges(pr, current)
	if len(changes) == 0 {
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Error: fmt.Sprintf("none of preset %q's settings can be changed on %s", name, t.ID()),
		})
		return
	}
	s.applySettings(w, r, t.ID(), ds, changes, map[string]any{"action": "preset-apply", "preset": name, "skipped": skipped},
		func(settings []target.Setting) any {
			return PresetApplyResponse{Settings: nonNilSettings(settings), Skipped: skipped}
		})
}

// presetChanges splits a preset into the changes this device can take and
// the ids it cannot: not listed by the device, or listed as unsupported.
// skipped is sorted and never nil.
func presetChanges(pr Preset, current []target.Setting) (changes map[string]any, skipped []string) {
	supported := map[string]bool{}
	for _, st := range current {
		supported[st.ID] = st.Supported
	}
	changes = map[string]any{}
	skipped = []string{}
	for id, v := range pr.Settings {
		if supported[id] {
			changes[id] = v
		} else {
			skipped = append(skipped, id)
		}
	}
	slices.Sort(skipped)
	return changes, skipped
}
