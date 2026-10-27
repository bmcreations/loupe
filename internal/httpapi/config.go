package httpapi

import (
	"io"
	"net/http"
	"sync"

	"github.com/bmcreations/loupe/internal/config"
)

// maxConfigBody bounds a PUT. The document has nine scalar fields; anything
// larger is a mistake or an attempt at one.
const maxConfigBody = 64 << 10

// ConfigStore is the server's half of the settings contract: it owns the file,
// applies what can be applied without a restart, and knows which settings a
// command-line flag is overriding.
//
// The flag set is the part that is easy to leave out and expensive to leave
// out. A flag beats the file at startup, so a UI that writes maxFps to the file
// while -max-fps is on the command line has changed nothing the running server
// will read. Saying so in the response is the difference between a setting that
// did not apply and a setting that appears to have applied.
type ConfigStore struct {
	path   string
	pinned map[string]bool
	apply  func(config.Config)

	mu  sync.Mutex
	cur config.Config
	// startup is the configuration in force at boot, flags included.
	startup config.Config
}

// NewConfigStore builds a store over path, starting from cur. pinned names the
// settings a flag is overriding, and apply receives every accepted change so
// the live settings reach the manager.
func NewConfigStore(path string, cur config.Config, pinned map[string]bool, startup config.Config, apply func(config.Config)) *ConfigStore {
	if pinned == nil {
		pinned = map[string]bool{}
	}
	if apply == nil {
		apply = func(config.Config) {}
	}
	return &ConfigStore{path: path, pinned: pinned, apply: apply, cur: cur, startup: startup}
}

// Path is where the configuration is persisted.
func (s *ConfigStore) Path() string { return s.path }

// Current returns a copy of the live configuration.
func (s *ConfigStore) Current() config.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// configField is one setting as the API reports it.
type configField struct {
	Name string `json:"name"`
	// Value is what the server is actually using. FileValue is what is
	// persisted. They differ exactly when a flag pins the setting, and a UI
	// that shows only the second renders a number the server is not using.
	Value     any          `json:"value"`
	FileValue any          `json:"fileValue"`
	Applies   config.Apply `json:"applies"`
	Doc       string       `json:"doc"`
	Source    string       `json:"source"`
	Pinned    bool         `json:"pinned"`
}

type ConfigResponse struct {
	Path   string        `json:"path"`
	Fields []configField `json:"fields"`
}

type PutResponse struct {
	Changed         []string      `json:"changed"`
	RestartRequired []string      `json:"restartRequired"`
	Pinned          []string      `json:"pinned"`
	Fields          []configField `json:"fields"`
}

// handleConfig reads and writes the persisted configuration.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if s.cfg == nil {
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "configuration is not enabled"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, ConfigResponse{Path: s.cfg.Path(), Fields: s.cfg.fields()})
	case http.MethodPut:
		s.cfg.put(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
	}
}

// fields renders the current configuration with, for each setting, where its
// value came from.
func (s *ConfigStore) fields() []configField {
	cur := s.Current()
	values := cur.Values()
	inForce := s.startup.Values()
	def := config.Default().Values()

	out := make([]configField, 0, len(config.Fields()))
	for _, f := range config.Fields() {
		source := "default"
		switch {
		case s.pinned[f.Name]:
			source = "flag"
		case values[f.Name] != def[f.Name]:
			source = "file"
		}
		// A pinned setting keeps the value the command line gave it for the
		// life of the process, however many times the file is rewritten.
		effective := values[f.Name]
		if s.pinned[f.Name] {
			effective = inForce[f.Name]
		}
		out = append(out, configField{
			Name:      f.Name,
			Value:     effective,
			FileValue: values[f.Name],
			Applies:   f.Applies,
			Doc:       f.Doc,
			Source:    source,
			Pinned:    s.pinned[f.Name],
		})
	}
	return out
}

// put validates a partial update, persists it, and applies what it can.
//
// Nothing is written and nothing is applied unless the whole patch is valid.
// A half-applied settings change is worse than a rejected one: the caller has
// no way to find out which half landed.
func (s *ConfigStore) put(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxConfigBody))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "read body: " + err.Error()})
		return
	}

	s.mu.Lock()
	next, changed, err := config.Merge(s.cur, body)
	if err != nil {
		s.mu.Unlock()
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	if err := config.Save(s.path, next); err != nil {
		s.mu.Unlock()
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "save: " + err.Error()})
		return
	}
	s.cur = next
	s.mu.Unlock()

	s.apply(next)

	applies := map[string]config.Apply{}
	for _, f := range config.Fields() {
		applies[f.Name] = f.Applies
	}
	if changed == nil {
		// config.Merge's changedFields returns nil, not [], when the patch
		// left every field equal to what it already was. Changed carries no
		// omitempty, so the schema promises an array; nil would marshal as
		// null and break a caller that ranges over it unconditionally.
		changed = []string{}
	}
	resp := PutResponse{
		Changed:         changed,
		RestartRequired: []string{},
		Pinned:          []string{},
		Fields:          s.fields(),
	}
	for _, name := range changed {
		if applies[name] == config.AppliesRestart {
			resp.RestartRequired = append(resp.RestartRequired, name)
		}
		if s.pinned[name] {
			resp.Pinned = append(resp.Pinned, name)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// SetConfig attaches a settings store. Without one, /api/config answers 404
// rather than pretending to have settings it cannot persist.
func (s *Server) SetConfig(store *ConfigStore) { s.cfg = store }
