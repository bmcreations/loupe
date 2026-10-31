package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// deviceLifeTimeout bounds one shutdown or delete. An emulator's console
// kill and a simctl shutdown both finish in seconds; a wedged one should
// answer 502 rather than hold the request.
const deviceLifeTimeout = 45 * time.Second

// deviceActions is which picker actions a target offers. The client's
// rowActions in web/drawer.js is the same rule; keep them together.
func deviceActions(ti TargetInfo) []string {
	if !strings.HasPrefix(ti.ID, "avd:") && !strings.HasPrefix(ti.ID, "sim:") {
		return nil
	}
	if ti.State == "creating" || !ti.Available {
		return nil
	}
	if ti.BootsOnOpen {
		return []string{"delete"}
	}
	return []string{"shutdown", "delete"}
}

// handleDevices serves /api/devices: POST creates, DELETE deletes.
func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		s.handleDeleteDevice(w, r)
		return
	}
	s.handleCreateDevice(w, r)
}

// handleDeviceShutdown is POST /api/devices/shutdown?target=.
func (s *Server) handleDeviceShutdown(w http.ResponseWriter, r *http.Request) {
	s.deviceLife(w, r, "shutdown")
}

// handleDeviceReset is POST /api/devices/reset?target=avd:<name>.
func (s *Server) handleDeviceReset(w http.ResponseWriter, r *http.Request) {
	s.deviceLife(w, r, "reset")
}

// handleDeleteDevice is DELETE /api/devices?target=.
func (s *Server) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
	s.deviceLife(w, r, "delete")
}

// deviceLife shuts down or deletes the target, arbitrated and traced like
// /api/input. Viewers are told first, so a person watching sees "is shut
// down" rather than a dropped stream, and a reconnect is refused while the
// device is going away. A failure lifts that refusal again, since the device
// is still there.
func (s *Server) deviceLife(w http.ResponseWriter, r *http.Request, action string) {
	t, ok := s.agentTarget(w, r)
	if !ok {
		return
	}
	id := t.ID()
	c := s.inputCaller(r, id)
	refuse := func(status int, resp ErrorResponse) {
		s.traceInputWith(r, id, action, c, nil, map[string]any{"outcome": "refused", "code": resp.Code})
		writeJSON(w, status, resp)
	}
	ti, listed := s.mgr.Info(r.Context(), id)
	booting := action == "shutdown" && !lifeAllowed(ti, action) && bootInProgress(r.Context(), t, ti)
	if !lifeAllowed(ti, action) && !booting {
		reason := notSupportedReason(ti, id, action)
		if !listed {
			reason = fmt.Sprintf("%s is not in the target list", id)
		}
		refuse(http.StatusUnprocessableEntity, ErrorResponse{Error: reason, Code: ErrCodeNotSupported})
		return
	}
	// A second request would lift or replace the first one's reopen guard
	// while its Stop or Delete is still running.
	if !s.lifeBegin(id) {
		refuse(http.StatusConflict, ErrorResponse{
			Error: fmt.Sprintf("%s is already being shut down or deleted", id),
			Code:  ErrCodeBusy,
		})
		return
	}
	defer s.lifeEnd(id)
	if s.recordings != nil {
		if m, busy := s.recordings.ActiveFor(id); busy {
			refuse(http.StatusConflict, ErrorResponse{
				Error: fmt.Sprintf("target %q is already recording as %s", id, m.ID),
				Code:  ErrCodeRecording,
				ID:    m.ID,
			})
			return
		}
	}

	if !s.arbitrate(w, r, id, action, c) {
		return
	}

	reason := "shutDown"
	if action == "delete" {
		reason = "deleted"
	}
	platform := "android"
	if strings.HasPrefix(id, "sim:") {
		platform = "ios"
	}

	began := time.Now()
	armed := s.mgr.EndSession(id, reason)
	// Not the request's context: a phone that drops the connection must not
	// leave a device half shut down.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), deviceLifeTimeout)
	defer cancel()
	var err error
	saved := false
	if action == "reset" {
		if rs, ok := t.(target.Resetter); !ok {
			err = fmt.Errorf("%s has no saved state to reset", id)
		} else {
			err = rs.ResetState(ctx)
		}
	} else if action == "delete" {
		// Delete stops the device first, on both platforms.
		if d, ok := t.(target.Deleter); !ok {
			err = fmt.Errorf("%s cannot be deleted", id)
		} else if err = d.Delete(ctx); err == nil {
			s.mgr.Unregister(id)
		}
	} else if st, ok := t.(target.Stopper); !ok {
		err = fmt.Errorf("%s cannot be shut down", id)
	} else {
		// Only a shutdown keeps state: a delete has nothing to resume. A
		// failed save is traced by the target and does not stop the
		// shutdown, which is what was asked for.
		// A boot that never came up has nothing worth resuming, and
		// saving it would make the next quickboot resume a half-booted VM.
		if ss, ok := t.(target.StateSaver); ok && !booting {
			saved, _ = ss.SaveState(ctx)
		}
		err = st.Stop(ctx)
	}
	// The guard has refused every open since EndSession. A failure lifts it,
	// since the device is still there; a shutdown that worked starts its grace
	// now, when the device is down. A delete's went with Unregister.
	outcome := "ok"
	if err != nil {
		outcome = "error"
		s.mgr.ClearEnded(id, armed)
	} else if action != "delete" {
		s.mgr.FinishEnded(id, armed)
	}
	s.traceInputWith(r, id, action, c, err, map[string]any{
		"platform": platform, "tookMs": time.Since(began).Milliseconds(), "outcome": outcome, "saved": saved,
	})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: err.Error()})
		return
	}
	if action == "delete" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	fresh, _ := s.mgr.Info(r.Context(), id)
	writeJSON(w, http.StatusOK, fresh)
}

// bootInProgress reports whether an AVD's emulator process is alive though
// the target has not attached, as when a boot outran ?wait= and adb still
// calls it offline. The picker offers shutdown only once a device attaches,
// so without this a stuck boot could only be stopped by its pid.
func bootInProgress(ctx context.Context, t target.Target, ti TargetInfo) bool {
	if !strings.HasPrefix(ti.ID, "avd:") || ti.State == "creating" || !ti.Available {
		return false
	}
	br, ok := t.(target.BootReporter)
	return ok && br.BootStatus(ctx).ProcessAlive
}

// lifeAllowed reports whether the target takes the action. Reset is not a
// picker action: it works on an AVD whether it is running or not, and only
// on an AVD, since a simulator keeps its state on its own.
func lifeAllowed(ti TargetInfo, action string) bool {
	if action != "reset" {
		return slices.Contains(deviceActions(ti), action)
	}
	return strings.HasPrefix(ti.ID, "avd:") && ti.State != "creating" && ti.Available
}

// notSupportedReason says why a target does not offer the action.
func notSupportedReason(ti TargetInfo, id, action string) string {
	switch {
	case ti.Server != "":
		return fmt.Sprintf("%s belongs to server %s; %s it there", id, ti.Server, action)
	case action == "reset" && !strings.HasPrefix(id, "avd:"):
		return fmt.Sprintf("%s is not a loupe AVD; only an AVD keeps a saved state to reset", id)
	case !strings.HasPrefix(id, "avd:") && !strings.HasPrefix(id, "sim:"):
		return fmt.Sprintf("%s is not a loupe AVD or simulator", id)
	case ti.State == "creating":
		return fmt.Sprintf("%s is still being created", id)
	case !ti.Available:
		return fmt.Sprintf("%s is not available: %s", id, ti.Reason)
	default:
		return fmt.Sprintf("%s is not running, so there is nothing to %s", id, action)
	}
}

// lifeBegin claims the target for one shutdown or delete.
func (s *Server) lifeBegin(id string) bool { return s.mgr.lifeBegin(id) }

// lifeInFlight reports whether a shutdown or delete of the target is running.
func (s *Server) lifeInFlight(id string) bool { return s.mgr.lifeInFlight(id) }

// explicitStart reports whether the request carries start=1, and if so lifts
// the target's shut-down reopen guard. It does not lift it while a shutdown or
// delete of the target is still running: EndSession arms the guard before Stop
// finishes, and ClearShutDown also leaves a guard whose shutdown has not
// returned. Both /ws and /wt call it before Acquire.
func (s *Server) explicitStart(r *http.Request, id string) bool {
	return s.startTarget(r.URL.Query().Get("start") == "1", id)
}

// startTarget is explicitStart for a transport with no URL: start is what
// start=1 would have said.
func (s *Server) startTarget(start bool, id string) bool {
	if !start {
		return false
	}
	if !s.lifeInFlight(id) {
		s.mgr.ClearShutDown(id)
	}
	return true
}

func (s *Server) lifeEnd(id string) { s.mgr.lifeEnd(id) }

// bootTimeout bounds POST /api/devices/boot. An emulator's own BootBudget is
// three minutes, and a first boot with no snapshot took 20s here; this only
// has to outlast that.
const bootTimeout = 4 * time.Minute

// bootWaitMax caps ?wait= on a boot, and bounds the background boot that
// wait=0 starts.
const bootWaitMax = 15 * time.Minute

// parseBootWait reads ?wait= as a Go duration ("90s", "10m") or a bare number
// of seconds. Absent is bootTimeout. Zero means do not wait.
func parseBootWait(v string) (time.Duration, error) {
	if v == "" {
		return bootTimeout, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		secs, serr := strconv.ParseFloat(v, 64)
		if serr != nil {
			return 0, fmt.Errorf("?wait= must be a duration (90s, 10m) or seconds, not %q", v)
		}
		d = time.Duration(secs * float64(time.Second))
	}
	if d < 0 || d > bootWaitMax {
		return 0, fmt.Errorf("?wait= must be between 0 and %s", bootWaitMax)
	}
	return d, nil
}

// handleDeviceBootStatus is GET /api/devices/boot?target=avd:<name>: how an
// AVD's boot is going (process alive, adb state, sys.boot_completed, log
// path), for polling a boot started with wait=0 or one that timed out. It
// reads only; it neither wakes a paused emulator nor takes a claim into account.
func (s *Server) handleDeviceBootStatus(w http.ResponseWriter, r *http.Request) {
	t, ok := s.agentTarget(w, r)
	if !ok {
		return
	}
	br, ok := t.(target.BootReporter)
	if !ok {
		writeJSON(w, http.StatusUnprocessableEntity, ErrorResponse{
			Error: fmt.Sprintf("%s has no boot status; only avd: targets do", t.ID()), Code: ErrCodeNotSupported,
		})
		return
	}
	writeJSON(w, http.StatusOK, br.BootStatus(r.Context()))
}

// handleDeviceBoot is POST /api/devices/boot?target=: boot an AVD or a
// simulator without opening a stream, and answer once it is up. One already
// running answers the same 200. It is arbitrated like /api/input, and it is
// an explicit start, so it lifts a shut-down reopen guard as start=1 does.
// An AVD booted this way has no session, so the idle pause and shutdown
// count from the boot.
func (s *Server) handleDeviceBoot(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.handleDeviceBootStatus(w, r)
		return
	}
	wait, werr := parseBootWait(r.URL.Query().Get("wait"))
	if werr != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: werr.Error()})
		return
	}
	t, ok := s.agentTarget(w, r)
	if !ok {
		return
	}
	id := t.ID()
	c := s.inputCaller(r, id)
	refuse := func(status int, resp ErrorResponse) {
		s.traceInputWith(r, id, "boot", c, nil, map[string]any{"outcome": "refused", "code": resp.Code})
		writeJSON(w, status, resp)
	}
	ti, listed := s.mgr.Info(r.Context(), id)
	st, starts := t.(target.Starter)
	switch {
	case !listed:
		refuse(http.StatusUnprocessableEntity, ErrorResponse{Error: fmt.Sprintf("%s is not in the target list", id), Code: ErrCodeNotSupported})
		return
	case !starts || (!strings.HasPrefix(id, "avd:") && !strings.HasPrefix(id, "sim:")):
		refuse(http.StatusUnprocessableEntity, ErrorResponse{Error: fmt.Sprintf("%s is not a loupe AVD or simulator", id), Code: ErrCodeNotSupported})
		return
	case ti.State == "creating" || !ti.Available:
		refuse(http.StatusUnprocessableEntity, ErrorResponse{Error: notSupportedReason(ti, id, "boot"), Code: ErrCodeNotSupported})
		return
	case s.lifeInFlight(id):
		refuse(http.StatusConflict, ErrorResponse{Error: fmt.Sprintf("%s is being shut down or deleted", id), Code: ErrCodeBusy})
		return
	}
	if !s.arbitrate(w, r, id, "boot", c) {
		return
	}
	s.startTarget(true, id)

	platform := "android"
	if strings.HasPrefix(id, "sim:") {
		platform = "ios"
	}
	wasRunning := !ti.BootsOnOpen
	began := time.Now()
	// A wait can outlast the claim's lease, and the caller makes no other
	// call while it waits, so the boot holds the caller's claim until it ends.
	unhold := s.claims.hold(id, c.agent, c.session)
	if wait == 0 && !wasRunning {
		s.bootInBackground(w, r, id, st, c, began, unhold)
		return
	}
	// Not the request's context, as for a shutdown: a caller that gives up
	// must not leave a boot half done.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), max(wait, time.Second))
	defer cancel()
	err := st.Boot(ctx)
	unhold()
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	s.traceInputWith(r, id, "boot", c, err, map[string]any{
		"platform": platform, "tookMs": time.Since(began).Milliseconds(), "outcome": outcome, "wasRunning": wasRunning,
	})
	if err != nil {
		// A boot that ran out of ?wait= is not a failed boot: the emulator
		// is still tracked and the error says how to poll it.
		status := http.StatusBadGateway
		if ctx.Err() != nil {
			status = http.StatusGatewayTimeout
		}
		writeJSON(w, status, ErrorResponse{Error: err.Error()})
		return
	}
	fresh, _ := s.mgr.Info(r.Context(), id)
	writeJSON(w, http.StatusOK, fresh)
}

// bootInBackground answers 202 with the boot status and finishes the boot on
// its own goroutine, bounded by bootWaitMax. A second wait=0 for a boot
// already running starts nothing more. unhold ends the caller's claim hold
// when the boot does.
func (s *Server) bootInBackground(w http.ResponseWriter, r *http.Request, id string, st target.Starter, c inputCaller, began time.Time, unhold func()) {
	if _, running := s.bgBoots.LoadOrStore(id, true); running {
		unhold()
	} else {
		go func() {
			defer s.bgBoots.Delete(id)
			defer unhold()
			ctx, cancel := context.WithTimeout(context.Background(), bootWaitMax)
			defer cancel()
			err := st.Boot(ctx)
			outcome := "ok"
			if err != nil {
				outcome = "error"
			}
			s.traceInputWith(r, id, "boot", c, err, map[string]any{
				"platform": "android", "tookMs": time.Since(began).Milliseconds(), "outcome": outcome, "background": true,
			})
		}()
	}
	if br, ok := st.(target.BootReporter); ok {
		writeJSON(w, http.StatusAccepted, br.BootStatus(r.Context()))
		return
	}
	fresh, _ := s.mgr.Info(r.Context(), id)
	writeJSON(w, http.StatusAccepted, fresh)
}
