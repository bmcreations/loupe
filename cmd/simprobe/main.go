// Command simprobe answers whether simcap can stream an iOS simulator
// well enough to be a loupe target, before anything in the session is built
// on it.
//
// It runs simcap against one booted simulator and reads the output with the
// same scrcpy parser a device stream goes through, so a pass here means the
// bytes are already in the shape the session consumes. It reports what
// wtprobe reports for the transport: time to the first frame, keyframe
// spacing, frame rate, and the first bytes of the first frame.
//
// -animate serves a page that repaints on every animation frame and opens it
// in the simulator's Safari, because a static screen composites nothing and
// would measure only the idle floor. It also asks for a keyframe halfway
// through and closes stdin at the end, since those are the two things
// baguette's stream got wrong: it sent no frames on a static screen and
// crashed when its input closed.
//
// -setbitrate sends simcap's live bitrate command at the same halfway point,
// and the per-second kbps line shows whether the encoder followed it.
//
//	go run ./cmd/simprobe -animate <udid>
//	go run ./cmd/simprobe -animate -bitrate 4000000 -setbitrate 500000 <udid>
//
// It drives the simulator's Safari, so do not point it at a simulator someone
// is using.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"time"

	"github.com/bmcreations/loupe/internal/h264"
	"github.com/bmcreations/loupe/internal/scrcpy"
)

func main() {
	bin := flag.String("simcap", ".build/simcap", "path to the built simcap helper")
	dur := flag.Duration("dur", 10*time.Second, "how long to read")
	animate := flag.Bool("animate", false, "open a page that repaints every frame")
	fps := flag.Int("fps", 60, "simcap's frame rate cap")
	bitRate := flag.Int("bitrate", 0, "simcap's starting bitrate, 0 for its default")
	setBitRate := flag.Int("setbitrate", 0, "bitrate to switch to live, halfway through")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: simprobe [-animate] [-dur 10s] <udid>")
		os.Exit(2)
	}
	if err := run(*bin, flag.Arg(0), *dur, *animate, *fps, *bitRate, *setBitRate); err != nil {
		fmt.Fprintln(os.Stderr, "probe failed:", err)
		os.Exit(1)
	}
}

// The motion is a CSS transform, run by the compositor. A requestAnimationFrame
// loop was tried first and painted at 6-7fps in the simulator's Safari, which
// measured the page rather than the capture.
const page = `<!doctype html><meta name=viewport content="width=device-width">
<style>body{margin:0;height:100vh;overflow:hidden;background:#222}
.b{position:absolute;top:0;width:60px;height:100%;background:#fff;animation:m .5s linear infinite alternate}
.c{position:absolute;top:40%;left:40%;width:120px;height:120px;background:#f40;animation:r 1s linear infinite}
@keyframes m{from{transform:translateX(0)}to{transform:translateX(90vw)}}
@keyframes r{to{transform:rotate(360deg)}}</style>
<div class=b></div><div class=c></div>`

func run(bin, udid string, dur time.Duration, animate bool, fps, bitRate, setBitRate int) error {
	if animate {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		defer ln.Close()
		go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, page) }))
		url := "http://" + ln.Addr().String() + "/"
		if out, err := exec.Command("xcrun", "simctl", "openurl", udid, url).CombinedOutput(); err != nil {
			return fmt.Errorf("openurl: %v: %s", err, out)
		}
		fmt.Println("animating:", url)
		time.Sleep(2 * time.Second) // let Safari come up before the clock starts
	}

	args := []string{udid, "-fps", fmt.Sprint(fps)}
	if bitRate > 0 {
		args = append(args, "-bitrate", fmt.Sprint(bitRate))
	}
	cmd := exec.Command(bin, args...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return err
	}

	hdr, err := scrcpy.ReadStreamHeader(stdout)
	if err != nil {
		return fmt.Errorf("stream header: %w", err)
	}
	fmt.Printf("header       %v  %q %s %dx%d\n", ms(time.Since(start)), hdr.DeviceName, hdr.Codec, hdr.Width, hdr.Height)

	type pkt struct {
		at     time.Duration
		pts    time.Duration
		key    bool
		config bool
		size   int
	}
	var pkts []pkt
	var firstFrame []byte
	askAt := time.Duration(-1)
	deadline := start.Add(dur)
	perSec := map[int]int{}
	bytesPerSec := map[int]int{}

	for time.Now().Before(deadline) {
		ph, err := scrcpy.ReadPacketHeader(stdout)
		if err != nil {
			return fmt.Errorf("packet header after %d packets: %w", len(pkts), err)
		}
		buf := make([]byte, ph.Size)
		if _, err := io.ReadFull(stdout, buf); err != nil {
			return fmt.Errorf("packet body: %w", err)
		}
		at := time.Since(start)
		pkts = append(pkts, pkt{at, ph.PTS, ph.Key, ph.Config, len(buf)})
		if ph.Config {
			sps := h264.FindSPS(buf)
			w, h, err := h264.SPSDimensions(sps)
			fmt.Printf("config       %v  %d bytes, SPS %dx%d %v\n", ms(at), len(buf), w, h, err)
			continue
		}
		if firstFrame == nil {
			firstFrame = buf
			fmt.Printf("first frame  %v  key=%v %d bytes, starts % x\n", ms(at), ph.Key, len(buf), buf[:min(8, len(buf))])
		}
		perSec[int(at/time.Second)]++
		bytesPerSec[int(at/time.Second)] += len(buf)
		if askAt < 0 && at > dur/2 {
			askAt = at
			stdin.Write([]byte{17}) // TYPE_RESET_VIDEO
			if setBitRate > 0 {
				stdin.Write([]byte{0x80, byte(setBitRate >> 24), byte(setBitRate >> 16), byte(setBitRate >> 8), byte(setBitRate)})
				fmt.Printf("set bitrate  %v  %d bps\n", ms(at), setBitRate)
			}
		}
		if askAt >= 0 && ph.Key && at > askAt && askAt > 0 {
			fmt.Printf("key on ask   %v after the request\n", ms(at-askAt))
			askAt = 0 // reported
		}
	}

	var frames, keys, bytesTotal int
	var keyAt []time.Duration
	var gaps []time.Duration
	var prev time.Duration = -1
	for _, p := range pkts {
		if p.config {
			continue
		}
		frames++
		bytesTotal += p.size
		if p.key {
			keys++
			keyAt = append(keyAt, p.at)
		}
		if prev >= 0 {
			gaps = append(gaps, p.at-prev)
		}
		prev = p.at
	}
	slices.Sort(gaps)
	pct := func(q float64) time.Duration {
		if len(gaps) == 0 {
			return 0
		}
		return gaps[int(q*float64(len(gaps)-1))]
	}
	var keyGaps []string
	for i := 1; i < len(keyAt); i++ {
		keyGaps = append(keyGaps, ms(keyAt[i]-keyAt[i-1]))
	}
	secs, kbps := []int{}, []int{}
	for s := 0; s < int(dur/time.Second); s++ {
		secs = append(secs, perSec[s])
		kbps = append(kbps, bytesPerSec[s]*8/1000)
	}
	fmt.Printf("frames       %d in %v, %d keyframes, %.0f kbps\n", frames, dur, keys, float64(bytesTotal*8)/dur.Seconds()/1000)
	fmt.Printf("per second   %v\n", secs)
	fmt.Printf("kbps/second  %v\n", kbps)
	fmt.Printf("arrival gap  p50 %v  p90 %v  max %v\n", ms(pct(.5)), ms(pct(.9)), ms(pct(1)))
	fmt.Printf("key spacing  %v\n", keyGaps)
	fmt.Printf("annex b      %v\n", bytes.HasPrefix(firstFrame, []byte{0, 0, 0, 1}))

	// Closing stdin is how the session will say it is done.
	closed := time.Now()
	stdin.Close()
	done := make(chan error, 1)
	go func() { io.Copy(io.Discard, stdout); done <- cmd.Wait() }()
	select {
	case err := <-done:
		fmt.Printf("stdin close  exited in %v, %v\n", ms(time.Since(closed)), exitStatus(err))
	case <-time.After(3 * time.Second):
		cmd.Process.Kill()
		return fmt.Errorf("simcap did not exit within 3s of stdin closing")
	}
	return nil
}

func exitStatus(err error) string {
	if err == nil {
		return "status 0"
	}
	return err.Error()
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000) }
