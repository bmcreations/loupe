package record

import (
	"bytes"
	"errors"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/h264"
	"github.com/bmcreations/loupe/internal/mp4"
)

// evenSamples is n samples of d ticks each, with keys at the given 1-based
// sample numbers (nil: every sample is a key).
func evenSamples(n int, d uint32, keys []uint32) mp4.Samples {
	s := mp4.Samples{Keys: keys}
	for i := 0; i < n; i++ {
		s.Durations = append(s.Durations, d)
	}
	return s
}

func TestSheetPlanSamplesDownToSixty(t *testing.T) {
	s := evenSamples(600, 1503, []uint32{1})
	seek, picks, pts, err := SheetPlan(s, 0, 10*time.Second, 0)
	must(t, err)
	if seek != 0 {
		t.Fatalf("seek %v", seek)
	}
	if len(picks) != 60 || len(pts) != 60 {
		t.Fatalf("%d picks, %d pts", len(picks), len(pts))
	}
	// 599 samples start inside [0, 10s]: 598 * 16.7ms = 9.9866s.
	if picks[0] != 0 || picks[59] != 598 {
		t.Fatalf("picks run %d..%d, want 0..598", picks[0], picks[59])
	}
	if pts[0] != 0 || pts[59] != 598*16700*time.Microsecond {
		t.Fatalf("pts run %v..%v", pts[0], pts[59])
	}
	for i := 1; i < len(picks); i++ {
		if picks[i] <= picks[i-1] {
			t.Fatalf("picks not increasing at %d: %v", i, picks)
		}
	}
}

func TestSheetPlanRefusesARangeOverTenSeconds(t *testing.T) {
	s := evenSamples(1200, 1503, nil)
	if _, _, _, err := SheetPlan(s, 0, 10*time.Second+time.Millisecond, 0); !errors.Is(err, ErrRangeTooLong) {
		t.Fatalf("err = %v, want ErrRangeTooLong", err)
	}
	if _, _, _, err := SheetPlan(s, 2*time.Second, time.Second, 0); err == nil {
		t.Fatal("a backwards range was accepted")
	}
}

func TestSheetPlanKeepsEveryFrameOfAShortRange(t *testing.T) {
	// Keys at samples 1 and 31 (0-based 0 and 30). A range from 0.6s holds
	// samples 36..47 at 16.7ms, and the seek is the key at sample 30.
	s := evenSamples(100, 1503, []uint32{1, 31})
	seek, picks, pts, err := SheetPlan(s, 36*16700*time.Microsecond, 47*16700*time.Microsecond, 0)
	must(t, err)
	if seek != 30*16700*time.Microsecond {
		t.Fatalf("seek %v, want %v", seek, 30*16700*time.Microsecond)
	}
	want := []int{6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17}
	if !reflect.DeepEqual(picks, want) {
		t.Fatalf("picks %v, want %v", picks, want)
	}
	if len(pts) != 12 || pts[0] != 36*16700*time.Microsecond {
		t.Fatalf("pts %v", pts)
	}
}

func TestSheetPlanSeeksToTheLastKeyAtOrBeforeFrom(t *testing.T) {
	s := evenSamples(100, 1503, []uint32{1, 11, 21})
	seek, picks, _, err := SheetPlan(s, 10*16700*time.Microsecond, 12*16700*time.Microsecond, 0)
	must(t, err)
	if seek != 10*16700*time.Microsecond || picks[0] != 0 {
		t.Fatalf("seek %v picks %v, want the key at sample 10 and pick 0", seek, picks)
	}
}

func TestSheetPlanWithNoFramesInRange(t *testing.T) {
	s := evenSamples(10, 1503, nil)
	if _, _, _, err := SheetPlan(s, 5*time.Second, 6*time.Second, 0); !errors.Is(err, ErrNoFrames) {
		t.Fatalf("err = %v, want ErrNoFrames", err)
	}
}

func TestSheetArgs(t *testing.T) {
	got := strings.Join(SheetArgs("/r/x.mp4", 1500*time.Millisecond, []int{0, 2, 5}), " ")
	want := `-v error -ss 1.500000 -i /r/x.mp4 -vf select=eq(n\,0)+eq(n\,2)+eq(n\,5),scale=270:-2,tile=3x1:nb_frames=3 -frames:v 1 -f image2pipe -c:v mjpeg -`
	if got != want {
		t.Fatalf("args\n got %s\nwant %s", got, want)
	}
	// Twenty picks tile eight across, three rows.
	picks := make([]int, 20)
	for i := range picks {
		picks[i] = i
	}
	if args := strings.Join(SheetArgs("p", 0, picks), " "); !strings.Contains(args, "tile=8x3:nb_frames=20 ") {
		t.Fatalf("args %s", args)
	}
}

// TestSheetRunsFFmpeg encodes real frames, records them through the mp4
// writer and asks ffmpeg for the sheet.
func TestSheetRunsFFmpeg(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	raw, err := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=60",
		"-frames:v", "30", "-pix_fmt", "yuv420p", "-c:v", "libx264", "-profile:v", "baseline",
		"-g", "10", "-bf", "0", "-f", "h264", "-").Output()
	must(t, err)
	sps, ppsNAL, aus := splitAccessUnits(raw)
	if len(aus) != 30 || sps == nil || ppsNAL == nil {
		t.Fatalf("split %d access units", len(aus))
	}
	path := filepath.Join(t.TempDir(), "x.mp4")
	w, err := mp4.Create(path)
	must(t, err)
	must(t, w.SetConfig(sps, ppsNAL))
	for i, au := range aus {
		key := false
		h264.SplitAnnexB(au, func(nal []byte) { key = key || h264.Type(nal) == h264.NALIDR })
		must(t, w.WriteSample(au, time.Duration(i)*16700*time.Microsecond, key))
	}
	must(t, w.Close())

	s, err := mp4.ReadSamples(path)
	must(t, err)
	seek, picks, _, err := SheetPlan(s, 12*16700*time.Microsecond, 25*16700*time.Microsecond, 0)
	must(t, err)
	if seek != 10*16700*time.Microsecond {
		t.Fatalf("seek %v", seek)
	}
	bin, err := FFmpeg()
	must(t, err)
	var stderr bytes.Buffer
	cmd := exec.Command(bin, SheetArgs(path, seek, picks)...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, stderr.String())
	}
	if len(out) < 2 || out[0] != 0xFF || out[1] != 0xD8 {
		t.Fatalf("output starts %x, want a JPEG SOI", out[:min(4, len(out))])
	}
}

// splitAccessUnits groups an Annex B elementary stream into access units,
// as internal/mp4's ffprobe test does.
func splitAccessUnits(raw []byte) (sps, pps []byte, aus [][]byte) {
	var cur [][]byte
	var curHasVCL bool
	flush := func() {
		if len(cur) > 0 {
			aus = append(aus, annexB(cur...))
		}
		cur, curHasVCL = nil, false
	}
	h264.SplitAnnexB(raw, func(nal []byte) {
		nal = append([]byte(nil), nal...)
		switch h264.Type(nal) {
		case h264.NALSPS:
			sps = nal
		case h264.NALPPS:
			pps = nal
		}
		if h264.IsVCL(nal) && h264.StartsNewPicture(nal) && curHasVCL {
			flush()
		} else if !h264.IsVCL(nal) && curHasVCL {
			flush()
		}
		cur = append(cur, nal)
		if h264.IsVCL(nal) {
			curHasVCL = true
		}
	})
	flush()
	return sps, pps, aus
}

func TestSheetPlanFPS(t *testing.T) {
	ms := time.Millisecond
	cases := []struct {
		name     string
		n        int // 16.7ms samples
		from, to time.Duration
		fps      float64
		want     []int // pick indices
	}{
		{"10fps over 1s of 60fps", 60, 0, time.Second, 10, []int{0, 6, 12, 18, 24, 30, 36, 42, 48, 54}},
		{"fps above the recording rate keeps every frame", 60, 0, time.Second, 240, seq(60)},
		{"fps equal to the recording rate keeps every frame", 60, 0, 990 * ms, 60, seq(60)},
		{"fps 0 keeps every frame", 60, 0, time.Second, 0, seq(60)},
		{"offset from", 60, 500 * ms, time.Second, 10, []int{30, 36, 42, 48, 54}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, picks, pts, err := SheetPlan(evenSamples(c.n, 1503, []uint32{1}), c.from, c.to, c.fps)
			must(t, err)
			if !reflect.DeepEqual(picks, c.want) {
				t.Fatalf("picks %v, want %v", picks, c.want)
			}
			if len(pts) != len(picks) {
				t.Fatalf("%d pts for %d picks", len(pts), len(picks))
			}
		})
	}
}

func TestSheetPlanFPSStillCapsAtSixty(t *testing.T) {
	// 100 steps a second over 10s is 1000 candidates, but the recording
	// holds 600 samples; either way 60 is the ceiling.
	_, picks, _, err := SheetPlan(evenSamples(600, 1503, []uint32{1}), 0, 10*time.Second, 100)
	must(t, err)
	if len(picks) != 60 {
		t.Fatalf("%d picks", len(picks))
	}
}

func seq(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}
