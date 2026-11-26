// Command wsprobe is a WebSocket viewer for measurement: it attaches to /ws
// the way the web client does, reads frames without decoding them, and writes
// one JSON line per second.
//
//	wsprobe -for 120s -token $TOK 'wss://127.0.0.1:18466/ws?target=synthetic'
//
// It sends what the server judges a viewer on: a ping every 500 ms, a
// MsgClientStats every second and, with -acks, a MsgFrameAck per frame. It
// does not decode, so the decFps it reports is the frames it received that
// second, never a number it did not measure.
//
// Run it on the server's Mac. sentAt is the server's wall clock when it handed
// the frame to this viewer, so arrival minus sentAt is a real one-way delay
// from fan-out to arrival, server-side queueing included. Across two machines
// the clocks differ and the number means nothing.
//
// wtprobe is the WebTransport equivalent. This is a separate command rather
// than a -ws mode there because none of wtprobe's loop carries over: it has no
// pings, no client stats and no per-second output, and its reader is built on
// WebTransport streams.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

func main() {
	dur := flag.Duration("for", 30*time.Second, "how long to watch")
	token := flag.String("token", "", "the server's token, sent as Authorization: Bearer (or put token= in the URL)")
	insecure := flag.Bool("insecure", true, "skip certificate verification (the server's certificate names its tailnet host, not 127.0.0.1)")
	out := flag.String("out", "", "write the JSON lines here as well as to stdout")
	acks := flag.Bool("acks", true, "ack each frame (MsgFrameAck) as the web and native clients do; the server's flow estimate for this viewer needs it")
	ackDelay := flag.Duration("ack-delay", 4*time.Millisecond, "with -acks: arrival to ack, standing in for decode and present")
	client := flag.String("client", "web", "report shape: web adds rttNowMs, the second's median, as web/pane.js does; native sends rttMs alone, as the native client's trace rows show")
	fps := flag.Int("fps", 60, "the target's nominal frame rate, for counting frames a pts jump skipped")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: wsprobe [flags] wss://host:port/ws?target=<id>")
		os.Exit(2)
	}
	if *client != "web" && *client != "native" {
		fmt.Fprintln(os.Stderr, "wsprobe: -client is web or native")
		os.Exit(2)
	}
	u, err := viewerURL(flag.Arg(0), *acks)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wsprobe:", err)
		os.Exit(2)
	}
	w := io.Writer(os.Stdout)
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			log.Fatalln("wsprobe:", err)
		}
		defer f.Close()
		w = io.MultiWriter(os.Stdout, f)
	}
	p := &probe{
		out: json.NewEncoder(w), acks: *acks, ackDelay: *ackDelay,
		web: *client == "web", name: "wsprobe-" + *client,
		t: tracker{frameDur: time.Second / time.Duration(max(*fps, 1))},
	}
	if err := p.run(u, *token, *insecure, *dur); err != nil {
		log.Fatalln("wsprobe:", err)
	}
}

// viewerURL normalises the scheme to ws(s) and adds the query the web client
// sends: proto=1, and acks=1 when acking.
func viewerURL(raw string, acks bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("%q: want a wss:// or https:// URL", raw)
	}
	q := u.Query()
	if q.Get("target") == "" {
		return "", errors.New("the URL has no target=")
	}
	if q.Get("proto") == "" {
		q.Set("proto", "1")
	}
	if acks {
		q.Set("acks", "1")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

type probe struct {
	out      *json.Encoder
	acks     bool
	ackDelay time.Duration
	web      bool
	name     string

	start time.Time // monotonic base for ping timestamps

	mu    sync.Mutex
	t     tracker
	rtt   rttMeter
	secs  []second
	ended string
}

func (p *probe) emit(v any) {
	if err := p.out.Encode(v); err != nil {
		log.Println("wsprobe: write:", err)
	}
}

func (p *probe) run(u, token string, insecure bool, dur time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), dur+10*time.Second)
	defer cancel()
	hdr := http.Header{}
	if token != "" {
		hdr.Set("Authorization", "Bearer "+token)
	}
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}}}
	dialCtx, dialCancel := context.WithTimeout(ctx, 15*time.Second)
	c, resp, err := websocket.Dial(dialCtx, u, &websocket.DialOptions{HTTPHeader: hdr, HTTPClient: hc})
	dialCancel()
	if err != nil {
		if resp != nil {
			return fmt.Errorf("dial: %w (HTTP %s)", err, resp.Status)
		}
		return fmt.Errorf("dial: %w", err)
	}
	defer c.CloseNow()
	// The default is 32 KiB, and one keyframe is larger than that.
	c.SetReadLimit(64 << 20)
	p.start = time.Now()

	readDone := make(chan error, 1)
	go func() { readDone <- p.read(ctx, c) }()

	ping := time.NewTicker(500 * time.Millisecond)
	defer ping.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	deadline := time.NewTimer(dur)
	defer deadline.Stop()
	p.send(ctx, c, encodePing(p.mono()))

	sec := 0
	var readErr error
loop:
	for {
		select {
		case <-ping.C:
			p.send(ctx, c, encodePing(p.mono()))
		case now := <-tick.C:
			sec++
			p.closeSecond(ctx, c, now, sec)
		case <-deadline.C:
			break loop
		case readErr = <-readDone:
			break loop
		}
	}
	p.mu.Lock()
	sum := summarize(p.secs, p.t.gapMaxAll)
	sum.EndedReason = p.ended
	p.mu.Unlock()
	if readErr != nil && sum.EndedReason == "" {
		sum.EndedReason = readErr.Error()
	}
	p.emit(sum)
	c.Close(websocket.StatusNormalClosure, "")
	return nil
}

// mono is microseconds since the probe started: monotonic, as the web
// client's performance.now() is. The server only echoes it.
func (p *probe) mono() uint64 { return uint64(time.Since(p.start).Microseconds()) }

func (p *probe) send(ctx context.Context, c *websocket.Conn, b []byte) {
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := c.Write(wctx, websocket.MessageBinary, b); err != nil && ctx.Err() == nil {
		log.Println("wsprobe: send:", err)
	}
}

// closeSecond writes the second's line and reports it to the server. The
// report goes out even in a second with no frames, with decFps 0: a stalled
// viewer that went quiet would read to the server as a stale one instead.
func (p *probe) closeSecond(ctx context.Context, c *websocket.Conn, now time.Time, sec int) {
	p.mu.Lock()
	line := p.t.close(now, sec)
	nowMs, n, ok := p.rtt.take()
	line.RttMs = round1(p.rtt.ewma)
	line.Pongs = n
	if ok {
		v := round1(nowMs)
		line.RttNowMs = &v
	}
	line.SentDecFps = line.Frames
	body := clientStats(line.Frames, &p.rtt, nowMs, ok, p.web, p.name)
	p.secs = append(p.secs, line)
	p.mu.Unlock()
	p.emit(line)
	go p.send(ctx, c, body)
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }

func (p *probe) read(ctx context.Context, c *websocket.Conn) error {
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			return err
		}
		now := time.Now()
		if len(b) == 0 {
			continue
		}
		switch b[0] {
		case msgFrame:
			f, err := parseFrame(b)
			if err != nil {
				continue
			}
			p.mu.Lock()
			p.t.frameArrived(f, now)
			p.mu.Unlock()
			if p.acks && !f.Config {
				pts := f.PTS
				time.AfterFunc(p.ackDelay, func() { p.send(ctx, c, encodeAck(pts)) })
			}
		case msgPong:
			if echoed, err := parsePong(b); err == nil {
				sample := float64(p.mono()-echoed) / 1000
				p.mu.Lock()
				p.rtt.add(sample)
				p.mu.Unlock()
			}
		case msgStats:
			if s, err := parseStats(b); err == nil {
				p.mu.Lock()
				p.t.cur.lastStats = &s
				p.mu.Unlock()
			}
		case msgInit:
			var init any = string(b[1:])
			if json.Valid(b[1:]) {
				init = json.RawMessage(b[1:])
			}
			p.emit(map[string]any{"init": init, "t": now.UTC().Format(time.RFC3339Nano)})
		case msgError, msgEnded:
			reason := "ended"
			if b[0] == msgError {
				reason = "error: " + strings.TrimSpace(string(b[1:]))
			}
			p.mu.Lock()
			p.ended = reason
			p.mu.Unlock()
			return errors.New(reason)
		}
	}
}
