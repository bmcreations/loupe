package target

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"
)

// peerCall records the one request a PeerTarget sends and answers it with
// resp, or err.
func peerCall(got *PeerRequest, resp *PeerResponse, err error) PeerCaller {
	return func(_ context.Context, _ string, req PeerRequest) (*PeerResponse, error) {
		*got = req
		return resp, err
	}
}

func TestPeerClipboardForwards(t *testing.T) {
	var got PeerRequest
	p := NewPeerTarget("studio", "android:emulator-5554", KindEmulator, "Pixel", nil,
		peerCall(&got, &PeerResponse{Status: 200, Body: []byte(`{"ok":true}`)}, nil))
	var cb Clipboarder = p
	if err := cb.WriteClipboard(context.Background(), "x", true); err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", got.Method)
	}
	u, err := url.Parse(got.Path)
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/api/clipboard" || u.Query().Get("target") != "android:emulator-5554" {
		t.Errorf("path = %s, want /api/clipboard?target=android:emulator-5554", got.Path)
	}
	var body struct {
		Text  string `json:"text"`
		Paste bool   `json:"paste"`
	}
	if err := json.Unmarshal(got.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Text != "x" || !body.Paste {
		t.Errorf("body = %+v, want text x, paste true", body)
	}
}

func TestPeerClipboardReadParses(t *testing.T) {
	var got PeerRequest
	p := NewPeerTarget("studio", "sim:ABC", KindSimulator, "iPhone", nil,
		peerCall(&got, &PeerResponse{Status: 200, Body: []byte(`{"target":"sim:ABC","text":"hello"}`)}, nil))
	text, err := p.ReadClipboard(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if text != "hello" {
		t.Errorf("text = %q, want hello", text)
	}
	if got.Method != http.MethodGet || got.Body != nil {
		t.Errorf("request = %s with body %q, want a bare GET", got.Method, got.Body)
	}
}

// An owner with no session for the target answers 409, which reaches the
// hub as the owner's PeerError rather than as a dead peer.
func TestPeerClipboardOwnerConflict(t *testing.T) {
	var got PeerRequest
	owner := &PeerError{Status: http.StatusConflict, Body: []byte(`{"error":"no session"}`)}
	p := NewPeerTarget("studio", "sim:ABC", KindSimulator, "iPhone", nil, peerCall(&got, nil, owner))
	_, err := p.ReadClipboard(context.Background())
	var pe *PeerError
	if !errors.As(err, &pe) || pe.Status != http.StatusConflict {
		t.Fatalf("err = %v, want the owner's 409", err)
	}
}
