// Command rotprobe answers one question about scrcpy-server 3.3.4: what does
// the video stream actually do when the device rotates?
//
// It launches a stream, prints the one stream header, then prints every config
// packet with the SPS inside it and a per-second frame count, while rotating
// the display underneath itself.
//
// The rotation has to be forced with set-ignore-orientation-request, because a
// portrait-locked launcher makes user_rotation a no-op and the probe then
// measures a device that never rotated.
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
		fmt.Fprintln(os.Stderr, "usage: rotprobe <serial>")
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
		Serial:        serial,
		ServerJar:     jar,
		ServerVersion: server.Version,
		MaxSize:       1080,
		MaxFPS:        60,
		BitRate:       4_000_000,
	})
	if err != nil {
		return err
	}
	defer sess.Close()

	h := sess.Header()
	fmt.Printf("HEADER name=%q codec=%s %dx%d   display=%s\n",
		h.DeviceName, h.Codec, h.Width, h.Height, rotState(serial))

	go func() {
		for _, r := range []string{"1", "0", "3"} {
			time.Sleep(5 * time.Second)
			fmt.Printf("%s ---- user_rotation=%s\n", ts(), r)
			sh(serial, "settings", "put", "system", "user_rotation", r)
			time.Sleep(2 * time.Second)
			fmt.Printf("%s ---- display now %s\n", ts(), rotState(serial))
		}
		time.Sleep(4 * time.Second)
		sess.Close()
	}()

	var n, pics int
	bucket := time.Now().Truncate(time.Second)
	var inBucket int
	for f := range sess.Frames() {
		n++
		if b := time.Now().Truncate(time.Second); b != bucket {
			if inBucket > 0 {
				fmt.Printf("%s   %d frames\n", bucket.Format("15:04:05"), inBucket)
			}
			bucket, inBucket = b, 0
		}
		inBucket++
		if f.Config {
			fmt.Printf("%s CONFIG  #%d  %d bytes  sps=%x\n", ts(), n, len(f.Data), h264.FindSPS(f.Data))
			continue
		}
		pics++
	}
	fmt.Printf("stream ended after %d frames (%d pictures): err=%v\n", n, pics, sess.Err())
	return nil
}

func sh(serial string, args ...string) string {
	out, err := exec.Command("adb", append([]string{"-s", serial, "shell"}, args...)...).CombinedOutput()
	if err != nil {
		return "ERR " + err.Error()
	}
	return strings.TrimSpace(string(out))
}

func rotState(serial string) string {
	out := sh(serial, "dumpsys", "window", "displays")
	var cur, rot string
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if cur == "" && strings.Contains(t, "cur=") {
			for _, f := range strings.Fields(t) {
				if strings.HasPrefix(f, "cur=") {
					cur = f
				}
			}
		}
		if rot == "" && strings.HasPrefix(t, "mRotation=") {
			rot = strings.Fields(t)[0]
		}
	}
	return cur + " " + rot
}

func ts() string { return time.Now().Format("15:04:05.000") }
