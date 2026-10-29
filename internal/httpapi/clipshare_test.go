package httpapi

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bmcreations/loupe/internal/clipshare"
	"github.com/bmcreations/loupe/internal/peer"
	"github.com/bmcreations/loupe/internal/target"
)

func sharedServer(t *testing.T, on bool) (*Server, *clipshare.Service) {
	t.Helper()
	share := clipshare.New()
	share.SetEnabled(on)
	s := &Server{mgr: NewManager(target.Options{}), claims: newClaimBook(nil)}
	s.SetClipShare(share)
	return s, share
}

func sharedDo(s *Server, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}

func TestSharedGetImmediate(t *testing.T) {
	s, share := sharedServer(t, true)
	pub, _ := share.Publish(clipshare.Value{Text: "a", Origin: "mac"})
	w := sharedDo(s, http.MethodGet, "/api/clipboard/shared", "")
	var v clipshare.Value
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &v) != nil {
		t.Fatalf("get = %d %s", w.Code, w.Body)
	}
	if v.ID != pub.ID || v.Text != "a" || v.Seq != 1 || v.Origin != "mac" {
		t.Fatalf("got %+v, want %+v", v, pub)
	}
}

func TestSharedGetTimeout204(t *testing.T) {
	s, share := sharedServer(t, true)
	share.Publish(clipshare.Value{Text: "a", Origin: "mac"})
	start := time.Now()
	w := sharedDo(s, http.MethodGet, "/api/clipboard/shared?after=1&waitMs=50", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("get = %d %s, want 204", w.Code, w.Body)
	}
	if d := time.Since(start); d < 40*time.Millisecond || d > 2*time.Second {
		t.Fatalf("held %v, want about 50ms", d)
	}
}

func TestSharedGetWakesOnPublish(t *testing.T) {
	s, share := sharedServer(t, true)
	got := make(chan *httptest.ResponseRecorder, 1)
	go func() { got <- sharedDo(s, http.MethodGet, "/api/clipboard/shared?after=0&waitMs=5000", "") }()
	time.Sleep(30 * time.Millisecond)
	share.Publish(clipshare.Value{Text: "late", Origin: "mac"})
	select {
	case w := <-got:
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"late"`) {
			t.Fatalf("get = %d %s", w.Code, w.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("long-poll not woken by a publish")
	}
}

func TestSharedDisabled409(t *testing.T) {
	s, share := sharedServer(t, false)
	for _, w := range []*httptest.ResponseRecorder{
		sharedDo(s, http.MethodGet, "/api/clipboard/shared", ""),
		sharedDo(s, http.MethodPost, "/api/clipboard/shared", `{"text":"a"}`),
	} {
		if w.Code != http.StatusConflict || errBody(t, w).Code != ErrCodeSharedClipboardOff {
			t.Fatalf("off = %d %s, want 409 %s", w.Code, w.Body, ErrCodeSharedClipboardOff)
		}
	}

	// A long-poll already waiting when sharing goes off answers 409 too.
	share.SetEnabled(true)
	got := make(chan *httptest.ResponseRecorder, 1)
	go func() { got <- sharedDo(s, http.MethodGet, "/api/clipboard/shared?waitMs=5000", "") }()
	time.Sleep(30 * time.Millisecond)
	share.SetEnabled(false)
	select {
	case w := <-got:
		if w.Code != http.StatusConflict {
			t.Fatalf("waiting get = %d %s, want 409", w.Code, w.Body)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting long-poll not ended by disable")
	}
}

func TestSharedWithoutServiceIs404(t *testing.T) {
	s := &Server{mgr: NewManager(target.Options{}), claims: newClaimBook(nil)}
	if w := sharedDo(s, http.MethodGet, "/api/clipboard/shared", ""); w.Code != http.StatusNotFound {
		t.Fatalf("get = %d, want 404", w.Code)
	}
}

func TestSharedPostTooLarge413(t *testing.T) {
	s, share := sharedServer(t, true)
	body, _ := json.Marshal(SharedClipboardRequest{Text: strings.Repeat("x", maxClipboardBytes+1)})
	if w := sharedDo(s, http.MethodPost, "/api/clipboard/shared", string(body)); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("post = %d %s, want 413", w.Code, w.Body)
	}
	if _, ok := share.Current(); ok {
		t.Fatal("oversized text was published")
	}
}

func TestSharedPostNotUTF8400(t *testing.T) {
	s, share := sharedServer(t, true)
	// encoding/json turns invalid bytes into U+FFFD before the handler's
	// own check sees them; either way no invalid text is stored.
	sharedDo(s, http.MethodPost, "/api/clipboard/shared", "{\"text\":\"\xff\"}")
	if v, ok := share.Current(); ok && !utf8.ValidString(v.Text) {
		t.Fatalf("stored invalid UTF-8 %q", v.Text)
	}
	if w := sharedDo(s, http.MethodPost, "/api/clipboard/shared", `{"text":""}`); w.Code != http.StatusBadRequest {
		t.Fatalf("empty post = %d %s, want 400", w.Code, w.Body)
	}
}

func TestSharedPostOriginForced(t *testing.T) {
	s, share := sharedServer(t, true)
	w := sharedDo(s, http.MethodPost, "/api/clipboard/shared", `{"text":"a","origin":"peer:x","id":"z-1","at":1000}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("post = %d %s", w.Code, w.Body)
	}
	v, _ := share.Current()
	if v.Origin != "participant" || v.ID != "z-1" || v.At == 1000 {
		t.Fatalf("stored %+v, want participant with the sent id and this server's At", v)
	}
}

// The per-target route sets only its target: it never publishes.
func TestPerTargetClipboardDoesNotPublish(t *testing.T) {
	m, share, _ := clipManager(t, true, "a")
	s := &Server{mgr: m, claims: newClaimBook(nil)}
	s.SetClipShare(share)
	w := sharedDo(s, http.MethodPost, "/api/clipboard?target=a", `{"text":"only a"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("post = %d %s", w.Code, w.Body)
	}
	if v, ok := share.Current(); ok {
		t.Fatalf("per-target set published %+v", v)
	}
}

// bridgeTimings shortens the bridge's waits for every test in the package.
// They are set once and never restored: a bridge goroutine still winding
// down after its test would race a restore.
var bridgeTimings sync.Once

// clipNode is a server with a shared clipboard, a peer registry and a TLS
// listener, so two of them can peer with each other.
type clipNode struct {
	*Server
	share *clipshare.Service
	url   string
	gets  atomic.Int64
	posts atomic.Int64
}

func newClipNode(t *testing.T, on bool) *clipNode {
	t.Helper()
	bridgeTimings.Do(func() {
		bridgeWait = 200 * time.Millisecond
		bridgeRetry = 50 * time.Millisecond
		bridgeOffRetry = 300 * time.Millisecond
		bridgeOldRetry = 300 * time.Millisecond
	})
	restore := peer.SetTimings(50*time.Millisecond, 10*time.Second, time.Minute)
	t.Cleanup(restore)

	n := &clipNode{share: clipshare.New()}
	n.share.SetEnabled(on)
	n.Server = &Server{mgr: NewManager(target.Options{}), claims: newClaimBook(nil), token: "tok"}
	n.SetClipShare(n.share)
	reg, err := peer.NewRegistry(peer.NewStore(filepath.Join(t.TempDir(), "servers.json")), nil)
	if err != nil {
		t.Fatal(err)
	}
	n.SetPeers(reg)
	reg.SetProbe(nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reg.Start(ctx)
	t.Cleanup(n.stopClipBridges)

	h := n.Handler()
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/clipboard/shared" {
			if r.Method == http.MethodGet {
				n.gets.Add(1)
			} else {
				n.posts.Add(1)
			}
		}
		h.ServeHTTP(w, r)
	}))
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{testCert(t)}}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	n.url = ts.URL
	return n
}

func (n *clipNode) do(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	n.Handler().ServeHTTP(w, req)
	return w
}

// peerWith adds other as a peer named name, confirming its certificate as
// a person would.
func (n *clipNode) peerWith(t *testing.T, name string, other *clipNode) {
	t.Helper()
	req := ServerAddRequest{Name: name, URL: other.url, Token: "tok"}
	b, _ := json.Marshal(req)
	w := n.do(http.MethodPost, "/api/servers", string(b))
	var er ErrorResponse
	if w.Code == http.StatusConflict && json.Unmarshal(w.Body.Bytes(), &er) == nil && er.Code == ErrCodeConfirmFingerprint {
		req.Fingerprint = er.Fingerprint
		b, _ = json.Marshal(req)
		w = n.do(http.MethodPost, "/api/servers", string(b))
	}
	if w.Code != http.StatusCreated {
		t.Fatalf("add %s = %d %s", name, w.Code, w.Body)
	}
}

func (n *clipNode) bridged(name string) bool {
	n.bridgeMu.Lock()
	defer n.bridgeMu.Unlock()
	_, ok := n.bridges[name]
	return ok
}

func TestBridgePullsAndPushes(t *testing.T) {
	a, b := newClipNode(t, true), newClipNode(t, true)
	a.peerWith(t, "b", b)

	// b's copy reaches a under b's id.
	fromB, _ := b.share.Publish(clipshare.Value{Text: "from b", Origin: "mac"})
	eventually(t, "a pulls b's value", func() bool {
		v, _ := a.share.Current()
		return v.ID == fromB.ID && v.Origin == "peer:b"
	})

	// a's copy reaches b under a's id, as a participant's.
	fromA, _ := a.share.Publish(clipshare.Value{Text: "from a", Origin: "mac"})
	eventually(t, "a pushes to b", func() bool {
		v, _ := b.share.Current()
		return v.ID == fromA.ID && v.Origin == "participant"
	})
	// b's value coming back to a is not a new one.
	time.Sleep(300 * time.Millisecond)
	if v, _ := a.share.Current(); v.ID != fromA.ID || v.Seq != 2 {
		t.Fatalf("a = %+v, want %s still at seq 2", v, fromA.ID)
	}
}

// Two servers that each hold the other as a peer settle on one copy rather
// than trading it.
func TestBridgeMutualPeersSettle(t *testing.T) {
	a, b := newClipNode(t, true), newClipNode(t, true)
	a.peerWith(t, "b", b)
	b.peerWith(t, "a", a)
	eventually(t, "both bridged", func() bool { return a.bridged("b") && b.bridged("a") })

	pub, _ := a.share.Publish(clipshare.Value{Text: "once", Origin: "mac"})
	eventually(t, "b has a's copy", func() bool { v, _ := b.share.Current(); return v.ID == pub.ID })
	time.Sleep(500 * time.Millisecond)
	va, _ := a.share.Current()
	vb, _ := b.share.Current()
	if va.Seq != 1 || vb.Seq != 1 || va.ID != pub.ID || vb.ID != pub.ID {
		t.Fatalf("a = %+v, b = %+v; want both on %s at seq 1", va, vb, pub.ID)
	}
}

// A peer whose clock is far behind still has its newer copy taken: values
// order by arrival, not by the sender's At.
func TestBridgeSkewedPeerNotDropped(t *testing.T) {
	a, b := newClipNode(t, true), newClipNode(t, true)
	a.peerWith(t, "b", b)
	first, _ := a.share.Publish(clipshare.Value{Text: "first", Origin: "mac"})
	eventually(t, "b has first", func() bool { v, _ := b.share.Current(); return v.ID == first.ID })
	if w := b.do(http.MethodPost, "/api/clipboard/shared", `{"text":"newer","id":"b-skew","at":1}`); w.Code != http.StatusNoContent {
		t.Fatalf("post = %d %s", w.Code, w.Body)
	}
	eventually(t, "a takes the newer copy", func() bool { v, _ := a.share.Current(); return v.ID == "b-skew" })
}

// After a failed exchange the hub posts only what it accepted since the
// peer last answered.
func TestNeedsCatchUp(t *testing.T) {
	v := clipshare.Value{Seq: 3, Origin: "mac"}
	for _, c := range []struct {
		name     string
		origin   string
		heard    bool
		heardSeq uint64
		want     bool
	}{
		{"never heard", "peer:b", false, 0, true},
		{"accepted since", "peer:b", true, 2, true},
		{"already offered", "peer:b", true, 3, false},
		{"older than the last answer", "peer:b", true, 5, false},
		{"came from the peer", "mac", false, 0, false},
	} {
		if got := needsCatchUp(v, c.origin, c.heard, c.heardSeq); got != c.want {
			t.Errorf("%s: needsCatchUp = %v, want %v", c.name, got, c.want)
		}
	}
}

// A peer with sharedClipboard off is polled again only after
// bridgeOffRetry, is not posted to meanwhile, and gets the hub's value once
// it turns sharing on.
func TestBridgeBackoffDisabled(t *testing.T) {
	a, b := newClipNode(t, true), newClipNode(t, false)
	a.peerWith(t, "b", b)
	eventually(t, "first poll", func() bool { return b.gets.Load() >= 1 })
	gets := b.gets.Load()
	pub, _ := a.share.Publish(clipshare.Value{Text: "while off", Origin: "mac"})
	time.Sleep(bridgeOffRetry / 2)
	if n := b.gets.Load(); n != gets {
		t.Fatalf("polled %d times within the backoff, want 0", n-gets)
	}
	if n := b.posts.Load(); n != 0 {
		t.Fatalf("posted %d times to a peer with sharing off", n)
	}

	b.share.SetEnabled(true)
	eventually(t, "b catches up on a's value", func() bool {
		v, _ := b.share.Current()
		return v.ID == pub.ID
	})
}

func TestBridgeStopsForRemovedPeer(t *testing.T) {
	a, b := newClipNode(t, true), newClipNode(t, true)
	a.peerWith(t, "b", b)
	eventually(t, "bridged", func() bool { return a.bridged("b") })
	if w := a.do(http.MethodDelete, "/api/servers?name=b", ""); w.Code/100 != 2 {
		t.Fatalf("remove = %d %s", w.Code, w.Body)
	}
	eventually(t, "bridge stopped", func() bool { return !a.bridged("b") })
	time.Sleep(100 * time.Millisecond)
	gets := b.gets.Load()
	time.Sleep(bridgeWait + 200*time.Millisecond)
	if n := b.gets.Load(); n != gets {
		t.Fatalf("polled %d times after removal", n-gets)
	}
}
