// Command overlaptest reproduces a relaunch's stream overlap against a real
// device: open a second scrcpy server while the first is still streaming, then
// close the first. It exists because that overlap is the one part of the swap
// that fakes cannot exercise -- scrcpy-server deletes its own jar on exit
// (trap 17), and whether a second server can launch beside the first and then
// survive its cleanup is a property of the device, not of loupe.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	scrcpyserver "github.com/bmcreations/loupe/internal/scrcpy/server"
	"github.com/bmcreations/loupe/internal/target"
)

func count(s target.Stream, d time.Duration) (frames, keys int) {
	deadline := time.After(d)
	for {
		select {
		case f, ok := <-s.Frames():
			if !ok {
				return
			}
			if f.Config {
				continue
			}
			frames++
			if f.Key {
				keys++
			}
		case <-deadline:
			return
		}
	}
}

func main() {
	serial := os.Args[1]
	adb := target.ADB{Path: "adb"}
	devices, err := adb.Devices(context.Background())
	if err != nil {
		fmt.Println("devices:", err)
		os.Exit(1)
	}
	jar, err := scrcpyserver.Materialize("")
	if err != nil {
		fmt.Println("materialize:", err)
		os.Exit(1)
	}

	var tgt *target.ADBTarget
	for _, d := range devices {
		if d.Serial == serial {
			tgt = target.NewADBTarget(adb, d, jar, scrcpyserver.Version)
		}
	}
	if tgt == nil {
		fmt.Println("no such device:", serial)
		os.Exit(1)
	}

	opts := target.DefaultOptions()
	opts.MaxSize, opts.MaxFPS, opts.BitRate = 1080, 60, 3_000_000

	first, err := tgt.Open(context.Background(), opts)
	if err != nil {
		fmt.Println("first open:", err)
		os.Exit(1)
	}
	f, k := count(first, 3*time.Second)
	fmt.Printf("first alone:     %3d frames, %d keyframes\n", f, k)

	opts.BitRate = 800_000
	t0 := time.Now()
	second, err := tgt.Open(context.Background(), opts)
	if err != nil {
		fmt.Println("second open while the first is live:", err)
		_ = first.Close()
		os.Exit(1)
	}
	fmt.Printf("second opened in %v beside the live first\n", time.Since(t0).Round(time.Millisecond))

	done := make(chan int, 1)
	go func() { a, _ := count(first, 2*time.Second); done <- a }()
	f, k = count(second, 2*time.Second)
	fmt.Printf("during overlap:  first %3d frames, second %3d frames (%d keyframes)\n", <-done, f, k)

	_ = first.Close()
	fmt.Println("closed the first")

	f, k = count(second, 4*time.Second)
	fmt.Printf("after shutdown:  second %3d frames, %d keyframes, err=%v\n", f, k, second.Err())
	_ = second.Close()
}
