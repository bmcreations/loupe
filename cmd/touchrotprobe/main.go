// Command touchrotprobe answers the question the rotation fix turns on: after
// a rotation, does scrcpy-server accept a touch whose screen size came from
// the new SPS, and does it reject one carrying the size from the stream
// header?
//
// Run with log_level=verbose patched into launch.go, so the server prints
// "Ignore positional event generated for size ..." for every event it drops.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/bmcreations/loupe/internal/h264"
	"github.com/bmcreations/loupe/internal/scrcpy"
	"github.com/bmcreations/loupe/internal/scrcpy/server"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: touchrotprobe <serial>")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "probe failed:", err)
		os.Exit(1)
	}
}

func run(serial string) error {
	jar, err := server.Materialize(server.DefaultDir())
	if err != nil {
		return err
	}

	sh(serial, "wm", "set-ignore-orientation-request", "true")
	defer sh(serial, "wm", "set-ignore-orientation-request", "false")
	sh(serial, "settings", "put", "system", "accelerometer_rotation", "0")
	sh(serial, "settings", "put", "system", "user_rotation", "0")
	time.Sleep(2 * time.Second)

	sess, err := scrcpy.Launch(context.Background(), scrcpy.Config{
		Serial: serial, ServerJar: jar, ServerVersion: server.Version,
		MaxSize: 1080, MaxFPS: 60, BitRate: 4_000_000,
		LogLevel: "verbose",
	})
	if err != nil {
		return err
	}
	defer sess.Close()

	h := sess.Header()
	headerW, headerH := h.Width, h.Height
	fmt.Printf("HEADER %dx%d\n", headerW, headerH)

	live := make(chan [2]int, 8)
	go func() {
		lastW, lastH := 0, 0
		for f := range sess.Frames() {
			if !f.Config {
				continue
			}
			sps := h264.FindSPS(f.Data)
			if sps == nil {
				continue
			}
			w, hh, err := h264.SPSDimensions(sps)
			if err != nil {
				fmt.Printf("  SPS parse error: %v\n", err)
				continue
			}
			if w != lastW || hh != lastH {
				lastW, lastH = w, hh
				fmt.Printf("%s SPS now %dx%d\n", ts(), w, hh)
				select {
				case live <- [2]int{w, hh}:
				default:
				}
			}
		}
	}()

	first := <-live
	fmt.Printf("portrait: header=%dx%d sps=%dx%d  agree=%v\n",
		headerW, headerH, first[0], first[1], headerW == first[0] && headerH == first[1])

	fmt.Printf("\n%s TAP portrait with header size %dx%d\n", ts(), headerW, headerH)
	tap(sess, headerW/2, headerH/2, headerW, headerH)
	time.Sleep(1500 * time.Millisecond)

	fmt.Printf("\n%s ---- rotating to landscape\n", ts())
	sh(serial, "settings", "put", "system", "user_rotation", "1")

	var now [2]int
	select {
	case now = <-live:
	case <-time.After(15 * time.Second):
		return fmt.Errorf("no new SPS after rotation")
	}
	time.Sleep(1500 * time.Millisecond)

	fmt.Printf("\n%s TAP landscape with STALE header size %dx%d  (expect a reject)\n",
		ts(), headerW, headerH)
	tap(sess, headerW/2, headerH/2, headerW, headerH)
	time.Sleep(1500 * time.Millisecond)

	fmt.Printf("\n%s TAP landscape with LIVE SPS size %dx%d  (expect no reject)\n",
		ts(), now[0], now[1])
	tap(sess, now[0]/2, now[1]/2, now[0], now[1])
	time.Sleep(2 * time.Second)

	sh(serial, "settings", "put", "system", "user_rotation", "0")
	time.Sleep(2 * time.Second)
	return nil
}

func tap(sess *scrcpy.Session, x, y, w, h int) {
	var buf [scrcpy.TouchMessageLen]byte
	for _, a := range []uint8{scrcpy.ActionDown, scrcpy.ActionUp} {
		n, err := scrcpy.EncodeTouch(buf[:], a, 1, int32(x), int32(y),
			uint16(w), uint16(h), 1.0, 0)
		if err != nil {
			fmt.Println("  encode:", err)
			return
		}
		if err := sess.SendControl(buf[:n]); err != nil {
			fmt.Println("  send:", err)
			return
		}
		time.Sleep(60 * time.Millisecond)
	}
}

func sh(serial string, args ...string) string {
	out, _ := exec.Command("adb", append([]string{"-s", serial, "shell"}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out))
}

func ts() string { return time.Now().Format("15:04:05.000") }
