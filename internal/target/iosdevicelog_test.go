package target

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// Lines as syslog_relay sends them, each followed by a NUL.
const syslogSample = "Oct 22 20:20:38 bmc-iphone wifid(WiFiPolicy)[56] <Notice>: scan done\n\x00" +
	"Oct 22 20:20:38 bmc-iphone Settings[812] <Error>: tapped \\M-b\\M^@\\M-&\n\x00" +
	"Oct 22 20:20:39 bmc-iphone wifid[56] <Notice>: event {\n" +
	"    \"FLAGS\" = 5;\n" +
	"}\n\x00" +
	"Oct 22 20:20:40 bmc-iphone Settings[812] <Notice>: path C:\\134x\n\x00"

// fakeSyslog hands Logcat one end of a pipe and writes to the other.
func fakeSyslog(t *testing.T, write func(c net.Conn)) {
	t.Helper()
	old, oldQuiet, oldCap := syslogOpen, syslogQuiet, syslogCap
	syslogQuiet, syslogCap = 50*time.Millisecond, time.Second
	t.Cleanup(func() { syslogOpen, syslogQuiet, syslogCap = old, oldQuiet, oldCap })
	syslogOpen = func(ctx context.Context, udid string) (net.Conn, error) {
		if udid != "00008140-TEST" {
			t.Errorf("opened %q, want the hardware UDID", udid)
		}
		a, b := net.Pipe()
		go write(b)
		return a, nil
	}
}

func testPhone() *IOSDevice {
	return NewIOSDevice(IOSDeviceInfo{UDID: "00008140-TEST", Name: "bmc-iphone", Transport: "localNetwork"})
}

func readLog(t *testing.T, lc *Logcat) string {
	t.Helper()
	defer lc.Close()
	b, err := io.ReadAll(lc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPhoneLogDumpStopsWhenTheBacklogGoesQuiet(t *testing.T) {
	fakeSyslog(t, func(c net.Conn) { c.Write([]byte(syslogSample)) }) // and never closes
	start := time.Now()
	lc, err := testPhone().Logcat(context.Background(), LogcatQuery{Lines: 500})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("took %v, want the quiet gap", d)
	}
	got := readLog(t, lc)
	for _, want := range []string{"scan done", "tapped …", `"FLAGS" = 5;`, `path C:\x`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x00") {
		t.Error("a NUL separator reached the output")
	}
}

func TestPhoneLogKeepsTheLastLines(t *testing.T) {
	fakeSyslog(t, func(c net.Conn) { c.Write([]byte(syslogSample)); c.Close() })
	lc, err := testPhone().Logcat(context.Background(), LogcatQuery{Lines: 2})
	if err != nil {
		t.Fatal(err)
	}
	got := readLog(t, lc)
	if want := "}\nOct 22 20:20:40 bmc-iphone Settings[812] <Notice>: path C:\\x\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPhoneLogByPIDKeepsContinuationLines(t *testing.T) {
	fakeSyslog(t, func(c net.Conn) { c.Write([]byte(syslogSample)); c.Close() })
	lc, err := testPhone().Logcat(context.Background(), LogcatQuery{PID: 56, Lines: 500})
	if err != nil {
		t.Fatal(err)
	}
	got := readLog(t, lc)
	if strings.Contains(got, "Settings") {
		t.Errorf("pid 812's lines got through:\n%s", got)
	}
	if !strings.Contains(got, "scan done") || !strings.Contains(got, `"FLAGS" = 5;`) {
		t.Errorf("pid 56's message or its continuation is missing:\n%s", got)
	}
	if lc.PID != 56 {
		t.Errorf("PID %d", lc.PID)
	}
}

func TestPhoneLogFollowStreamsAndCloses(t *testing.T) {
	more := make(chan struct{})
	fakeSyslog(t, func(c net.Conn) {
		c.Write([]byte("Oct 22 20:20:38 p a[1] <Notice>: one\n\x00"))
		<-more
		c.Write([]byte("Oct 22 20:20:39 p a[1] <Notice>: two\n\x00"))
	})
	lc, err := testPhone().Logcat(context.Background(), LogcatQuery{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 256)
	n, err := lc.Read(buf)
	if err != nil || !strings.HasSuffix(string(buf[:n]), "one\n") {
		t.Fatalf("first read %q, %v", buf[:n], err)
	}
	close(more)
	n, err = lc.Read(buf)
	if err != nil || !strings.HasSuffix(string(buf[:n]), "two\n") {
		t.Fatalf("second read %q, %v", buf[:n], err)
	}
	lc.Close()
	if _, err := lc.Read(buf); err == nil {
		t.Error("read after Close succeeded")
	}
}

func TestPhoneLogFollowEndsWithTheContext(t *testing.T) {
	fakeSyslog(t, func(c net.Conn) {})
	ctx, cancel := context.WithCancel(context.Background())
	lc, err := testPhone().Logcat(ctx, LogcatQuery{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	defer lc.Close()
	done := make(chan error, 1)
	go func() { _, err := lc.Read(make([]byte, 16)); done <- err }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("read returned no error after cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("read still blocked after cancel")
	}
}

func TestPhoneLogRefusesWhatItCannotAnswer(t *testing.T) {
	fakeSyslog(t, func(c net.Conn) { t.Error("opened the syslog for a refused query") })
	for name, q := range map[string]LogcatQuery{
		"ndjson":     {Format: LogFormatNDJSON},
		"since":      {Since: time.Now().Add(-time.Minute)},
		"package":    {Package: "com.apple.Preferences"},
		"foreground": {Foreground: true},
	} {
		if _, err := testPhone().Logcat(context.Background(), q); !errors.Is(err, ErrLogQueryUnsupported) {
			t.Errorf("%s: %v, want ErrLogQueryUnsupported", name, err)
		}
	}
}

func TestPhoneLogNeedsAReachablePhone(t *testing.T) {
	fakeSyslog(t, func(c net.Conn) { t.Error("opened the syslog of an offline phone") })
	d := NewIOSDevice(IOSDeviceInfo{UDID: "00008140-TEST", Name: "bmc-iphone"})
	if _, err := d.Logcat(context.Background(), LogcatQuery{}); err == nil {
		t.Error("an offline phone answered")
	}
}

func TestUnvis(t *testing.T) {
	for in, want := range map[string]string{
		`plain`:                  "plain",
		`\M-b\M^@\M-&`:           "…",
		`\M-p\M^_\M^X\M^@`:       "😀",
		`a\134b`:                 `a\b`,
		`tab\^Iend`:              "tab\tend",
		`\M^?`:                   "\xff",
		`keep \q and trailing \`: `keep \q and trailing \`,
		`\M-`:                    `\M-`,
	} {
		if got := string(unvis([]byte(in))); got != want {
			t.Errorf("unvis(%q) = %q, want %q", in, got, want)
		}
	}
}
