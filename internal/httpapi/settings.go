package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// maxSettingsBody bounds a PUT's body. The largest legitimate payload is five
// bool/float changes; this is generous for that and small enough that a
// caller cannot make the server buffer an arbitrary amount of JSON.
const maxSettingsBody = 16 << 10

// settingsRereadTimeout bounds the re-read putSettings does after a rollback,
// the same way rollback itself is bounded in target/settings.go. Both run
// under a context detached from the request: a 409 means something already
// went wrong communicating with the device, often because the request's own
// deadline is what triggered the failure, and reusing that context would
// mean the re-read that is supposed to tell the caller what state the device
// is actually in never gets to run.
// It is a var, not a const, so a test can shrink it to prove it is actually
// enforced rather than merely detaching the context: a re-read that never
// returns on its own must still complete the request, bounded, and a
// request-scoped context (which the detach alone would not provide a
// deadline for) is not enough to demonstrate that on a fast test.
var settingsRereadTimeout = 10 * time.Second

// SettingsResponse is what both GET and a successful PUT return: the full,
// freshly-read set of settings, the same shape /api/config uses so a caller
// never has to guess whether a write actually took. Settings carries no
// omitempty -- the schema promises an array -- so both callers that build
// one (getSettings and putSettings' success path) normalize a nil
// target.DeviceSettings.Settings result to [] before constructing it, the
// same null-array fix already made for handleUI's Nodes, PUT /api/config's
// changed and SettingsConflictResponse.Failed.
type SettingsResponse struct {
	Settings []target.Setting `json:"settings"`
}

// nonNilSettings normalizes a nil settings slice to an empty one, so
// SettingsResponse.Settings always marshals as [] rather than null. A
// target.DeviceSettings implementation returning nil alongside a nil error
// is a valid "no settings" answer, not an error, and the wire contract
// promises an array either way.
func nonNilSettings(settings []target.Setting) []target.Setting {
	if settings == nil {
		return []target.Setting{}
	}
	return settings
}

// SettingsConflictResponse is what a PUT returns when applying a change
// failed partway through and some settings had to be rolled back. Failed
// names the setting that failed and everything after it in table order,
// which never ran at all -- always an array, never omitted, because Failed
// carries no omitempty in the schema and there is always at least one name
// in it on this path (see putSettings). NotRestored, when present, names
// any already-applied setting whose rollback write itself failed -- best
// effort, not guaranteed.
//
// Settings is a pointer, not a plain slice, because the field's presence and
// its value are two separate facts and only a pointer can carry both: the
// re-read after a rollback either succeeds, in which case the caller gets
// whatever ds.Settings() returned -- however that happens to marshal, [] or
// null included -- or it fails, in which case the key is absent entirely so
// a caller does not mistake "settings" was never read for "there are no
// settings". A plain []target.Setting with omitempty cannot make that
// distinction: encoding/json's omitempty drops an empty slice exactly the
// same way it drops a nil one, which would silently change what a caller
// already relying on the field's presence sees. See
// TestSettingsConflictResponseMatchesTheOldMapEncoding, which pins this
// against the map-literal encoding this type replaced.
type SettingsConflictResponse struct {
	Error       string            `json:"error"`
	Failed      []string          `json:"failed"`
	NotRestored []string          `json:"notRestored,omitempty"`
	Settings    *[]target.Setting `json:"settings,omitempty"`
}

// handleSettings reads or changes device-wide settings (dark mode, font
// scale, reduce motion, layout bounds, TalkBack). Unlike /api/config, which
// changes what a *future* streaming session launches with, these take effect
// on the device immediately -- there is no session to queue behind.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		w.Header().Set("Allow", "GET, PUT")
		writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
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

	switch r.Method {
	case http.MethodGet:
		s.getSettings(w, r, t.ID(), ds)
	case http.MethodPut:
		s.putSettings(w, r, t.ID(), ds)
	}
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request, id string, ds target.DeviceSettings) {
	ctx, cancel := context.WithTimeout(r.Context(), agentTimeout)
	defer cancel()

	settings, err := ds.Settings(ctx)
	if err != nil {
		s.traceAgent(r, id, "settings", err)
		writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: err.Error()})
		return
	}
	s.traceAgent(r, id, "settings", nil)
	writeJSON(w, http.StatusOK, SettingsResponse{Settings: nonNilSettings(settings)})
}

// SettingsPutRequest carries only the settings the caller wants to change,
// the same "changes" shape /api/config uses, so a PUT never has to restate
// every field to change one of them.
type SettingsPutRequest struct {
	Changes map[string]any `json:"changes"`
}

// putSettings applies changes and reports what took effect. A request
// rejected before anything ran -- an unknown id, or a value outside its
// field's range -- is 400, the same as a malformed body: nothing on the
// device changed, so there is nothing to report as failed. A failure partway
// through applying (409) is different: some settings may have been read and
// rolled back, and the response carries which ones never took along with a
// freshly-read settings list when the device is reachable enough to read it
// back, for the same reason /api/config's PUT does -- a caller that cannot
// see what happened will assume the whole request took.
func (s *Server) putSettings(w http.ResponseWriter, r *http.Request, id string, ds target.DeviceSettings) {
	var req SettingsPutRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSettingsBody)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	if len(req.Changes) == 0 {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "changes must not be empty"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), agentTimeout)
	defer cancel()

	failed, notRestored, applyErr := ds.ApplySettings(ctx, req.Changes)

	rec := map[string]any{
		"kind": "agent", "target": id, "action": "settings-put",
		"ua": r.UserAgent(), "changes": req.Changes,
	}
	if len(failed) > 0 {
		rec["failed"] = failed
	}
	if len(notRestored) > 0 {
		rec["notRestored"] = notRestored
	}
	if applyErr != nil {
		rec["err"] = applyErr.Error()
	}
	s.trace.Record(rec)

	if applyErr != nil {
		if errors.Is(applyErr, target.ErrInvalidSetting) {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: applyErr.Error()})
			return
		}
		if errors.Is(applyErr, target.ErrDeviceUnreachable) {
			// Nothing was applied -- this fails before ApplySettings writes
			// anything -- but unlike ErrInvalidSetting the cause is the
			// device not answering, not the request being wrong, so this is
			// 502 rather than 400. A caller that retried a 400 forever
			// against a device that is merely offline would be learning the
			// wrong lesson from it.
			writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: applyErr.Error()})
			return
		}
		if failed == nil {
			// Failed carries no omitempty -- the schema promises an array --
			// and every path here is expected to name at least one setting,
			// but nothing enforces that at the type level, so normalise
			// defensively rather than let a future change under this branch
			// wire null onto the client.
			failed = []string{}
		}
		resp := SettingsConflictResponse{Error: applyErr.Error(), Failed: failed, NotRestored: notRestored}
		// The re-read runs detached from the request context and with its
		// own timeout, the same reasoning as rollback in
		// target/settings.go: a 409 usually means the request's own
		// deadline is already tight or expired, and reusing it here would
		// mean the one read meant to tell the caller what state the device
		// is actually in never gets to run.
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(r.Context()), settingsRereadTimeout)
		defer rcancel()
		if settings, err := ds.Settings(rctx); err == nil {
			resp.Settings = &settings
		}
		writeJSON(w, http.StatusConflict, resp)
		return
	}

	settings, err := ds.Settings(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, SettingsResponse{Settings: nonNilSettings(settings)})
}
