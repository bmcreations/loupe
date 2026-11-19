package target

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"

	"github.com/bmcreations/loupe/internal/h264"
)

// WHEPConfig says where a WHEP (RFC 9725) endpoint is and how to reach it.
type WHEPConfig struct {
	// URL is the endpoint the SDP offer is POSTed to. It is a bearer of
	// access on some services, so it never appears in an error or a log.
	URL string
	// Header is added to every request: the offer POST, and the DELETE that
	// ends the session. Nil sends none.
	Header http.Header
	// Client sends the requests. Nil uses a client with a 20 s timeout.
	Client *http.Client
	// ICEServers is passed to the peer connection. Nil gathers host
	// candidates only, which reaches a server with a public address.
	ICEServers []webrtc.ICEServer
	// Name becomes Meta.Name.
	Name string
	// FirstFrame bounds how long Dial waits for the first parameter sets,
	// which is when Meta first has a real size. Zero means 20 s.
	FirstFrame time.Duration

	// settings replaces the default SettingEngine. Tests use it to allow
	// loopback candidates.
	settings *webrtc.SettingEngine
}

// whepReceiver receives one H.264 video track over WHEP and hands it out as
// Annex B access units. It is the video half of a Stream; the caller adds
// input. See dialWHEP.
type whepReceiver struct {
	cfg      WHEPConfig
	pc       *webrtc.PeerConnection
	resource string // the session URL from Location, for the DELETE

	frames chan Frame
	done   chan struct{} // closed when the receiver starts shutting down
	closed chan struct{} // closed after frames is closed
	ready  chan struct{} // closed when the first Config frame is out
	once   sync.Once
	ready1 sync.Once
	wg     sync.WaitGroup

	mu       sync.Mutex
	meta     Meta
	err      error
	shutdown bool
	ssrc     uint32
	haveSSRC bool
}

// h264Fmtps are the H.264 profiles offered, so a server that only encodes one
// of them still finds a match. packetization-mode=1 is required for FU-A,
// which every frame larger than one MTU uses.
var h264Fmtps = []string{
	"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
	"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f",
	"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=4d001f",
	"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640032",
	"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640c1f",
}

// dialWHEP offers a receive-only H.264 track to cfg.URL and returns once the
// first parameter sets have arrived, so Meta carries the real size from the
// SPS. It sends a picture-loss indication while it waits, since a server
// that joined mid-GOP may not send a keyframe until asked.
func dialWHEP(ctx context.Context, cfg WHEPConfig) (*whepReceiver, error) {
	if cfg.URL == "" {
		return nil, errors.New("whep: no endpoint")
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 20 * time.Second}
	}
	if cfg.FirstFrame <= 0 {
		cfg.FirstFrame = 20 * time.Second
	}

	m := &webrtc.MediaEngine{}
	for i, fmtp := range h264Fmtps {
		err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:    webrtc.MimeTypeH264,
				ClockRate:   90000,
				SDPFmtpLine: fmtp,
				RTCPFeedback: []webrtc.RTCPFeedback{
					{Type: "goog-remb"}, {Type: "ccm", Parameter: "fir"},
					{Type: "nack"}, {Type: "nack", Parameter: "pli"},
				},
			},
			PayloadType: webrtc.PayloadType(102 + 2*i),
		}, webrtc.RTPCodecTypeVideo)
		if err != nil {
			return nil, fmt.Errorf("whep: register codec: %w", err)
		}
	}
	// NACK and receiver reports: without them a lost packet is never resent
	// and the server's congestion control sees no feedback.
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		return nil, fmt.Errorf("whep: interceptors: %w", err)
	}
	opts := []func(*webrtc.API){webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir)}
	if cfg.settings != nil {
		opts = append(opts, webrtc.WithSettingEngine(*cfg.settings))
	}
	pc, err := webrtc.NewAPI(opts...).NewPeerConnection(webrtc.Configuration{ICEServers: cfg.ICEServers})
	if err != nil {
		return nil, fmt.Errorf("whep: peer connection: %w", err)
	}
	r := &whepReceiver{
		cfg:    cfg,
		pc:     pc,
		frames: make(chan Frame, 64),
		done:   make(chan struct{}),
		closed: make(chan struct{}),
		ready:  make(chan struct{}),
		meta:   Meta{Codec: "h264", Name: cfg.Name},
	}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("whep: transceiver: %w", err)
	}
	pc.OnTrack(r.onTrack)
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		switch s {
		case webrtc.PeerConnectionStateFailed:
			r.finish(errors.New("whep: connection failed"))
		case webrtc.PeerConnectionStateClosed:
			r.finish(errors.New("whep: connection closed"))
		}
	})

	if err := r.negotiate(ctx); err != nil {
		r.finish(err)
		<-r.closed
		return nil, err
	}

	wait := time.NewTimer(cfg.FirstFrame)
	defer wait.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.ready:
			return r, nil
		case <-tick.C:
			_ = r.RequestKeyframe()
		case <-r.done:
			err := r.Err()
			if err == nil {
				err = errors.New("whep: stream ended before its first frame")
			}
			r.Close()
			return nil, err
		case <-wait.C:
			r.Close()
			return nil, fmt.Errorf("whep: no video within %s", cfg.FirstFrame)
		case <-ctx.Done():
			r.Close()
			return nil, ctx.Err()
		}
	}
}

// negotiate sends the offer and applies the answer. It gathers every
// candidate first rather than trickling them, since trickle is optional in
// WHEP and a server that lacks it would never see them.
func (r *whepReceiver) negotiate(ctx context.Context) error {
	offer, err := r.pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("whep: offer: %w", err)
	}
	gathered := webrtc.GatheringCompletePromise(r.pc)
	if err := r.pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("whep: local description: %w", err)
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		return ctx.Err()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.URL,
		strings.NewReader(r.pc.LocalDescription().SDP))
	if err != nil {
		return errors.New("whep: bad endpoint URL")
	}
	r.addHeaders(req)
	req.Header.Set("Content-Type", "application/sdp")
	req.Header.Set("Accept", "application/sdp")
	resp, err := r.cfg.Client.Do(req)
	if err != nil {
		return fmt.Errorf("whep: send offer: %w", redactURL(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("whep: read answer: %w", redactURL(err))
	}
	if resp.StatusCode/100 != 2 {
		// The body is the server's reason, not the request, so it is safe
		// to show; it is cut short in case it is a whole HTML page.
		return fmt.Errorf("whep: offer refused: %s: %s", resp.Status, clip(string(bytes.TrimSpace(body)), 200))
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		if u, err := resp.Request.URL.Parse(loc); err == nil {
			r.resource = u.String()
		}
	}
	if err := r.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer, SDP: string(body),
	}); err != nil {
		return fmt.Errorf("whep: answer: %w", err)
	}
	return nil
}

func (r *whepReceiver) addHeaders(req *http.Request) {
	for k, vs := range r.cfg.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
}

func (r *whepReceiver) onTrack(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
	if tr.Kind() != webrtc.RTPCodecTypeVideo {
		return
	}
	r.mu.Lock()
	if r.shutdown || r.haveSSRC {
		r.mu.Unlock()
		return
	}
	r.ssrc, r.haveSSRC = uint32(tr.SSRC()), true
	r.wg.Add(1)
	r.mu.Unlock()
	go r.read(tr)
}

// read assembles RTP packets into access units and emits them.
//
// An access unit ends on the RTP marker bit, the way RFC 6184 section 5.1
// says the last packet of a picture is flagged, and not when the next
// picture's first packet arrives. A jitter-buffer that waits for the next
// timestamp would hold the last picture of a still screen until something
// moved, so a tap's result would show one frame late or never.
func (r *whepReceiver) read(tr *webrtc.TrackRemote) {
	defer r.wg.Done()
	var (
		depack   codecs.H264Packet
		au       []byte
		ts       uint32
		lastSeq  uint16
		haveSeq  bool
		broken   bool
		needKey  = true
		ptsBase  uint32
		havePTS  bool
		sps, pps []byte
	)
	emit := func() {
		defer func() { au, broken = nil, false }()
		if broken || len(au) == 0 {
			return
		}
		if !havePTS {
			ptsBase, havePTS = ts, true
		}
		pts := time.Duration(int64(int32(ts-ptsBase))) * time.Second / 90000
		var pic []byte
		var key, newParams bool
		h264.SplitAnnexB(au, func(nal []byte) {
			switch h264.Type(nal) {
			case h264.NALSPS:
				sps, newParams = append([]byte(nil), nal...), true
			case h264.NALPPS:
				pps, newParams = append([]byte(nil), nal...), true
			case h264.NALAUD:
			default:
				if h264.Type(nal) == h264.NALIDR {
					key = true
				}
				pic = append(pic, 0, 0, 0, 1)
				pic = append(pic, nal...)
			}
		})
		if sps == nil || pps == nil || (needKey && !key) {
			// Nothing decodable yet: ask for a keyframe and drop this.
			if needKey {
				_ = r.RequestKeyframe()
			}
			return
		}
		// Parameter sets go out before the picture that needs them, and Meta
		// changes before they do: the session reads Meta when it sees a
		// Config frame. They are sent again before an IDR that came without
		// them, so a viewer that joins on any keyframe can decode it.
		if newParams || key {
			if w, h, err := h264.SPSDimensions(sps); err == nil {
				r.mu.Lock()
				r.meta.Width, r.meta.Height = w, h
				r.mu.Unlock()
			}
			cfg := append(append(append([]byte{0, 0, 0, 1}, sps...), 0, 0, 0, 1), pps...)
			if !r.send(Frame{PTS: pts, Config: true, Data: cfg, Captured: time.Now()}) {
				return
			}
			r.ready1.Do(func() { close(r.ready) })
		}
		if len(pic) == 0 {
			return
		}
		needKey = false
		r.send(Frame{PTS: pts, Key: key, Data: pic, Captured: time.Now()})
	}

	for {
		pkt, _, err := tr.ReadRTP()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				r.finish(fmt.Errorf("whep: read: %w", err))
			} else {
				r.finish(errors.New("whep: stream ended"))
			}
			return
		}
		if haveSeq && pkt.SequenceNumber != lastSeq+1 {
			// A packet was lost for good (NACK already had its chance).
			// The picture it belonged to is undecodable, and so is every
			// picture after it until the next keyframe.
			au, broken, needKey = nil, true, true
			depack = codecs.H264Packet{}
			_ = r.RequestKeyframe()
		}
		lastSeq, haveSeq = pkt.SequenceNumber, true
		if len(au) > 0 && pkt.Timestamp != ts {
			// A new picture started without the last one's marker bit.
			emit()
		}
		ts = pkt.Timestamp
		nal, err := depack.Unmarshal(pkt.Payload)
		if err != nil {
			broken, needKey = true, true
			depack = codecs.H264Packet{}
		} else {
			au = append(au, nal...)
		}
		if pkt.Marker {
			emit()
			broken = false
		}
	}
}

// send hands a frame to the consumer, or drops it if the receiver is
// shutting down. It reports whether the frame went out.
func (r *whepReceiver) send(f Frame) bool {
	select {
	case r.frames <- f:
		return true
	case <-r.done:
		return false
	}
}

// finish starts shutdown once: it records why, closes the peer connection
// and, after the reader has stopped, closes the frame channel.
func (r *whepReceiver) finish(err error) {
	r.once.Do(func() {
		r.mu.Lock()
		r.err = err
		r.shutdown = true
		r.mu.Unlock()
		close(r.done)
		go func() {
			_ = r.pc.Close()
			r.wg.Wait()
			close(r.frames)
			close(r.closed)
		}()
	})
}

func (r *whepReceiver) Meta() Meta {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.meta
}

func (r *whepReceiver) Frames() <-chan Frame { return r.frames }

// RequestKeyframe sends an RTCP picture-loss indication. Before the track
// has arrived there is nothing to address it to, and it does nothing.
func (r *whepReceiver) RequestKeyframe() error {
	r.mu.Lock()
	ssrc, ok, down := r.ssrc, r.haveSSRC, r.shutdown
	r.mu.Unlock()
	if !ok || down {
		return nil
	}
	return r.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}})
}

// Err reports why the stream ended, or nil after Close.
func (r *whepReceiver) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Close ends the WHEP session with a DELETE, as RFC 9725 section 4.3 asks,
// and waits for the frame channel to close.
func (r *whepReceiver) Close() error {
	r.finish(nil)
	if res := r.resource; res != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if req, err := http.NewRequestWithContext(ctx, http.MethodDelete, res, nil); err == nil {
			r.addHeaders(req)
			if resp, err := r.cfg.Client.Do(req); err == nil {
				resp.Body.Close()
			}
		}
		cancel()
	}
	<-r.closed
	return nil
}

// redactURL strips the URL from a net/http error. *url.Error prints the
// whole request URL, and a WHEP URL can carry the session's access token.
func redactURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
