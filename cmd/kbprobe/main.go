// Command kbprobe attaches loupe's UHID keyboard to a device through a live
// scrcpy session, types, and removes it, saving a screenshot at each step so
// whether the on-screen keyboard stayed down can be looked at. It types into
// whatever field has focus, so point it at a device nobody is using, with a
// text field focused.
//
//	go run ./cmd/kbprobe -text "hello" -shots /tmp/kb emulator-5556
//
// Screenshots rather than a dumpsys flag: with a hardware keyboard attached
// Gboard hides its keys but keeps a floating button up, and both
// mInputShown and mIsInputViewShown still read true on Android 16.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	scrcpyserver "github.com/bmcreations/loupe/internal/scrcpy/server"
	"github.com/bmcreations/loupe/internal/target"
)

var shots string

// state reports whether Android lists loupe's keyboard among its input
// devices, and saves a screenshot named for the step when -shots is set.
func state(serial, step string) string {
	in, _ := exec.Command("adb", "-s", serial, "shell", "dumpsys", "input").Output()
	out := fmt.Sprintf("loupeKeyboard=%v", strings.Contains(string(in), "loupe keyboard"))
	if shots != "" {
		png, err := exec.Command("adb", "-s", serial, "exec-out", "screencap", "-p").Output()
		name := filepath.Join(shots, step+".png")
		if err == nil {
			err = os.WriteFile(name, png, 0o644)
		}
		if err != nil {
			return out + " screenshot: " + err.Error()
		}
		out += " " + name
	}
	return out
}

// focusedText is the text of the focused EditText, from a uiautomator dump.
func focusedText(serial string) string {
	_ = exec.Command("adb", "-s", serial, "shell", "rm", "-f", "/sdcard/kbprobe.xml").Run()
	_ = exec.Command("adb", "-s", serial, "shell", "uiautomator", "dump", "/sdcard/kbprobe.xml").Run()
	x, _ := exec.Command("adb", "-s", serial, "exec-out", "cat", "/sdcard/kbprobe.xml").Output()
	for _, node := range strings.Split(string(x), "<node ") {
		if strings.Contains(node, `focused="true"`) && strings.Contains(node, "EditText") {
			if _, rest, ok := strings.Cut(node, `text="`); ok {
				v, _, _ := strings.Cut(rest, `"`)
				return v
			}
		}
	}
	return "(no focused field)"
}

func main() {
	text := flag.String("text", "", "text to type once the keyboard is attached")
	flag.StringVar(&shots, "shots", "", "directory to save a screenshot of each step in")
	hold := flag.Duration("hold", 3*time.Second, "how long to keep the session open after typing")
	seq := flag.String("seq", "", "steps sent back to back once the keyboard is attached, comma-separated: text:STR or key:KEYCODE; checks what reaches the field in order")
	noKB := flag.Bool("nokb", false, "leave the keyboard detached, to compare")
	gap := flag.Duration("gap", 0, "pause between -seq steps")
	flag.Parse()
	serial := flag.Arg(0)
	ctx := context.Background()

	adb := target.ADB{Path: "adb"}
	devices, err := adb.Devices(ctx)
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

	fmt.Println("before:          ", state(serial, "1-before"))
	s, err := tgt.Open(ctx, target.DefaultOptions())
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(1)
	}
	defer s.Close()
	go func() {
		for range s.Frames() {
		}
	}()

	t0 := time.Now()
	if !*noKB {
		if err := tgt.SetHardwareKeyboard(ctx, true); err != nil {
			fmt.Println("keyboard on:", err)
			os.Exit(1)
		}
	}
	time.Sleep(time.Second)
	fmt.Println("keyboard on:     ", state(serial, "2-on"), time.Since(t0).Round(time.Millisecond))

	if *seq != "" {
		for _, step := range strings.Split(*seq, ",") {
			kind, arg, _ := strings.Cut(step, ":")
			var err error
			switch kind {
			case "text":
				err = tgt.TypeText(ctx, arg)
			case "key":
				var code int
				fmt.Sscan(arg, &code)
				err = tgt.PressKey(ctx, code)
			}
			if err != nil {
				fmt.Println(step+":", err)
			}
			time.Sleep(*gap)
		}
		time.Sleep(time.Second)
		fmt.Printf("field:            %q\n", focusedText(serial))
	}

	if *text != "" {
		t1 := time.Now()
		if err := tgt.TypeText(ctx, *text); err != nil {
			fmt.Println("type:", err)
		}
		fmt.Println("typed in:        ", time.Since(t1).Round(time.Microsecond))
		time.Sleep(500 * time.Millisecond)
		fmt.Println("after typing:    ", state(serial, "3-typed"))
	}
	time.Sleep(*hold)

	if err := tgt.SetHardwareKeyboard(ctx, false); err != nil {
		fmt.Println("keyboard off:", err)
	}
	time.Sleep(time.Second)
	fmt.Println("keyboard off:    ", state(serial, "4-off"))
}
