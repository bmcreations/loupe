package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bmcreations/loupe/internal/httpapi"
)

// flagServer answers GET /api/flags from a script: the answer for each
// ?after= revision, and a block for any it has none for.
func flagServer(t *testing.T, script map[string]httpapi.FlagsResponse) *Client {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp, ok := script[r.URL.Query().Get("after")]
		if !ok {
			<-r.Context().Done()
			return
		}
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(ts.Close)
	c, err := NewClient(ts.URL, "tok", "claude")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type pushed struct {
	Content string            `json:"content"`
	Meta    map[string]string `json:"meta"`
}

func TestPushFlagsSendsNewFlagsAndEndedWatchesAsChannelMessages(t *testing.T) {
	c := flagServer(t, map[string]httpapi.FlagsResponse{
		// Already on the server at start: raised for someone else.
		"": {Rev: 1, Flags: []httpapi.Flag{{ID: 1, Target: "emulator-5554", Rev: 1}}},
		"1": {Rev: 2, Flags: []httpapi.Flag{{ID: 2, Target: "emulator-5554", Rev: 2, Note: "button clipped",
			Region: &httpapi.FlagRect{X: 10, Y: 20, Width: 300, Height: 80}, ClaimedBy: "claude", Watching: true}}},
		"2": {Rev: 3, Flags: []httpapi.Flag{{ID: 2, Target: "emulator-5554", Rev: 3, ClaimedBy: "claude",
			End: &httpapi.FlagSnapshot{}, Events: []httpapi.FlagEvent{{Kind: "tap"}, {Kind: "swipe"}}}}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, peer := mcp.NewInMemoryTransports()
	ct := &channelTransport{Transport: st}
	if _, err := ct.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	pconn, err := peer.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c.claims["emulator-5554"] = true
	go pushFlags(ctx, c, ct)

	var got []pushed
	for len(got) < 2 {
		msg, err := pconn.Read(ctx)
		if err != nil {
			t.Fatalf("read after %d messages: %v", len(got), err)
		}
		req, ok := msg.(*jsonrpc.Request)
		if !ok || req.Method != channelMethod || req.ID.IsValid() {
			t.Fatalf("got %#v, want a %s notification", msg, channelMethod)
		}
		var p pushed
		if err := json.Unmarshal(req.Params, &p); err != nil {
			t.Fatal(err)
		}
		got = append(got, p)
	}

	if m := got[0].Meta; m["flag_id"] != "2" || m["state"] != "watching" || m["target"] != "emulator-5554" || m["claimed_by"] != "claude" || m["held_by_you"] != "true" {
		t.Errorf("first meta = %v", m)
	}
	for _, want := range []string{`"button clipped"`, "10,20, 300x80", "reproducing it now", "flag tool, id 2"} {
		if !strings.Contains(got[0].Content, want) {
			t.Errorf("first content %q lacks %q", got[0].Content, want)
		}
	}
	if m := got[1].Meta; m["flag_id"] != "2" || m["state"] != "ended" {
		t.Errorf("second meta = %v", m)
	}
	if !strings.Contains(got[1].Content, "2 inputs") {
		t.Errorf("second content %q lacks the input count", got[1].Content)
	}
}

func TestChannelCapabilityIsDeclared(t *testing.T) {
	cs, stop := connect(t, &fakeLoupe{})
	defer stop()
	caps := cs.InitializeResult().Capabilities
	if _, ok := caps.Experimental[channelCapability]; !ok {
		t.Errorf("experimental capabilities = %v, want %s", caps.Experimental, channelCapability)
	}
	if caps.Tools == nil {
		t.Error("tools capability missing")
	}
	if v := cs.InitializeResult().ProtocolVersion; v > "2025-11-25" {
		t.Errorf("negotiated %s; Claude Code registers no channel past 2025-11-25", v)
	}
}

func TestFlagToolReturnsTheRecordAndBothScreenshots(t *testing.T) {
	fake := &fakeLoupe{handle: func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/api/flags":
			json.NewEncoder(w).Encode(httpapi.Flag{ID: 4, Target: "avd:Pixel_10",
				Start: httpapi.FlagSnapshot{Screenshot: "/api/flags/shot?id=4&which=start"},
				End:   &httpapi.FlagSnapshot{Screenshot: "/api/flags/shot?id=4&which=end"}})
		case "/api/flags/shot":
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("png-" + r.URL.Query().Get("which")))
		default:
			return false
		}
		return true
	}}
	cs, stop := connect(t, fake)
	defer stop()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "flag", Arguments: map[string]any{"id": 4}})
	if err != nil {
		t.Fatal(err)
	}
	var imgs []string
	for _, c := range res.Content {
		if im, ok := c.(*mcp.ImageContent); ok {
			imgs = append(imgs, string(im.Data))
		}
	}
	if strings.Join(imgs, ",") != "png-start,png-end" {
		t.Errorf("images = %v", imgs)
	}
	want := []string{"GET /api/flags?id=4", "GET /api/flags/shot?id=4&which=start", "GET /api/flags/shot?id=4&which=end"}
	if got := fake.requests(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("requests = %v, want %v", got, want)
	}
}

func TestAnInterruptIsPushedAsStateInterruptThenEnded(t *testing.T) {
	open := httpapi.Flag{ID: 4, Target: "dev1", Kind: httpapi.FlagInterrupt, Watching: true}
	m := flagMeta(open, true)
	if m["state"] != "interrupt" || m["kind"] != "interrupt" || m["held_by_you"] != "true" {
		t.Errorf("open meta = %v", m)
	}
	if msg := flagMessage(open, false); !strings.Contains(msg, "Stop driving") {
		t.Errorf("open message = %q", msg)
	}
	ended := open
	ended.Watching = false
	ended.End = &httpapi.FlagSnapshot{}
	ended.Note = "wrong chat"
	if m := flagMeta(ended, true); m["state"] != "ended" || m["kind"] != "interrupt" {
		t.Errorf("ended meta = %v", m)
	}
	if msg := flagMessage(ended, true); !strings.Contains(msg, `"wrong chat"`) || !strings.Contains(msg, "refused until you do") {
		t.Errorf("ended message = %q", msg)
	}
}

func TestPushFlagsSendsAFlagSentToOneSessionOnlyToIt(t *testing.T) {
	c := flagServer(t, map[string]httpapi.FlagsResponse{
		"": {Rev: 0, Flags: []httpapi.Flag{}},
		"0": {Rev: 2, Flags: []httpapi.Flag{
			{ID: 1, Target: "dev1", Rev: 1, Note: "for another", To: &httpapi.FlagRecipient{Agent: "claude", Session: "other"}},
			{ID: 2, Target: "dev1", Rev: 2, Note: "for me", To: &httpapi.FlagRecipient{Agent: "claude", Session: "me"}},
		}},
	})
	c.session = "me"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, peer := mcp.NewInMemoryTransports()
	ct := &channelTransport{Transport: st}
	if _, err := ct.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	pconn, err := peer.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go pushFlags(ctx, c, ct)

	msg, err := pconn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var p pushed
	if err := json.Unmarshal(msg.(*jsonrpc.Request).Params, &p); err != nil {
		t.Fatal(err)
	}
	if p.Meta["flag_id"] != "2" || p.Meta["to_you"] != "true" {
		t.Errorf("first push = %v, want flag 2 marked to_you; flag 1 went to another session", p.Meta)
	}
}

func TestPushFlagsReleasesASessionAnInterruptWasNotSentTo(t *testing.T) {
	c := flagServer(t, map[string]httpapi.FlagsResponse{
		"":  {Rev: 0, Flags: []httpapi.Flag{}},
		"0": {Rev: 1, Flags: []httpapi.Flag{{ID: 1, Target: "dev1", Rev: 1, Kind: httpapi.FlagInterrupt, Watching: true}}},
		"1": {Rev: 2, Flags: []httpapi.Flag{{ID: 1, Target: "dev1", Rev: 2, Kind: httpapi.FlagInterrupt,
			End: &httpapi.FlagSnapshot{}, To: &httpapi.FlagRecipient{Agent: "claude", Session: "other"}}}},
	})
	c.session = "me"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, peer := mcp.NewInMemoryTransports()
	ct := &channelTransport{Transport: st}
	if _, err := ct.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	pconn, err := peer.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go pushFlags(ctx, c, ct)

	var got []pushed
	for len(got) < 2 {
		msg, err := pconn.Read(ctx)
		if err != nil {
			t.Fatalf("read after %d: %v", len(got), err)
		}
		var p pushed
		if err := json.Unmarshal(msg.(*jsonrpc.Request).Params, &p); err != nil {
			t.Fatal(err)
		}
		got = append(got, p)
	}
	if got[0].Meta["state"] != "interrupt" {
		t.Errorf("first = %v, want the interrupt", got[0].Meta)
	}
	if m := got[1].Meta; m["state"] != "released" || m["to_you"] != "" || !strings.Contains(got[1].Content, "may go on") {
		t.Errorf("second = %v %q, want a release", m, got[1].Content)
	}
}

func TestPushFlagsLeavesAFlagSentToT3ToTheServer(t *testing.T) {
	c := flagServer(t, map[string]httpapi.FlagsResponse{
		"": {Rev: 0, Flags: []httpapi.Flag{}},
		"0": {Rev: 2, Flags: []httpapi.Flag{
			{ID: 1, Target: "dev1", Rev: 1, To: &httpapi.FlagRecipient{T3Thread: "469bab44-1f43-4536-b0f6-3349d0c7092f"}},
			{ID: 2, Target: "dev1", Rev: 2},
		}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, peer := mcp.NewInMemoryTransports()
	ct := &channelTransport{Transport: st}
	if _, err := ct.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	pconn, err := peer.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go pushFlags(ctx, c, ct)

	msg, err := pconn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var p pushed
	if err := json.Unmarshal(msg.(*jsonrpc.Request).Params, &p); err != nil {
		t.Fatal(err)
	}
	if p.Meta["flag_id"] != "2" {
		t.Errorf("first push = %v, want flag 2; flag 1 went to T3", p.Meta)
	}
}

func TestAnInterruptSentToT3StillReachesTheHolder(t *testing.T) {
	f := httpapi.Flag{ID: 4, Target: "dev1", Kind: httpapi.FlagInterrupt, End: &httpapi.FlagSnapshot{},
		To: &httpapi.FlagRecipient{T3Project: "/work/app"}}
	if m := flagMeta(f, true); m["to_you"] != "" || m["state"] != "ended" {
		t.Errorf("meta = %v; want ended, not to_you", m)
	}
	if msg := flagMessage(f, true); !strings.Contains(msg, "T3 Code thread") || !strings.Contains(msg, "refused until you do") {
		t.Errorf("message = %q", msg)
	}
}
