package revyl

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/bmcreations/loupe/internal/h264"

	"github.com/bmcreations/loupe/internal/target"
)

// clipAUs splits testdata/whep/clip-360x202.h264 into access units, each in
// Annex B. The clip is 12 frames of ffmpeg's testsrc at 360x202 (202 is not a
// multiple of 16, so the SPS crops), Constrained Baseline, no B-frames, one
// GOP. Its IDR is larger than one MTU, so the server sends it as FU-A, and
// pion puts SPS+PPS in a STAP-A ahead of it. Made with:
//
//	ffmpeg -f lavfi -i testsrc=size=360x202:rate=30 -frames:v 12 -c:v libx264 \
//	  -profile:v baseline -bf 0 -g 12 -pix_fmt yuv420p \
//	  -x264-params keyint=12:scenecut=0 -f h264 clip-360x202.h264
func clipAUs(t *testing.T) [][]byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "whep", "clip-360x202.h264"))
	if err != nil {
		t.Fatal(err)
	}
	var aus [][]byte
	var cur []byte
	sawPic := false
	h264.SplitAnnexB(raw, func(nal []byte) {
		if h264.StartsNewPicture(nal) && sawPic || (!h264.IsVCL(nal) && sawPic) {
			aus = append(aus, cur)
			cur, sawPic = nil, false
		}
		cur = append(cur, 0, 0, 0, 1)
		cur = append(cur, nal...)
		if h264.IsVCL(nal) {
			sawPic = true
		}
	})
	aus = append(aus, cur)
	if len(aus) != 12 {
		t.Fatalf("clip has %d access units, want 12", len(aus))
	}
	return aus
}

// vclOnly keeps the picture NALs of an Annex B access unit, the part a
// picture Frame carries (parameter sets travel in Config frames).
func vclOnly(au []byte) []byte {
	var out []byte
	h264.SplitAnnexB(au, func(nal []byte) {
		switch h264.Type(nal) {
		case h264.NALSPS, h264.NALPPS, h264.NALAUD:
			return
		}
		out = append(out, 0, 0, 0, 1)
		out = append(out, nal...)
	})
	return out
}

// fakeWHEP is an in-process WHEP server. It starts mid-GOP, sending P-frames
// a decoder cannot use, and sends the clip from its IDR only when a PLI
// arrives. After the last frame it goes quiet, the way a still screen does.
type fakeWHEP struct {
	t   *testing.T
	aus [][]byte
	srv *httptest.Server

	mu       sync.Mutex
	auth     string
	deleted  string
	plis     int
	pli      chan struct{}
	pcs      []*webrtc.PeerConnection
	finished chan struct{} // closed after the first full clip is sent
}

func newFakeWHEP(t *testing.T) *fakeWHEP {
	f := &fakeWHEP{t: t, aus: clipAUs(t), pli: make(chan struct{}, 16), finished: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /whep/endpoint", f.offer)
	mux.HandleFunc("DELETE /whep/res/1", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.deleted = r.URL.Path
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(func() {
		f.srv.Close()
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, pc := range f.pcs {
			_ = pc.Close()
		}
	})
	return f
}

func loopbackSettings() *webrtc.SettingEngine {
	se := &webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	return se
}

func (f *fakeWHEP) offer(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "application/sdp" {
		http.Error(w, "want application/sdp, got "+ct, http.StatusUnsupportedMediaType)
		return
	}
	f.mu.Lock()
	f.auth = r.Header.Get("Authorization")
	f.mu.Unlock()
	sdp, _ := io.ReadAll(r.Body)

	m := &webrtc.MediaEngine{}
	_ = m.RegisterDefaultCodecs()
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(*loopbackSettings()))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	f.mu.Lock()
	f.pcs = append(f.pcs, pc)
	f.mu.Unlock()
	track, _ := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "loupe-test")
	sender, err := pc.AddTrack(track)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	go func() {
		for {
			pkts, _, err := sender.ReadRTCP()
			if err != nil {
				return
			}
			for _, p := range pkts {
				if _, ok := p.(*rtcp.PictureLossIndication); ok {
					f.mu.Lock()
					f.plis++
					f.mu.Unlock()
					select {
					case f.pli <- struct{}{}:
					default:
					}
				}
			}
		}
	}()
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(sdp)}); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	answer, _ := pc.CreateAnswer(nil)
	gathered := webrtc.GatheringCompletePromise(pc)
	_ = pc.SetLocalDescription(answer)
	<-gathered
	connected := make(chan struct{})
	var once sync.Once
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			once.Do(func() { close(connected) })
		}
	})
	go f.play(track, connected)

	// A relative Location, which the client has to resolve against the
	// endpoint's URL.
	w.Header().Set("Location", "/whep/res/1")
	w.Header().Set("Content-Type", "application/sdp")
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, pc.LocalDescription().SDP)
}

func (f *fakeWHEP) play(track *webrtc.TrackLocalStaticSample, connected <-chan struct{}) {
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		return
	}
	write := func(au []byte) {
		_ = track.WriteSample(media.Sample{Data: au, Duration: time.Second / 30})
		time.Sleep(5 * time.Millisecond)
	}
	// Mid-GOP: P-frames only, until asked for a keyframe.
	for i := 1; ; i = 1 + i%(len(f.aus)-1) {
		select {
		case <-f.pli:
			goto keyframe
		default:
		}
		write(f.aus[i])
		time.Sleep(30 * time.Millisecond)
	}
keyframe:
	for _, au := range f.aus {
		write(au)
	}
	close(f.finished)
	// Quiet, as a still screen is, except that each PLI resends the IDR.
	for {
		select {
		case <-f.pli:
			write(f.aus[0])
		case <-time.After(15 * time.Second):
			return
		}
	}
}

func TestWHEPReceivesDecodableFramesAtSPSSize(t *testing.T) {
	f := newFakeWHEP(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r, err := dialWHEP(ctx, WHEPConfig{
		URL:        f.srv.URL + "/whep/endpoint",
		Header:     http.Header{"Authorization": {"Bearer test-token"}},
		Name:       "Revyl Pixel 7",
		FirstFrame: 10 * time.Second,
		settings:   loopbackSettings(),
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Dial returns once the first SPS is in, so Meta is already real.
	if m := r.Meta(); m.Width != 360 || m.Height != 202 || m.Codec != "h264" || m.Name != "Revyl Pixel 7" {
		t.Fatalf("Meta = %+v, want h264 360x202 named Revyl Pixel 7", m)
	}

	// The P-frames sent before the PLI are dropped: the first frame out is
	// the parameter sets, then the IDR, then the clip in order. The last
	// one arrives although nothing follows it.
	var got []target.Frame
	timeout := time.After(10 * time.Second)
	for len(got) < 1+len(f.aus) {
		select {
		case fr, ok := <-r.Frames():
			if !ok {
				t.Fatalf("frames closed after %d: %v", len(got), r.Err())
			}
			got = append(got, fr)
		case <-timeout:
			t.Fatalf("got %d frames, want %d", len(got), 1+len(f.aus))
		}
	}
	if !got[0].Config || h264.FindSPS(got[0].Data) == nil {
		t.Fatalf("first frame is not the parameter sets: %+v", got[0])
	}
	for i, au := range f.aus {
		fr := got[1+i]
		if fr.Config {
			t.Fatalf("frame %d is a Config frame", 1+i)
		}
		if fr.Key != (i == 0) {
			t.Fatalf("frame %d Key = %v, want %v", 1+i, fr.Key, i == 0)
		}
		if !bytes.Equal(fr.Data, vclOnly(au)) {
			t.Fatalf("frame %d differs from the clip's access unit %d (%d vs %d bytes)", 1+i, i, len(fr.Data), len(vclOnly(au)))
		}
	}
	if got[len(got)-1].PTS <= got[1].PTS {
		t.Fatalf("PTS did not advance: first %v last %v", got[1].PTS, got[len(got)-1].PTS)
	}

	// And an actual decoder agrees, where one is installed.
	if ffprobe, err := exec.LookPath("ffprobe"); err == nil {
		var es []byte
		for _, fr := range got {
			es = append(es, fr.Data...)
		}
		p := filepath.Join(t.TempDir(), "out.h264")
		if err := os.WriteFile(p, es, 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(ffprobe, "-v", "error", "-count_frames", "-select_streams", "v:0",
			"-show_entries", "stream=width,height,nb_read_frames", "-of", "csv=p=0", p).CombinedOutput()
		if err != nil || string(bytes.TrimSpace(out)) != "360,202,12" {
			t.Fatalf("ffprobe: %q err %v, want 360,202,12", out, err)
		}
	}

	// RequestKeyframe reaches the server as a PLI and brings parameter sets
	// and an IDR back.
	f.mu.Lock()
	before := f.plis
	f.mu.Unlock()
	if err := r.RequestKeyframe(); err != nil {
		t.Fatalf("RequestKeyframe: %v", err)
	}
	var cfg, key bool
	timeout = time.After(10 * time.Second)
	for !(cfg && key) {
		select {
		case fr := <-r.Frames():
			cfg = cfg || fr.Config
			key = key || fr.Key
		case <-timeout:
			t.Fatal("no keyframe after RequestKeyframe")
		}
	}
	f.mu.Lock()
	after, auth := f.plis, f.auth
	f.mu.Unlock()
	if after <= before {
		t.Fatalf("PLIs before %d after %d: RequestKeyframe sent none", before, after)
	}
	if auth != "Bearer test-token" {
		t.Fatalf("offer Authorization = %q, want the configured header", auth)
	}

	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-r.Frames(); ok {
		// Drain whatever was buffered; the channel must end closed.
		for range r.Frames() {
		}
	}
	if err := r.Err(); err != nil {
		t.Fatalf("Err after Close = %v, want nil", err)
	}
	f.mu.Lock()
	deleted := f.deleted
	f.mu.Unlock()
	if deleted != "/whep/res/1" {
		t.Fatalf("DELETE went to %q, want the Location /whep/res/1", deleted)
	}
}

func TestWHEPRefusedOfferKeepsURLOutOfError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "session not ready", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	secret := srv.URL + "/whep?token=s3cret"
	_, err := dialWHEP(ctx, WHEPConfig{URL: secret, settings: loopbackSettings()})
	if err == nil {
		t.Fatal("dial succeeded against a refusing server")
	}
	if bytes.Contains([]byte(err.Error()), []byte("s3cret")) {
		t.Fatalf("error leaks the URL: %v", err)
	}

	// A transport error is a *url.Error, which prints the URL unless
	// stripped.
	_, err = dialWHEP(ctx, WHEPConfig{URL: "http://127.0.0.1:1/whep?token=s3cret", settings: loopbackSettings()})
	if err == nil || bytes.Contains([]byte(err.Error()), []byte("s3cret")) {
		t.Fatalf("transport error = %v, want one without the URL", err)
	}
}
