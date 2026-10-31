package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bmcreations/loupe/internal/awake"
	"github.com/bmcreations/loupe/internal/clipshare"
	"github.com/bmcreations/loupe/internal/pairing"
	"github.com/bmcreations/loupe/internal/peer"
	"github.com/bmcreations/loupe/internal/record"
	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/t3"
	"github.com/bmcreations/loupe/internal/trace"
	"github.com/bmcreations/loupe/internal/wsx"
	"github.com/quic-go/webtransport-go"
)

// Server wires the manager to HTTP routes and serves the browser client.
type Server struct {
	mgr   *Manager
	web   fs.FS
	trace *trace.Writer
	// claims holds agents' claims and the hands-off set (claims.go).
	claims *claimBook
	// bgBoots holds the ids of AVDs with a wait=0 boot running.
	bgBoots sync.Map
	// exts is the registry /api/ext/ passes through to (extension.go).
	exts *extensionRegistry
	// flags holds what people flagged for agents (flags.go).
	flags *flagStore
	// findT3 locates the T3 CLI; nil uses t3.Find. Tests swap in a fake.
	findT3 func() (t3.CLI, error)
	// flagDir is where flags sent to T3 are written; empty is the user
	// cache dir's loupe/flags.
	flagDir string
	// destPath is destinations.json; empty offers no commands or webhooks.
	destPath string
	t3cache  t3Cache

	// handoffURL is the link /api/handoff builds on (handoff.go).
	handoffURL string

	// indexHTML is the client page with its script reference stamped with a
	// content hash, and build is that hash.
	indexHTML []byte
	appJS     []byte
	build     string

	// token is the shared secret every request must present. Empty disables
	// the check entirely, which is what -no-auth selects.
	token string

	// pairing holds device sessions and pending pairing codes (pairing.go).
	// Nil when sessions.json could not be read: only the master token works
	// then, and the pairing routes answer 404.
	pairing *pairing.Store
	// pairLimit throttles failed POST /api/pair attempts.
	pairLimit *pairLimiter
	// cookieSet is when each browser session's cookie was last sent, so its
	// Max-Age is refreshed once a day rather than on every response.
	cookieMu  sync.Mutex
	cookieSet map[string]time.Time

	// cfg is the persisted settings store. Nil leaves /api/config answering
	// 404, which is what a build with no config file selects.
	cfg *ConfigStore

	// avds creates AVDs through the android CLI. Nil when the CLI is not
	// installed, and /api/avds then answers 404.
	avds *AVDCreator
	// devs creates AVDs through avdmanager and simulators through simctl
	// from what is installed. Nil when neither can, and /api/devices then
	// answers 404.
	devs *DeviceCreator
	// slot is the one creation in flight, shared by avds and devs. Nil until
	// either is set, which leaves withCreation a no-op.
	slot *createSlot

	// wtServer upgrades WebTransport sessions. Nil when HTTP/3 is not running,
	// in which case the client falls back to the WebSocket transport.
	wtServer *webtransport.Server

	// awake keeps the Mac from idle-sleeping while a viewer is attached or an
	// agent has driven a device recently. Nil keeps nothing awake.
	awake *awake.Keeper

	// aapt2 reads an uploaded APK's package name. Empty when the SDK has no
	// build-tools, which leaves ?package= as the only source.
	aapt2 string

	// peers is the registry of servers this one merges targets from. Nil
	// answers /api/servers 404.
	peers *peer.Registry
	// peerSync serializes syncPeers: two peers' pollers finish at once, and
	// the older merge must not be the one applied last.
	peerSync sync.Mutex

	// clipShare is the shared clipboard /api/clipboard/shared serves and
	// the peer bridges carry. Nil answers that route 404.
	clipShare *clipshare.Service
	// bridgeMu guards bridges, one cancel per peer bridged
	// (clipshare_bridge.go).
	bridgeMu sync.Mutex
	bridges  map[string]context.CancelFunc

	// recordings holds /api/record's recordings. Nil answers 501.
	recordings *record.Store

	// ffmpeg finds the binary a contact sheet runs. Nil is record.FFmpeg;
	// tests replace it to stand for a Mac without ffmpeg.
	ffmpeg func() (string, error)
}

// SetAAPT2 gives /api/install the aapt2 binary it reads package names with.
// Empty leaves the name to ?package=.
func (s *Server) SetAAPT2(path string) { s.aapt2 = path }

// KeepAwake attaches the keeper that holds off idle sleep while loupe is in use.
func (s *Server) KeepAwake(k *awake.Keeper) { s.awake = k }

// EnableWebTransport attaches the HTTP/3 server that upgrades WebTransport
// sessions. Without it the /wt endpoint reports 404 and clients use WebSocket.
func (s *Server) EnableWebTransport(wt *webtransport.Server) { s.wtServer = wt }

// NewServer builds the HTTP handler set. web is the filesystem holding the
// browser client, normally an embedded one. tracer may be nil.
func NewServer(mgr *Manager, web fs.FS, tracer *trace.Writer) *Server {
	s := &Server{mgr: mgr, web: web, trace: tracer}
	s.claims = newClaimBook(tracer.Record)
	s.exts = newExtensionRegistry()
	s.flags = newFlagStore()
	mgr.SetTrace(tracer)
	mgr.SetClaimed(s.claims.held)
	s.build, s.indexHTML, s.appJS = stampIndex(web)
	return s
}

// stampIndex computes a content hash over the client assets and rewrites the
// page's script reference to include it.
//
// no-store alone only protects future loads: a browser that already cached
// /app.js under that exact URL will keep serving it, and on iOS there is no
// convenient way to force otherwise. Making the URL change whenever the content
// changes means a stale copy can never be the one that loads, because the URL
// the new page asks for was never cached under the old build.
// clientModules is every module app.js imports. It is one list because it is
// read twice -- once into the build hash, once to stamp the import specifiers
// -- and a module missing from either is reachable at an unversioned URL,
// which is trap 7. TestEveryClientModuleIsStamped keeps it honest against
// app.js itself.
//
// Stamping gap (review round 5, finding 5): the rewrite loop below only
// touches app.js's own body. toast.js is not imported by app.js at all --
// corners.js, appcard.js, devicesettings.js and inspect.js each import it
// directly, and so do keyboard.js, clipboard.js, streamconfig.js, record.js, drop.js and flag.js -- so its specifier inside THOSE files is never rewritten, and a
// browser fetches it at the bare, unversioned /toast.js. That is only safe
// because toast.js is stateless and it is the only module in this shape
// today. If app.js ever adds `import './toast.js'` of its own, this loop
// rewrites that one specifier to a stamped URL while the other four files
// keep asking for the unstamped one, and the two URLs resolve to two
// separate module instances in the same page. Fixing that means rewriting
// every embedded module's own imports, not just app.js's -- out of scope
// here; TestEveryClientModuleIsStamped only checks that this list is
// complete, not that every file naming a listed module gets stamped.
var clientModules = []string{"jitter.js", "transport.js", "pane.js", "panes.js", "paneheader.js", "orientation.js", "layout.js", "sheet.js", "handedness.js", "appearance.js", "health.js", "pill.js", "drawer.js", "targets.js", "corners.js", "fold.js", "appcard.js", "devicesettings.js", "inspect.js", "toast.js", "eventlog.js", "livecheck.js", "immersive.js", "doubletap.js", "keyboard.js", "clipboard.js", "touches.js", "control.js", "streamconfig.js", "connstats.js", "states.js", "newdevice.js", "helpsheets.js", "record.js", "pip.js", "peek.js", "drop.js", "rotate.js", "panels.js", "cursor.js", "servers.js", "extoverlay.js", "zoom.js", "flag.js"}

func stampIndex(web fs.FS) (build string, page, app []byte) {
	h := sha256.New()
	for _, name := range append([]string{"app.js", "style.css"}, clientModules...) {
		b, err := fs.ReadFile(web, name)
		if err != nil {
			continue
		}
		_, _ = h.Write(b)
	}
	build = hex.EncodeToString(h.Sum(nil))[:10]

	page, err := fs.ReadFile(web, "index.html")
	if err != nil {
		return build, nil, nil
	}
	page = bytes.ReplaceAll(page, []byte(`src="/app.js"`), []byte(`src="/app.js?b=`+build+`"`))
	page = bytes.ReplaceAll(page, []byte(`href="/style.css"`), []byte(`href="/style.css?b=`+build+`"`))

	// The module's own imports need stamping too. Stamping only the entry
	// point leaves its dependencies reachable at an unversioned URL, which is
	// exactly how a stale copy gets loaded alongside a fresh one.
	app, err = fs.ReadFile(web, "app.js")
	if err != nil {
		return build, page, nil
	}
	for _, dep := range clientModules {
		app = bytes.ReplaceAll(app,
			[]byte(`'./`+dep+`'`),
			[]byte(`'./`+dep+`?b=`+build+`'`))
	}
	return build, page, app
}

// route is one entry in the table Handler registers from. Keeping the table
// as a value, rather than a sequence of inline mux.HandleFunc calls, is what
// lets Routes and Handler share one list instead of two that could silently
// drift apart -- Routes is what apischema's route-coverage test walks (see
// TestEveryJSONRouteHasASchemaEntry), and a path added only to one of them
// would either serve traffic no schema describes or describe an endpoint
// that answers 404.
//
// methods names every HTTP method this route's handler actually answers, so
// coverage can be checked per method+path rather than per path alone -- a
// path registers one handler regardless of method (net/http's mux routes on
// the path only), but /api/config and /api/settings each read on GET and
// write on PUT with two different response shapes, and a route-coverage
// check keyed on path alone cannot tell "GET has a schema entry" from "PUT
// does". Handler enforces it directly, below, so this is not just a label:
// TestPUTOnAGETOnlyRouteIs405 (server_test.go) checks that adding a method
// here without a matching handler branch fails, by asserting a method this
// list does not name is rejected.
//
// plainNotAllowed, when set, is the plain-text body Handler writes for a
// disallowed method instead of the ordinary JSON ErrorResponse. It exists
// for the two 405s that predate ErrorResponse and apischema's route
// coverage (see that package's endpoints comment): handleInput's own
// "POST only" and handleEvents' own "GET only". Both handlers still carry
// their own inline method check too, reachable only when a test calls them
// directly rather than through Handler -- see agent_test.go and
// events_test.go -- and this field is what keeps Handler's centralized
// check from writing different bytes than that inline check would have.
type route struct {
	path            string
	methods         []string
	handler         func(*Server, http.ResponseWriter, *http.Request)
	plainNotAllowed string
}

var routes = []route{
	{"/api/targets", []string{http.MethodGet}, (*Server).handleTargets, ""},
	{"/api/servers", []string{http.MethodGet, http.MethodPost, http.MethodDelete}, (*Server).handleServers, ""},
	{"/api/log", []string{http.MethodPost}, (*Server).handleClientLog, ""},
	{"/api/stats", []string{http.MethodGet}, (*Server).handleStats, ""},
	{"/healthz", []string{http.MethodGet}, (*Server).handleHealth, ""},
	{"/ws", []string{http.MethodGet}, (*Server).handleWS, ""},
	// A WebTransport session opens with an extended CONNECT
	// (RFC 9220/draft webtransport-http3, ":protocol" webtransport), not
	// GET -- http3.Server hands that through as r.Method == "CONNECT". GET
	// stays too: 1be73c4 (before the central method check existed)
	// accepted it, and handleWebTransport itself only ever looked at
	// r.Header/query, never r.Method, so nothing here narrows what already
	// worked. Omitting CONNECT is what silently broke every WebTransport
	// session (round-5 review of 28d320b): the central check now rejects
	// it with a 405 before handleWebTransport, and no test previously drove
	// a CONNECT request through Handler to notice.
	{"/wt", []string{http.MethodGet, http.MethodConnect}, (*Server).handleWebTransport, ""},
	{"/api/ui", []string{http.MethodGet}, (*Server).handleUI, ""},
	{"/api/input", []string{http.MethodPost}, (*Server).handleInput, "POST only"},
	{"/api/sequence", []string{http.MethodPost}, (*Server).handleSequence, "POST only"},
	{"/api/screenshot", []string{http.MethodGet}, (*Server).handleScreenshot, ""},
	{"/api/record", []string{http.MethodGet, http.MethodPost}, (*Server).handleRecord, ""},
	{"/api/record/", []string{http.MethodGet, http.MethodPost, http.MethodDelete}, (*Server).handleRecordItem, ""},
	{"/api/install", []string{http.MethodPost}, (*Server).handleInstall, ""},
	{"/api/media", []string{http.MethodPost}, (*Server).handleMedia, ""},
	{"/api/drop", []string{http.MethodPost}, (*Server).handleDrop, ""},
	{"/api/camera", []string{http.MethodPost, http.MethodDelete}, (*Server).handleCamera, ""},
	{"/api/logcat", []string{http.MethodGet}, (*Server).handleLogcat, ""},
	{"/api/crashes", []string{http.MethodGet}, (*Server).handleCrashes, ""},
	{"/api/crashes/", []string{http.MethodGet}, (*Server).handleCrash, ""},
	{"/api/frame", []string{http.MethodGet}, (*Server).handleFrame, ""},
	{"/api/handoff", []string{http.MethodGet}, (*Server).handleHandoff, ""},
	{"/api/overlays", []string{http.MethodGet, http.MethodPost, http.MethodDelete}, (*Server).handleOverlays, ""},
	{"/api/extensions", []string{http.MethodGet, http.MethodPost, http.MethodDelete}, (*Server).handleExtensions, ""},
	{"/api/ext/", []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete}, (*Server).handleExtensionProxy, ""},
	{"/api/overlays/image", []string{http.MethodGet}, (*Server).handleOverlayImage, ""},
	{"/api/frame/asset", []string{http.MethodGet}, (*Server).handleFrameAsset, ""},
	{"/api/config", []string{http.MethodGet, http.MethodPut}, (*Server).handleConfig, ""},
	{"/api/events", []string{http.MethodGet}, (*Server).handleEvents, "GET only"},
	{"/api/app", []string{http.MethodGet}, (*Server).handleApp, ""},
	{"/api/settings", []string{http.MethodGet, http.MethodPut}, (*Server).handleSettings, ""},
	{"/api/clipboard", []string{http.MethodGet, http.MethodPost}, (*Server).handleClipboard, ""},
	{"/api/clipboard/shared", []string{http.MethodGet, http.MethodPost}, (*Server).handleSharedClipboard, ""},
	{"/api/flags", []string{http.MethodGet, http.MethodPost}, (*Server).handleFlags, ""},
	{"/api/flags/end", []string{http.MethodPost}, (*Server).handleFlagEnd, ""},
	{"/api/flags/listeners", []string{http.MethodGet}, (*Server).handleFlagListeners, ""},
	{"/api/flags/destinations", []string{http.MethodGet}, (*Server).handleFlagDestinations, ""},
	{"/api/flags/shot", []string{http.MethodGet}, (*Server).handleFlagShot, ""},
	{"/api/avds", []string{http.MethodPost}, (*Server).handleCreateAVD, ""},
	{"/api/avds/profiles", []string{http.MethodGet}, (*Server).handleAVDProfiles, ""},
	{"/api/devices/options", []string{http.MethodGet}, (*Server).handleDeviceOptions, ""},
	{"/api/devices/shutdown", []string{http.MethodPost}, (*Server).handleDeviceShutdown, ""},
	{"/api/devices/boot", []string{http.MethodGet, http.MethodPost}, (*Server).handleDeviceBoot, ""},
	{"/api/devices/reset", []string{http.MethodPost}, (*Server).handleDeviceReset, ""},
	{"/api/devices", []string{http.MethodPost, http.MethodDelete}, (*Server).handleDevices, ""},
	{"/api/devices/handsoff", []string{http.MethodPost}, (*Server).handleHandsOff, ""},
	{"/api/claims", []string{http.MethodGet, http.MethodPost, http.MethodDelete}, (*Server).handleClaims, ""},
	{"/api/auth", []string{http.MethodGet}, (*Server).handleAuth, ""},
	{"/api/pair", []string{http.MethodPost}, (*Server).handlePair, ""},
	{"/api/pairings", []string{http.MethodGet, http.MethodPost}, (*Server).handlePairings, ""},
	{"/api/pairings/", []string{http.MethodDelete}, (*Server).handlePairingItem, ""},
	{"/api/sessions", []string{http.MethodGet, http.MethodDelete}, (*Server).handleSessions, ""},
	{"/api/sessions/", []string{http.MethodDelete}, (*Server).handleSessionItem, ""},
}

// Route is one method+path pair Handler actually serves.
type Route struct {
	Method string
	Path   string
}

// Routes returns every method+path pair Handler registers, in registration
// order, excluding "/" -- the catch-all that serves the static client
// bundle, not a JSON endpoint. It exists so a route-coverage test outside
// this package (apischema, which already imports httpapi for its response
// types) can walk the same table Handler itself registers from, keyed the
// same way its own endpoint entries are: by method and path together, not
// path alone.
func Routes() []Route {
	var out []Route
	for _, r := range routes {
		for _, m := range r.methods {
			out = append(out, Route{Method: m, Path: r.path})
		}
	}
	return out
}

// Handler returns the configured mux.
//
// Every route's methods list is enforced here, centrally, before its handler
// ever runs -- net/http's ServeMux routes on the path alone, so without this
// a route table entry naming only GET would still answer a PUT with whatever
// the handler happened to do (nothing, for most of them: several never
// checked r.Method at all). Adding a method to a route's list without a
// handler branch for it is therefore not silently permissive.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, r := range routes {
		r := r
		mux.HandleFunc(r.path, func(w http.ResponseWriter, req *http.Request) {
			if !methodAllowed(r.methods, req.Method) {
				w.Header().Set("Allow", strings.Join(r.methods, ", "))
				if r.plainNotAllowed != "" {
					http.Error(w, r.plainNotAllowed, http.StatusMethodNotAllowed)
				} else {
					writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
				}
				return
			}
			r.handler(s, w, req)
		})
	}
	mux.HandleFunc("/", s.handleRoot)
	return s.authed(mux)
}

// methodAllowed reports whether method is one methods names, or HEAD
// whenever GET is -- the one method net/http conventionally serves
// alongside GET without a route needing to say so twice, and what
// handleEvents relied on (GET and HEAD both) before this check existed.
func methodAllowed(methods []string, method string) bool {
	for _, m := range methods {
		if m == method {
			return true
		}
		if method == http.MethodHead && m == http.MethodGet {
			return true
		}
	}
	return false
}

// statusRecorder captures the response status so requests can be traced.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusRecorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// handleRoot serves the stamped client page, and everything else from the
// embedded filesystem.
//
// Every request is traced. A client that fails before it opens its WebSocket
// produces no metrics at all, so without this there is no way to tell a page
// that never loaded from one that loaded and then died, and no way to see
// which asset a browser actually asked for.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w}
	setNoStore(rec)

	switch {
	case s.indexHTML != nil && (r.URL.Path == "/" || r.URL.Path == "/index.html"):
		rec.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = rec.Write(s.indexHTML)
	case r.URL.Path == manifestPath:
		s.handleManifest(rec, r)
	case s.appJS != nil && r.URL.Path == "/app.js":
		rec.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = rec.Write(s.appJS)
	default:
		http.FileServer(http.FS(s.web)).ServeHTTP(rec, r)
	}

	s.trace.Record(map[string]any{
		"kind":        "http",
		"path":        r.URL.Path,
		"query":       redactQuery(r.URL.RawQuery),
		"status":      rec.status,
		"bytes":       rec.bytes,
		"contentType": rec.Header().Get("Content-Type"),
		"ua":          r.UserAgent(),
		"proto":       r.Proto,
	})
}

func (s *Server) handleTargets(w http.ResponseWriter, r *http.Request) {
	// ?local=1 is how a hub polls this server: only its own rows, so two
	// servers that peer with each other do not list each other's rows back.
	s.mgr.RescanAVDs()
	rows := s.mgr.Targets
	if r.URL.Query().Get("local") == "1" {
		rows = s.mgr.LocalTargets
	}
	targets := s.slot.withCreation(rows(r.Context()))
	s.claims.decorate(targets)
	ids := make([]string, 0, len(targets))
	for _, t := range targets {
		ids = append(ids, t.ID)
	}
	// Traced because its absence is otherwise indistinguishable from a client
	// that never got far enough to ask. A page that loads every asset and then
	// shows no target is either a client that never ran or a server with an
	// empty list, and only this record separates the two.
	s.trace.Record(map[string]any{
		"kind": "http", "path": "/api/targets", "status": 200,
		"targets": ids, "proto": r.Proto, "ua": r.UserAgent(),
	})
	writeJSON(w, http.StatusOK, targets)
}

// handleClientLog records what the browser reports about itself.
//
// Everything before the first WebSocket frame is invisible from the server: a
// module that throws during evaluation, a missing capability, a transport that
// fails to open. The client beacons those here so a failed load leaves a record
// instead of silence.
func (s *Server) handleClientLog(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var rec map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&rec); err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if rec == nil {
		rec = map[string]any{}
	}
	// A client record carries location.href, which holds the token whenever
	// the page was opened from a token link the strip redirect didn't see,
	// and a pairing link's code in its fragment.
	for k, v := range rec {
		if str, ok := v.(string); ok && holdsSecretParam(str) {
			rec[k] = redactURL(str)
		}
	}
	rec["kind"] = "client"
	rec["ua"] = r.UserAgent()
	s.trace.Record(rec)
	w.WriteHeader(http.StatusNoContent)
}

// handleStats exposes the live counters for both sides of every running
// session, so the stream can be diagnosed without reading numbers off a phone.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.AllStats())
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, OKResponse{OK: true})
}

// handleWS upgrades to a WebSocket and runs one viewer for the requested
// target until the client disconnects or the session ends.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	targetID := r.URL.Query().Get("target")
	if targetID == "" {
		http.Error(w, "missing ?target=", http.StatusBadRequest)
		return
	}
	// Refused before acquiring, so a client the server cannot serve never
	// boots the target. Upgraded first, as for a shut-down target, so the
	// client reads the reason rather than a bare status code.
	if _, err := session.NegotiateProtocol(r.URL.Query().Get("proto")); err != nil {
		s.trace.Record(map[string]any{"kind": "ws-error", "stage": "protocol", "target": targetID, "err": err.Error()})
		if conn, uerr := wsx.Upgrade(w, r); uerr == nil {
			_ = wsConn{c: conn}.SendControl(session.EndedMessage(session.EndedUnsupportedProtocol))
			_ = conn.Close()
		}
		return
	}

	attempt := map[string]any{"kind": "ws-attempt", "target": targetID, "ua": r.UserAgent()}
	if s.explicitStart(r, targetID) {
		attempt["start"] = true
	}
	s.trace.Record(attempt)

	sess, err := s.mgr.Acquire(r.Context(), targetID)
	if err != nil {
		s.trace.Record(map[string]any{"kind": "ws-error", "stage": "acquire", "err": err.Error()})
		if errors.Is(err, ErrJustShutDown) {
			// A reconnect hears what the original close said: upgrade, send
			// MsgEnded, and close, rather than a bare 502.
			if conn, uerr := wsx.Upgrade(w, r); uerr == nil {
				_ = wsConn{c: conn}.SendControl(session.EndedMessage(endedReason(err)))
				_ = conn.Close()
				return
			}
		}
		// Report the failure over HTTP rather than upgrading and closing, so
		// the browser surfaces a real status code instead of a bare
		// "connection closed".
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	conn, err := wsx.Upgrade(w, r)
	if err != nil {
		s.trace.Record(map[string]any{"kind": "ws-error", "stage": "upgrade", "err": err.Error()})
		s.mgr.Release(targetID)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.recordPath("websocket", targetID, r.RemoteAddr)
	s.serveViewer(targetID, sess, wsConn{c: conn})
}

// dispatch routes one decoded client message. Input errors are reported but
// never fatal: a device that rejects one tap should not drop the session.
//
// viewerID identifies which viewer sent payload, so a touch, key or scroll
// can be attributed to it in the event log -- see session.ViewerTouch and its
// siblings. /api/input is a second, viewerless writer into the same session;
// it publishes its own events directly rather than through dispatch.
func (s *Server) dispatch(conn Conn, sess *session.Session, viewerID uint64, payload []byte) error {
	msgType, body := payload[0], payload[1:]
	switch msgType {
	case session.MsgTouch:
		e, err := session.DecodeTouch(body)
		if err != nil {
			return err
		}
		return sess.ViewerTouch(viewerID, e)
	case session.MsgKey:
		e, err := session.DecodeKey(body)
		if err != nil {
			return err
		}
		return sess.ViewerKey(viewerID, e)
	case session.MsgText:
		text, err := session.DecodeText(body)
		if err != nil {
			return err
		}
		return sess.ViewerText(viewerID, text)
	case session.MsgTakeOver:
		return sess.ViewerTakeOver(viewerID)
	case session.MsgScroll:
		e, err := session.DecodeScroll(body)
		if err != nil {
			return err
		}
		return sess.ViewerScroll(viewerID, e)
	case session.MsgClientStats:
		var raw map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			return err
		}
		sess.RecordClientStats(raw)
		return nil
	case session.MsgHello:
		// Its only job was to exist. See MsgHello.
		return nil
	case session.MsgPing:
		clientMicros, err := session.DecodePing(body)
		if err != nil {
			return err
		}
		return conn.SendControl(session.EncodePong(clientMicros, uint64(time.Now().UnixMicro())))
	default:
		return fmt.Errorf("unknown client message type 0x%02x", msgType)
	}
}

// noStore stops browsers caching the client.
//
// This is not a nicety. Files served from an embed.FS carry a zero modtime, so
// net/http sends no Last-Modified and there is nothing for the browser to
// revalidate against. Safari will then reuse a cached copy indefinitely, and
// every reload silently runs the previous build. That turns a fixed bug into a
// bug that appears unfixed, which is far more expensive than the bytes saved.
//
// The client is a few tens of kilobytes on a link that is already carrying
// video, so caching it buys nothing worth this failure mode.
func setNoStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}

// Build returns the content hash of the client assets.
func (s *Server) Build() string { return s.build }

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
