package revyl

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
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"

	"github.com/bmcreations/loupe/internal/h264"

	"github.com/bmcreations/loupe/internal/target"
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
	// Trace, when set, gets a "whep" record each second the track is
	// receiving: packets, sequence gaps, late packets, and pictures dropped
	// and why. Nil traces nothing.
	Trace func(map[string]any)

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

	frames chan target.Frame
	done   chan struct{} // closed when the receiver starts shutting down
	closed chan struct{} // closed after frames is closed
	ready  chan struct{} // closed when the first Config frame is out
	once   sync.Once
	ready1 sync.Once
	wg     sync.WaitGroup

	mu       sync.Mutex
	meta     target.Meta
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
		// RTX: offer a separate repair stream, as browsers do. Some senders
		// only retransmit on it; pion unwraps it into the main track.
		err = m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:    webrtc.MimeTypeRTX,
				ClockRate:   90000,
				SDPFmtpLine: fmt.Sprintf("apt=%d", 102+2*i),
			},
			PayloadType: webrtc.PayloadType(103 + 2*i),
		}, webrtc.RTPCodecTypeVideo)
		if err != nil {
			return nil, fmt.Errorf("whep: register rtx: %w", err)
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
		frames: make(chan target.Frame, 64),
		done:   make(chan struct{}),
		closed: make(chan struct{}),
		ready:  make(chan struct{}),
		meta:   target.Meta{Codec: "h264", Name: cfg.Name},
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
	if r.cfg.Trace != nil {
		var fb []string
		for _, l := range strings.Split(string(body), "\n") {
			l = strings.TrimSpace(l)
			if strings.HasPrefix(l, "a=rtcp-fb") || strings.HasPrefix(l, "a=rtpmap") || strings.HasPrefix(l, "a=fmtp") || strings.HasPrefix(l, "a=extmap") || strings.HasPrefix(l, "a=ssrc-group") {
				fb = append(fb, l)
			}
		}
		r.cfg.Trace(map[string]any{"kind": "whep-answer", "action": "whep-answer", "lines": fb})
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

// read assembles RTP packets into access units and emits them, after
// reorder has put them back in sequence.
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
		broken   bool
		needKey  = true
		ptsBase  uint32
		havePTS  bool
		sps, pps []byte
		ws       whepStats
	)
	ws.start(r.cfg.Trace)
	// Every dropped picture wants a keyframe; one request per
	// whepKeyRequestEvery is enough, since Revyl's encoder answers at most
	// that often (a trace showed one IDR a second whatever was asked).
	var lastKeyReq time.Time
	pli := func() {
		if time.Since(lastKeyReq) < whepKeyRequestEvery {
			return
		}
		lastKeyReq = time.Now()
		ws.pli()
		_ = r.RequestKeyframe()
	}
	emit := func() {
		defer func() { au, broken = nil, false }()
		if len(au) == 0 {
			return
		}
		ws.pics++
		if broken {
			ws.dropBroken++
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
		if key {
			ws.idr()
		}
		ws.picTS(ts)
		if sps == nil || pps == nil || (needKey && !key) {
			// Nothing decodable yet: ask for a keyframe and drop this.
			ws.dropAwaitKey++
			if needKey {
				pli()
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
			if !r.send(target.Frame{PTS: pts, Config: true, Data: cfg, Captured: time.Now()}) {
				return
			}
			r.ready1.Do(func() { close(r.ready) })
		}
		if len(pic) == 0 {
			return
		}
		needKey = false
		ws.sent++
		t0 := time.Now()
		r.send(target.Frame{PTS: pts, Key: key, Data: pic, Captured: t0})
		ws.blocked(time.Since(t0))
	}

	// process handles packets in sequence order. lost says the packets
	// before this one never came, so the picture they belonged to is
	// undecodable, and so is every picture after it until the next keyframe.
	process := func(pkt *rtp.Packet, lost bool) {
		if lost {
			au, broken, needKey = nil, true, true
			depack = codecs.H264Packet{}
			pli()
		}
		if len(au) > 0 && pkt.Timestamp != ts {
			// A new picture started without the last one's marker bit.
			emit()
		}
		ts = pkt.Timestamp
		nal, err := depack.Unmarshal(pkt.Payload)
		if err != nil {
			ws.depackErr++
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

	pkts := make(chan *rtp.Packet, 256)
	readErr := make(chan error, 1)
	go func() {
		for {
			pkt, _, err := tr.ReadRTP()
			if err != nil {
				readErr <- err
				return
			}
			select {
			case pkts <- pkt:
			case <-r.done:
				return
			}
		}
	}()

	var (
		ro    reorder
		timer = time.NewTimer(time.Hour)
	)
	timer.Stop()
	arm := func() {
		timer.Stop()
		if at, ok := ro.deadline(); ok {
			timer.Reset(time.Until(at))
		}
	}
	for {
		select {
		case err := <-readErr:
			if !errors.Is(err, io.EOF) {
				r.finish(fmt.Errorf("whep: read: %w", err))
			} else {
				r.finish(errors.New("whep: stream ended"))
			}
			return
		case <-r.done:
			return
		case pkt := <-pkts:
			ws.queued(len(pkts))
			ws.packet(pkt.SequenceNumber)
			ro.push(pkt, time.Now(), process)
			arm()
		case <-timer.C:
			ro.expire(time.Now(), process)
			arm()
		}
	}
}

// whepReorderHold is how long packets behind a gap wait for it to fill.
// A NACK resend from Revyl came back 54 to 123 ms after the gap in a trace
// of a scroll, and read sees a gap as soon as the next packet arrives, before
// NACK has had its chance. A gap that is still open after this is a loss.
const whepReorderHold = 200 * time.Millisecond

// whepKeyRequestEvery spaces the keyframe requests read sends while it
// waits for one.
const whepKeyRequestEvery = 250 * time.Millisecond

// whepReorderMax caps how many packets a gap can hold back.
const whepReorderMax = 512

// reorder puts RTP packets back in sequence order. A packet that is next in
// sequence goes through at once, with no hold, so a still screen's last
// picture is never kept waiting. Packets after a gap wait until it fills or
// until whepReorderHold after the first of them arrived, released by a
// timer rather than by the next packet, which may never come.
type reorder struct {
	want    uint16 // the next sequence number to hand on
	started bool
	pending map[uint16]heldPacket
}

type heldPacket struct {
	pkt *rtp.Packet
	at  time.Time // when it arrived
}

func (o *reorder) push(pkt *rtp.Packet, now time.Time, out func(*rtp.Packet, bool)) {
	seq := pkt.SequenceNumber
	if !o.started {
		o.started, o.want = true, seq
	}
	d := int16(seq - o.want)
	switch {
	case d < 0:
		return // a late copy of a packet already handed on, or skipped
	case d == 0:
		out(pkt, false)
		o.want++
		o.drain(out, false)
	default:
		if o.pending == nil {
			o.pending = map[uint16]heldPacket{}
		}
		if _, dup := o.pending[seq]; dup {
			return
		}
		o.pending[seq] = heldPacket{pkt, now}
		if len(o.pending) >= whepReorderMax {
			o.skip(out)
		}
	}
}

// deadline is when the current gap is given up on, if packets are held:
// whepReorderHold after the first packet past it arrived. It is timed from
// that arrival, not from when an earlier gap was given up on, so a run of
// gaps never holds a packet longer than whepReorderHold.
func (o *reorder) deadline() (time.Time, bool) {
	first, ok := o.first()
	if !ok {
		return time.Time{}, false
	}
	return o.pending[first].at.Add(whepReorderHold), true
}

// first is the lowest held sequence number.
func (o *reorder) first() (uint16, bool) {
	first, found := uint16(0), false
	for seq := range o.pending {
		if !found || int16(seq-first) < 0 {
			first, found = seq, true
		}
	}
	return first, found
}

// expire gives up on every gap whose hold has run out.
func (o *reorder) expire(now time.Time, out func(*rtp.Packet, bool)) {
	for {
		at, ok := o.deadline()
		if !ok || now.Before(at) {
			return
		}
		o.skip(out)
	}
}

// skip declares the packets before the oldest held one lost and hands on
// what follows, up to the next gap.
func (o *reorder) skip(out func(*rtp.Packet, bool)) {
	first, ok := o.first()
	if !ok {
		return
	}
	o.want = first
	o.drain(out, true)
}

// drain hands on held packets that are now next in sequence. lost marks
// the first of them as coming after a loss.
func (o *reorder) drain(out func(*rtp.Packet, bool), lost bool) {
	for {
		h, ok := o.pending[o.want]
		if !ok {
			return
		}
		delete(o.pending, o.want)
		out(h.pkt, lost)
		lost = false
		o.want++
	}
}

// send hands a frame to the consumer, or drops it if the receiver is
// shutting down. It reports whether the frame went out.
func (r *whepReceiver) send(f target.Frame) bool {
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

func (r *whepReceiver) Meta() target.Meta {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.meta
}

func (r *whepReceiver) Frames() <-chan target.Frame { return r.frames }

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

// whepStats counts what read sees, for the "whep" trace record: one a
// second while packets arrive. Only read touches it.
type whepStats struct {
	trace                               func(map[string]any)
	last                                time.Time
	hi                                  uint16
	haveHi                              bool
	holes                               map[uint16]time.Time // missing seq -> when the jump past it came
	pkts                                int
	gaps                                int   // forward jumps
	missing                             int   // seqs skipped by them
	late                                int   // packets that filled a hole
	lateMaxMs                           int64 // the latest of them
	stale                               int   // packets behind the highest seq that filled no hole
	pics                                int
	sent                                int
	dropBroken, dropAwaitKey, depackErr int
	plis                                int
	idrs                                int
	pliAt                               time.Time // first PLI since the last IDR
	keyWaitMs                           int64     // longest PLI -> IDR wait
	prevTS                              uint32
	haveTS                              bool
	tsSkips                             int   // pictures more than two 60 fps intervals after the last
	queueMax                            int   // most packets waiting for read at once
	sendMaxMs                           int64 // longest wait handing a picture on
}

func (w *whepStats) start(trace func(map[string]any)) {
	w.trace, w.last, w.holes = trace, time.Now(), map[uint16]time.Time{}
}

func (w *whepStats) packet(seq uint16) {
	if w.trace == nil {
		return
	}
	now := time.Now()
	w.pkts++
	if !w.haveHi {
		w.hi, w.haveHi = seq, true
	} else if d := int16(seq - w.hi); d > 1 {
		w.gaps++
		w.missing += int(d) - 1
		for k := uint16(1); k < uint16(d) && len(w.holes) < 1024; k++ {
			w.holes[w.hi+k] = now
		}
		w.hi = seq
	} else if d == 1 {
		w.hi = seq
	} else if at, ok := w.holes[seq]; ok {
		delete(w.holes, seq)
		w.late++
		w.lateMaxMs = max(w.lateMaxMs, now.Sub(at).Milliseconds())
	} else {
		w.stale++
	}
	if now.Sub(w.last) >= time.Second {
		w.flush(now)
	}
}

func (w *whepStats) queued(n int) { w.queueMax = max(w.queueMax, n) }

func (w *whepStats) blocked(d time.Duration) {
	w.sendMaxMs = max(w.sendMaxMs, d.Milliseconds())
}

func (w *whepStats) pli() {
	w.plis++
	if w.pliAt.IsZero() {
		w.pliAt = time.Now()
	}
}

func (w *whepStats) idr() {
	w.idrs++
	if !w.pliAt.IsZero() {
		w.keyWaitMs = max(w.keyWaitMs, time.Since(w.pliAt).Milliseconds())
		w.pliAt = time.Time{}
	}
}

func (w *whepStats) picTS(ts uint32) {
	if w.haveTS && ts-w.prevTS > 3000 && ts-w.prevTS < 1<<31 {
		w.tsSkips++
	}
	w.prevTS, w.haveTS = ts, true
}

func (w *whepStats) flush(now time.Time) {
	for seq, at := range w.holes {
		if now.Sub(at) > 2*time.Second {
			delete(w.holes, seq)
		}
	}
	w.trace(map[string]any{
		"kind": "whep", "action": "whep", "pkts": w.pkts, "gaps": w.gaps, "missing": w.missing,
		"late": w.late, "lateMaxMs": w.lateMaxMs, "stale": w.stale,
		"pics": w.pics, "sent": w.sent, "dropBroken": w.dropBroken,
		"dropAwaitKey": w.dropAwaitKey, "depackErr": w.depackErr,
		"plis": w.plis, "idrs": w.idrs, "keyWaitMs": w.keyWaitMs, "tsSkips": w.tsSkips,
		"queueMax": w.queueMax, "sendMaxMs": w.sendMaxMs,
	})
	*w = whepStats{trace: w.trace, last: now, hi: w.hi, haveHi: w.haveHi, holes: w.holes,
		pliAt: w.pliAt, prevTS: w.prevTS, haveTS: w.haveTS}
}
