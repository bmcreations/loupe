package httpapi

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/peer"
	"github.com/bmcreations/loupe/internal/target"
)

func testCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "loupe-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// startTLS serves h on addr (a free port when empty) with a certificate no
// system root signed.
func startTLS(t *testing.T, addr string, cert tls.Certificate, h http.Handler) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(h)
	if addr != "" {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		ts.Listener.Close()
		ts.Listener = l
	}
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

// ownerServer is a real loupe server with a synthetic target and token "tok".
func ownerServer() *Server {
	m := NewManager(target.Options{})
	m.Register(&target.Synthetic{})
	s := &Server{mgr: m, claims: newClaimBook(nil), token: "tok"}
	return s
}

type hub struct {
	*Server
	h       http.Handler
	servers string
}

// newHub is a server with no targets of its own and a registry reading
// servers.json under a temp dir. Polling runs until the test ends.
func newHub(t *testing.T) *hub {
	t.Helper()
	s := &Server{mgr: NewManager(target.Options{}), claims: newClaimBook(nil)}
	path := filepath.Join(t.TempDir(), "servers.json")
	reg, err := peer.NewRegistry(peer.NewStore(path), nil)
	if err != nil {
		t.Fatal(err)
	}
	s.SetPeers(reg)
	// Most tests add owners that have no QUIC listener, and a probe would
	// wait out its timeout on each. quicHub switches it back on.
	reg.SetProbe(nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reg.Start(ctx)
	return &hub{Server: s, h: s.Handler(), servers: path}
}

func (h *hub) do(method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}

// add posts the server and, when the hub asks to have the certificate
// confirmed, confirms it, as a person does in the Add server form.
func (h *hub) add(t *testing.T, name, url, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := ServerAddRequest{Name: name, URL: url, Token: token}
	b, _ := json.Marshal(req)
	w := h.do(http.MethodPost, "/api/servers", string(b))
	var er ErrorResponse
	if w.Code == http.StatusConflict && json.Unmarshal(w.Body.Bytes(), &er) == nil && er.Code == ErrCodeConfirmFingerprint {
		req.Fingerprint = er.Fingerprint
		b, _ = json.Marshal(req)
		w = h.do(http.MethodPost, "/api/servers", string(b))
	}
	return w
}

func (h *hub) targets(t *testing.T, query string) []TargetInfo {
	t.Helper()
	w := h.do(http.MethodGet, "/api/targets"+query, "")
	var rows []TargetInfo
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return rows
}

func find(rows []TargetInfo, id string) *TargetInfo {
	for i := range rows {
		if rows[i].ID == id {
			return &rows[i]
		}
	}
	return nil
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("never: %s", what)
}

func TestAddingAServerMergesItsTargetsUnderItsName(t *testing.T) {
	cert := testCert(t)
	owner := ownerServer()
	ts := startTLS(t, "", cert, owner.Handler())
	h := newHub(t)

	w := h.add(t, "mini", ts.URL, "tok")
	if w.Code != http.StatusCreated {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}

	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	raw, err := os.ReadFile(h.servers)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(peer.Fingerprint(leaf))) {
		t.Fatalf("servers.json has no pin: %s", raw)
	}
	if st, _ := os.Stat(h.servers); st.Mode().Perm() != 0o600 {
		t.Fatalf("servers.json mode = %v", st.Mode())
	}

	// A list never shows the token.
	list := h.do(http.MethodGet, "/api/servers", "")
	if strings.Contains(list.Body.String(), "tok") || !strings.Contains(list.Body.String(), `"name":"mini"`) {
		t.Fatalf("GET /api/servers = %s", list.Body)
	}

	rows := h.targets(t, "")
	row := find(rows, "mini/synthetic")
	if row == nil || row.Server != "mini" || row.Kind != string(target.KindSynthetic) || !row.Available {
		t.Fatalf("rows = %+v", rows)
	}
	if _, ok := h.mgr.Target("mini/synthetic"); !ok {
		t.Fatal("mini/synthetic does not resolve in the manager")
	}
	if rows := h.targets(t, "?local=1"); len(rows) != 0 {
		t.Fatalf("?local=1 listed %+v", rows)
	}
	// The owner's own listing never carries a server.
	ow := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/targets", nil)
	req.Header.Set("Authorization", "Bearer tok")
	owner.Handler().ServeHTTP(ow, req)
	if strings.Contains(ow.Body.String(), `"server"`) {
		t.Fatalf("owner row has a server: %s", ow.Body)
	}

	if w := h.do(http.MethodDelete, "/api/servers?name=mini", ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", w.Code, w.Body)
	}
	if rows := h.targets(t, ""); len(rows) != 0 {
		t.Fatalf("after delete: %+v", rows)
	}
	if _, ok := h.mgr.Target("mini/synthetic"); ok {
		t.Fatal("mini/synthetic still resolves")
	}
	if w := h.do(http.MethodDelete, "/api/servers?name=mini", ""); w.Code != http.StatusNotFound {
		t.Fatalf("second delete = %d", w.Code)
	}
}

func TestAnUnverifiedServerIsAddedOnlyOnceItsFingerprintIsConfirmed(t *testing.T) {
	cert := testCert(t)
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	var authorized atomic.Int32
	owner := ownerServer().Handler()
	ts := startTLS(t, "", cert, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			authorized.Add(1)
		}
		owner.ServeHTTP(w, r)
	}))
	h := newHub(t)
	post := func(fp string) (int, ErrorResponse) {
		b, _ := json.Marshal(ServerAddRequest{Name: "mini", URL: ts.URL, Token: "tok", Fingerprint: fp})
		w := h.do(http.MethodPost, "/api/servers", string(b))
		var er ErrorResponse
		_ = json.Unmarshal(w.Body.Bytes(), &er)
		return w.Code, er
	}

	code, er := post("")
	if code != http.StatusConflict || er.Code != ErrCodeConfirmFingerprint || er.Fingerprint != peer.Fingerprint(leaf) {
		t.Fatalf("unconfirmed add = %d %+v, want 409 confirmFingerprint %s", code, er, peer.Fingerprint(leaf))
	}
	code, er = post(strings.Repeat("0", 64))
	if code != http.StatusConflict || er.Code != ErrCodeConfirmFingerprint || er.Fingerprint != peer.Fingerprint(leaf) {
		t.Fatalf("add with the wrong fingerprint = %d %+v", code, er)
	}
	if n := authorized.Load(); n != 0 {
		t.Fatalf("the token reached the peer %d times before its certificate was confirmed", n)
	}
	if code, er = post(er.Fingerprint); code != http.StatusCreated {
		t.Fatalf("confirmed add = %d %+v", code, er)
	}
	if authorized.Load() == 0 {
		t.Fatal("the confirmed add never reached the peer")
	}
}

func TestAddingAServerThatCannotBeReachedLeavesNoTrace(t *testing.T) {
	ts := startTLS(t, "", testCert(t), ownerServer().Handler())
	h := newHub(t)

	if w := h.add(t, "mini", ts.URL, "nope"); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d %s", w.Code, w.Body)
	}
	w := h.add(t, "mini", "https://127.0.0.1:1", "tok")
	var e ErrorResponse
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if w.Code != http.StatusBadGateway || e.Code != ErrCodeServerDown {
		t.Errorf("dead address = %d %s", w.Code, w.Body)
	}
	if w := h.add(t, "a/b", ts.URL, "tok"); w.Code != http.StatusBadRequest {
		t.Errorf("slash in name = %d", w.Code)
	}
	if got := strings.TrimSpace(h.do(http.MethodGet, "/api/servers", "").Body.String()); got != "[]" {
		t.Errorf("list = %s", got)
	}
	if _, err := os.Stat(h.servers); err == nil {
		t.Error("a failed add wrote servers.json")
	}
	if w := h.add(t, "mini", ts.URL, "tok"); w.Code != http.StatusCreated {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}
	if w := h.add(t, "mini", ts.URL, "tok"); w.Code != http.StatusConflict {
		t.Errorf("duplicate = %d", w.Code)
	}
}

func TestAPeerRowThatAlreadyHasAServerIsDropped(t *testing.T) {
	rows := `[
	  {"id":"sim:A","kind":"simulator","label":"A","active":false,"available":true,"claim":{"target":"sim:A","agent":"x","since":"2026-10-26T00:00:00Z","idleMs":0,"expiresInMs":1},"handsOff":true,"drivenBy":"x"},
	  {"id":"other/sim:B","kind":"simulator","label":"B","available":true,"server":"other"}]`
	ts := startTLS(t, "", testCert(t), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("local") != "1" {
			t.Errorf("polled %s without local=1", r.URL)
		}
		_, _ = io.WriteString(w, rows)
	}))
	h := newHub(t)
	if w := h.add(t, "mini", ts.URL, "tok"); w.Code != http.StatusCreated {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}
	got := h.targets(t, "")
	if len(got) != 1 || got[0].ID != "mini/sim:A" {
		t.Fatalf("rows = %+v", got)
	}
	// The owner's claim, hands-off and driving stand as it reported them,
	// whatever this hub's own book says about the namespaced id.
	if got[0].Claim == nil || !got[0].HandsOff || got[0].DrivenBy != "x" {
		t.Fatalf("overlay lost the owner's fields: %+v", got[0])
	}
}

func TestTheHubsClaimBookNeverDecoratesAPeerRow(t *testing.T) {
	h := newHub(t)
	if err := h.claims.setHandsOff("mini/sim:A", true); err != nil {
		t.Fatal(err)
	}
	rows := []TargetInfo{{ID: "mini/sim:A", Server: "mini"}, {ID: "mini/sim:A"}}
	h.claims.decorate(rows)
	if rows[0].HandsOff || !rows[1].HandsOff {
		t.Fatalf("decorate = %+v", rows)
	}
}

func TestPeerRowsAreSkippedByCodeThatReadsLocalState(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&target.Synthetic{})
	m.SetPeerRows([]TargetInfo{
		{ID: "mini/avd:Pixel", Server: "mini", Kind: "emulator", Label: "Pixel", Available: true},
		{ID: "mini/sim:UDID", Server: "mini", Kind: "simulator", Label: "iPhone", Available: true},
	})
	if got := m.AVDNames(); len(got) != 0 {
		t.Errorf("AVDNames = %v", got)
	}
	if got := m.SimulatorNames(); len(got) != 0 {
		t.Errorf("SimulatorNames = %v", got)
	}
	for _, ti := range m.Targets(context.Background()) {
		if ti.Server != "" && (len(deviceActions(ti)) != 0 || lifeAllowed(ti, "reset")) {
			t.Errorf("%s offers a device action", ti.ID)
		}
	}
	m.pauseIdle(context.Background())
	if got := m.LocalTargets(context.Background()); len(got) != 1 || got[0].ID != "synthetic" {
		t.Errorf("LocalTargets = %+v", got)
	}
	if _, err := m.Acquire(context.Background(), "mini/sim:UDID"); err == nil || !strings.Contains(err.Error(), "cannot stream from") {
		t.Errorf("Acquire = %v", err)
	}
}

func TestAServerThatStopsAnsweringIsMarkedThenDroppedThenComesBack(t *testing.T) {
	defer peer.SetTimings(20*time.Millisecond, 150*time.Millisecond, 600*time.Millisecond)()
	cert := testCert(t)
	owner := ownerServer()
	ts := startTLS(t, "", cert, owner.Handler())
	h := newHub(t)
	if w := h.add(t, "mini", ts.URL, "tok"); w.Code != http.StatusCreated {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}
	addr := ts.Listener.Addr().String()
	ts.Close()

	eventually(t, "row unavailable with serverDown", func() bool {
		r := find(h.targets(t, ""), "mini/synthetic")
		return r != nil && !r.Available && r.Reason == ErrCodeServerDown
	})
	if _, err := h.mgr.Acquire(context.Background(), "mini/synthetic"); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("Acquire of a down peer's target = %v", err)
	}
	eventually(t, "row dropped", func() bool { return len(h.targets(t, "")) == 0 })

	startTLS(t, addr, cert, owner.Handler())
	eventually(t, "row back", func() bool {
		r := find(h.targets(t, ""), "mini/synthetic")
		return r != nil && r.Available
	})
}

func TestAChangedCertificateTakesTheServerDownUntilItIsAddedAgain(t *testing.T) {
	defer peer.SetTimings(20*time.Millisecond, 100*time.Millisecond, time.Minute)()
	owner := ownerServer()
	ts := startTLS(t, "", testCert(t), owner.Handler())
	h := newHub(t)
	if w := h.add(t, "mini", ts.URL, "tok"); w.Code != http.StatusCreated {
		t.Fatalf("add = %d", w.Code)
	}
	addr := ts.Listener.Addr().String()
	ts.Close()
	startTLS(t, addr, testCert(t), owner.Handler())

	eventually(t, "row unavailable", func() bool {
		r := find(h.targets(t, ""), "mini/synthetic")
		return r != nil && !r.Available
	})
	var list []ServerInfo
	_ = json.Unmarshal(h.do(http.MethodGet, "/api/servers", "").Body.Bytes(), &list)
	if len(list) != 1 || list[0].State != "down" {
		t.Fatalf("servers = %+v", list)
	}
	// Adding it again under a new name takes the new certificate.
	if w := h.add(t, "mini2", ts.URL, "tok"); w.Code != http.StatusCreated {
		t.Fatalf("re-add = %d %s", w.Code, w.Body)
	}
}
