package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

// TestPUTOnAGETOnlyRouteIs405 is round-4 item 6: Handler enforces each
// route's methods list itself, centrally, rather than the list being a
// label nothing checks. /api/stats names only GET, so a PUT must never
// reach handleStats -- it must 405 before the handler runs at all.
//
// This is the reviewer's own mutation check: adding http.MethodPut to
// /api/stats' methods list in server.go (turning it into an exemption from
// this enforcement) makes this test fail, because handleStats has no PUT
// branch and nothing else here would catch a route silently answering a
// method its own table denies.
func TestPUTOnAGETOnlyRouteIs405(t *testing.T) {
	s := &Server{mgr: NewManager(target.Options{})}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/stats", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405: %s", rec.Code, rec.Body.String())
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
		t.Fatalf("Allow header = %q, want %q", allow, http.MethodGet)
	}
	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body = %s; want a JSON ErrorResponse: %v", rec.Body.String(), err)
	}
	if resp.Error == "" {
		t.Fatalf("ErrorResponse.Error is empty")
	}
	// round-5 item 4: pin the exact bytes, not just "some JSON ErrorResponse
	// with a non-empty Error" -- Handler's central 405 branch writes this
	// through writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error:
	// "method not allowed"}), and a change to that literal or to writeJSON's
	// encoding (e.g. losing json.Encoder's trailing newline) should fail a
	// test rather than only the looser check above.
	if got, want := rec.Body.String(), "{\"error\":\"method not allowed\"}\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// TestPUTOnAGETOnlyRouteNeverReachesTheHandler is the same request as above,
// checked a different way: handleStats itself must not run, not merely that
// the response happens to look like a 405. A handler with a stray
// unconditional 200 write, reached despite the central check, would pass
// the status-code assertion above if it also happened to overwrite the
// header -- this fails independently of that by asserting on a side effect
// only handleStats produces.
func TestPUTOnAGETOnlyRouteNeverReachesTheHandler(t *testing.T) {
	m := NewManager(target.Options{})
	s := &Server{mgr: m}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/stats", nil)
	s.Handler().ServeHTTP(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct == "text/event-stream" {
		t.Fatalf("Content-Type = %q; handleStats appears to have run despite the method mismatch", ct)
	}
}

// TestGETOnAPOSTOnlyRouteKeepsItsPlainTextBody pins the two pre-existing
// plain-text 405s (handleInput's "POST only", handleEvents' "GET only")
// byte-identical through the new centralized check: route.plainNotAllowed
// exists specifically so Handler's enforcement writes the same bytes the
// handler's own inline check always wrote, for the two 405s that predate
// ErrorResponse and are not JSON (see the endpoints comment in
// internal/apischema/schema.go).
func TestGETOnAPOSTOnlyRouteKeepsItsPlainTextBody(t *testing.T) {
	s := &Server{mgr: NewManager(target.Options{})}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/input", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405: %s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Body.String(), "POST only\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
		t.Fatalf("Allow header = %q, want %q", allow, http.MethodPost)
	}
}

// TestHEADIsAllowedWhereverGETIs checks methodAllowed's one deliberate
// exception: HEAD rides along with GET without a route having to name it
// twice, which is what handleEvents (GET-and-HEAD, per its own comment)
// relies on through Handler now that the check is centralized.
func TestHEADIsAllowedWhereverGETIs(t *testing.T) {
	s := &Server{mgr: NewManager(target.Options{})}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/api/stats", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusMethodNotAllowed {
		t.Fatalf("status = %d; HEAD should be allowed wherever GET is", rec.Code)
	}
}

// TestGETOnAGETAndPUTRouteStillReachesTheHandler is the non-mutation
// control for TestPUTOnAGETOnlyRouteIs405: a method a route's own table does
// list must still reach its handler through the same centralized check, so
// the enforcement is a real allow-list rather than a check that happens to
// reject everything.
func TestGETOnAGETAndPUTRouteStillReachesTheHandler(t *testing.T) {
	s := &Server{mgr: NewManager(target.Options{})}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusMethodNotAllowed {
		t.Fatalf("status = %d; GET /api/config is in that route's own methods list and must not 405", rec.Code)
	}
}

// TestAllowHeaderJoinsMultipleMethods pins the Allow header's exact format
// on a route naming more than one method, since TestPUTOnAGETOnlyRouteIs405
// only exercises a single-method route and "GET" alone cannot distinguish
// strings.Join(methods, ", ") from any other join separator.
func TestAllowHeaderJoinsMultipleMethods(t *testing.T) {
	s := &Server{mgr: NewManager(target.Options{})}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/config", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405: %s", rec.Code, rec.Body.String())
	}
	if allow := rec.Header().Get("Allow"); allow != "GET, PUT" {
		t.Fatalf("Allow header = %q, want %q", allow, "GET, PUT")
	}
}

// TestExtendedCONNECTReachesWebTransportHandler is the round-5 regression: a
// WebTransport session opens with an extended CONNECT
// (":protocol" webtransport), not GET -- http3.Server hands that through as
// r.Method == "CONNECT" with r.Proto set to the HTTP/3 version string. When
// /wt's methods list named only GET, 28d320b's new centralized method check
// 405'd every one of those before handleWebTransport ever ran, which is
// trap 11's symptom: wt-attempt never fires and the client hangs.
//
// s.wtServer is left nil here, which makes handleWebTransport itself answer
// 404 "webtransport not enabled" as its very first line -- a response no
// other code path in this package produces. That 404 is what proves the
// request reached the handler; a 405 proves it did not. Asserting "not 405"
// alone would also pass if some other, unrelated bug elsewhere made the
// handler 404 for its own reasons, so the body is checked too.
func TestExtendedCONNECTReachesWebTransportHandler(t *testing.T) {
	s := &Server{mgr: NewManager(target.Options{})}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodConnect, "/wt?target=synthetic", nil)
	req.Proto = "webtransport"
	s.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusMethodNotAllowed {
		t.Fatalf("status = %d; extended CONNECT to /wt must not 405 before reaching handleWebTransport", rec.Code)
	}
	if rec.Code != http.StatusNotFound || rec.Body.String() != "webtransport not enabled\n" {
		t.Fatalf("status = %d, body = %q; want handleWebTransport's own 404 not-enabled response, proving the request reached the handler", rec.Code, rec.Body.String())
	}
}

// TestGETStillReachesWebTransportHandler pins that GET was not narrowed away
// from /wt in the process of adding CONNECT: 1be73c4, before the centralized
// method check existed, accepted GET, and handleWebTransport itself never
// looked at r.Method, so nothing about fixing the CONNECT gap should have
// changed what GET does.
func TestGETStillReachesWebTransportHandler(t *testing.T) {
	s := &Server{mgr: NewManager(target.Options{})}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/wt?target=synthetic", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusMethodNotAllowed {
		t.Fatalf("status = %d; GET /wt must still reach handleWebTransport", rec.Code)
	}
	if rec.Code != http.StatusNotFound || rec.Body.String() != "webtransport not enabled\n" {
		t.Fatalf("status = %d, body = %q; want handleWebTransport's own 404 not-enabled response", rec.Code, rec.Body.String())
	}
}
