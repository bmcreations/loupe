package record

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/bmcreations/loupe/internal/mp4"
)

var (
	ErrNoFFmpeg     = errors.New("record: ffmpeg not found")
	ErrRangeTooLong = errors.New("record: sheet range over 10s")
	ErrBadRange     = errors.New("record: sheet range ends before it starts")
	ErrNoFrames     = errors.New("record: no frames in range")
)

const (
	// sheetMax is the most frames one contact sheet holds.
	sheetMax = 60
	// sheetSpan is the longest range one sheet covers.
	sheetSpan = 10 * time.Second
	// timescale is the track timescale internal/mp4 writes, in ticks a
	// second; mp4.Samples durations are in it.
	timescale = 90000
)

// FFmpeg finds ffmpeg: PATH, then /opt/homebrew/bin and /usr/local/bin,
// which launchd's PATH reaches neither of.
func FFmpeg() (string, error) {
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p, nil
	}
	for _, p := range []string{"/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg"} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", ErrNoFFmpeg
}

func ticksToDuration(t uint64) time.Duration {
	return time.Duration(t) * time.Second / timescale
}

// SheetPlan picks at most 60 samples between from and to and returns their indices relative to the keyframe ffmpeg seeks to, that
// keyframe's time, and each pick's PTS for X-Loupe-Frames. A sample is in
// range when its PTS is within [from, to], both ends included.
//
// With fps > 0 the candidates are the first sample at or after each step of
// 1/fps from from, without repeats; otherwise every sample in range. More
// than 60 candidates are then sampled evenly down to 60.
func SheetPlan(s mp4.Samples, from, to time.Duration, fps float64) (seek time.Duration, picks []int, pts []time.Duration, err error) {
	if to < from {
		return 0, nil, nil, ErrBadRange
	}
	if to-from > sheetSpan {
		return 0, nil, nil, ErrRangeTooLong
	}
	starts := make([]time.Duration, len(s.Durations))
	var ticks uint64
	for i, d := range s.Durations {
		starts[i] = ticksToDuration(ticks)
		ticks += uint64(d)
	}
	first, last := -1, -1
	for i, p := range starts {
		if p < from {
			continue
		}
		if p > to {
			break
		}
		if first < 0 {
			first = i
		}
		last = i
	}
	if first < 0 {
		return 0, nil, nil, ErrNoFrames
	}

	// The last keyframe at or before from; nil Keys means every sample is
	// one. ffmpeg's select counts frames from where it seeks.
	key := 0
	if s.Keys == nil {
		key = first
		for key+1 < len(starts) && starts[key+1] <= from {
			key++
		}
	} else {
		for _, k := range s.Keys {
			i := int(k) - 1
			if i >= 0 && i < len(starts) && starts[i] <= from {
				key = i
			}
		}
	}

	cands := make([]int, 0, last-first+1)
	if fps > 0 {
		next := first
		for k := 0; ; k++ {
			t := from + time.Duration(float64(k)*float64(time.Second)/fps)
			if t > to {
				break
			}
			for next <= last && starts[next] < t {
				next++
			}
			if next > last {
				break
			}
			if len(cands) == 0 || cands[len(cands)-1] != next {
				cands = append(cands, next)
			}
		}
	} else {
		for i := first; i <= last; i++ {
			cands = append(cands, i)
		}
	}
	n := len(cands)
	m := min(n, sheetMax)
	for j := 0; j < m; j++ {
		c := 0
		if m > 1 {
			c = (j*(n-1) + (m-1)/2) / (m - 1)
		}
		i := cands[c]
		picks = append(picks, i-key)
		pts = append(pts, starts[i])
	}
	return starts[key], picks, pts, nil
}

// SheetArgs builds ffmpeg's argv: -ss <seek> -i <path>, a select filter of
// eq(n\,i) terms, scale=270:-2, a tile of min(8, n) columns that emits after
// exactly n frames, -frames:v 1 -f image2pipe -c:v mjpeg -.
//
// nb_frames matters: without it a tile with empty cells is only emitted at
// the end of input, so ffmpeg would decode the rest of the file.
func SheetArgs(path string, seek time.Duration, picks []int) []string {
	terms := make([]string, len(picks))
	for i, p := range picks {
		terms[i] = fmt.Sprintf(`eq(n\,%d)`, p)
	}
	n := max(len(picks), 1)
	cols := min(8, n)
	rows := (n + cols - 1) / cols
	filter := fmt.Sprintf("select=%s,scale=270:-2,tile=%dx%d:nb_frames=%d",
		strings.Join(terms, "+"), cols, rows, n)
	return []string{
		"-v", "error",
		"-ss", fmt.Sprintf("%.6f", seek.Seconds()),
		"-i", path,
		"-vf", filter,
		"-frames:v", "1",
		"-f", "image2pipe",
		"-c:v", "mjpeg",
		"-",
	}
}
