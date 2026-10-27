package target

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/bmcreations/loupe/internal/h264"
)

// Synthetic is a target that produces a 60fps H.264 test pattern from ffmpeg
// instead of a real device.
//
// It exists to isolate the riskiest assumption in the whole system: that an
// iPhone can decode and present this stream at a steady 60fps in Safari with
// touch latency low enough to feel live. That question has nothing to do with
// adb, scrcpy, or USB, so it should be answerable without any of them. If the
// synthetic pattern stutters on the phone, no amount of device-side work will
// fix it.
//
// It accepts no input, so every input method returns ErrNoControl.
type Synthetic struct {
	// FFmpegPath is the ffmpeg binary. Empty means "ffmpeg" on PATH.
	FFmpegPath string
	// Width and Height default to a portrait phone shape.
	Width, Height int
}

func (s *Synthetic) ID() string    { return "synthetic" }
func (s *Synthetic) Kind() Kind    { return KindSynthetic }
func (s *Synthetic) Label() string { return "Synthetic 60fps test pattern" }

// Describe reports the fixed, made-up identity of the test pattern: there is
// no OS or device version to ask for, so the fields that would name one are
// left empty rather than invented.
func (s *Synthetic) Describe(ctx context.Context) (Description, error) {
	return Description{Form: "pattern", OS: "synthetic"}, nil
}

func (s *Synthetic) Open(ctx context.Context, opts Options) (Stream, error) {
	bin := s.FFmpegPath
	if bin == "" {
		bin = "ffmpeg"
	}
	w, h := s.Width, s.Height
	if w == 0 || h == 0 {
		w, h = 720, 1560
	}
	fps := opts.MaxFPS
	if fps == 0 {
		fps = 60
	}
	bitrate := opts.BitRate
	if bitrate == 0 {
		bitrate = 8_000_000
	}

	args := []string{
		"-hide_banner", "-loglevel", "error",
		// -re paces output at wall-clock rate. Without it ffmpeg emits as fast
		// as it can encode and the client sees a burst, not a 60fps stream.
		"-re",
		"-f", "lavfi",
		"-i", fmt.Sprintf("testsrc2=size=%dx%d:rate=%d", w, h, fps),
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-tune", "zerolatency",
		// Baseline, no B-frames: the decode path with the widest hardware
		// support on iOS and the lowest inherent latency.
		"-profile:v", "baseline",
		"-pix_fmt", "yuv420p",
		// A keyframe every second bounds how long a joining client waits for a
		// decodable picture.
		"-g", strconv.Itoa(fps),
		"-keyint_min", strconv.Itoa(fps),
		"-sc_threshold", "0",
		// Single slice per picture: lower latency, and one NAL per frame.
		"-x264-params", "sliced-threads=0",
		"-b:v", strconv.Itoa(bitrate),
		"-maxrate", strconv.Itoa(bitrate),
		"-bufsize", strconv.Itoa(bitrate / 2),
		"-f", "h264", "-",
	}

	cmd := exec.Command(bin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg: %w (install it, or run with -target=device)", err)
	}
	go func() {
		out, _ := io.ReadAll(stderr)
		if len(out) > 0 {
			fmt.Printf("[ffmpeg] %s\n", out)
		}
	}()

	st := &syntheticStream{
		cmd:    cmd,
		meta:   Meta{Codec: "h264", Width: w, Height: h, Name: "Synthetic test pattern"},
		frames: make(chan Frame, 64),
		closed: make(chan struct{}),
		fps:    fps,
	}
	go st.pump(stdout)
	return st, nil
}

type syntheticStream struct {
	cmd    *exec.Cmd
	meta   Meta
	frames chan Frame
	closed chan struct{}
	fps    int

	closeOnce sync.Once
	errMu     sync.Mutex
	err       error
}

func (s *syntheticStream) Meta() Meta           { return s.meta }
func (s *syntheticStream) Frames() <-chan Frame { return s.frames }
func (s *syntheticStream) Touch(Touch) error    { return ErrNoControl }

// RequestKeyframe is a no-op: the pattern already emits a keyframe every
// second, so a viewer waiting to resynchronize never waits long.
func (s *syntheticStream) RequestKeyframe() error { return nil }
func (s *syntheticStream) Scroll(Scroll) error    { return ErrNoControl }
func (s *syntheticStream) Key(Key) error          { return ErrNoControl }

func (s *syntheticStream) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

func (s *syntheticStream) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
			go func() { _ = s.cmd.Wait() }()
		}
	})
	return nil
}

// pump reassembles ffmpeg's raw Annex B output into access units.
//
// ffmpeg writes a continuous byte stream with no framing, so picture
// boundaries have to be recovered from the bitstream. This is not merely
// splitting on start codes: x264 at ultrafast with zerolatency emits several
// slices per picture, each in its own NAL, and treating each as a frame would
// report two to three times the true frame rate. Since measuring the true
// frame rate is the entire reason this target exists, the boundary is read
// from first_mb_in_slice instead.
//
// Parameter sets are emitted as their own config frame, matching what scrcpy
// does, so the rest of the pipeline treats both targets identically.
func (s *syntheticStream) pump(r io.Reader) {
	defer close(s.frames)
	defer s.Close()

	br := bufio.NewReaderSize(r, 1<<20)
	frameDur := time.Second / time.Duration(s.fps)

	var (
		paramSets []byte // SPS/PPS accumulating toward a config frame
		au        []byte // the access unit under construction
		auHasVCL  bool
		auIsKey   bool
		frameIdx  int64
	)

	flush := func() {
		if !auHasVCL {
			return
		}
		data := make([]byte, len(au))
		copy(data, au)
		s.emit(Frame{
			PTS:      time.Duration(frameIdx) * frameDur,
			Key:      auIsKey,
			Data:     data,
			Captured: time.Now(),
		})
		frameIdx++
		au = au[:0]
		auHasVCL = false
		auIsKey = false
	}

	scanner := newNALScanner(br)
	for {
		nal, err := scanner.Next()
		if err != nil {
			flush()
			if err != io.EOF {
				s.errMu.Lock()
				s.err = err
				s.errMu.Unlock()
			}
			return
		}

		t := h264.Type(nal)
		isVCL := h264.IsVCL(nal)

		// A new picture's first slice, or any non-VCL NAL arriving after slices,
		// closes the access unit in progress.
		if auHasVCL && (!isVCL || h264.StartsNewPicture(nal)) {
			flush()
		}

		switch {
		case t == h264.NALSPS || t == h264.NALPPS:
			paramSets = appendAnnexB(paramSets, nal)
			// Emit once the set is complete. PPS always follows SPS.
			if t == h264.NALPPS {
				cfg := make([]byte, len(paramSets))
				copy(cfg, paramSets)
				s.emit(Frame{Config: true, Data: cfg, Captured: time.Now()})
				paramSets = paramSets[:0]
			}
		case isVCL:
			au = appendAnnexB(au, nal)
			auHasVCL = true
			if t == h264.NALIDR {
				auIsKey = true
			}
		default:
			// SEI, AUD and anything else prefixes the picture they precede.
			au = appendAnnexB(au, nal)
		}
	}
}

// appendAnnexB appends a NAL to buf with a four-byte start code.
func appendAnnexB(buf, nal []byte) []byte {
	buf = append(buf, 0, 0, 0, 1)
	return append(buf, nal...)
}

func (s *syntheticStream) emit(f Frame) {
	select {
	case s.frames <- f:
	case <-s.closed:
	}
}

// nalScanner splits an Annex B byte stream into NAL unit payloads.
type nalScanner struct {
	r   *bufio.Reader
	buf []byte
	// pendingStartCode records that Next already consumed the marker that
	// terminated the previous unit, so the next call must not scan for one.
	pendingStartCode bool
}

func newNALScanner(r *bufio.Reader) *nalScanner {
	return &nalScanner{r: r, buf: make([]byte, 0, 1<<16)}
}

// Next returns the next NAL payload with its start code stripped. It blocks
// until a full unit is available, which it detects by finding the following
// start code.
func (s *nalScanner) Next() ([]byte, error) {
	// Consume the leading start code.
	if err := s.consumeStartCode(); err != nil {
		return nil, err
	}
	s.buf = s.buf[:0]
	zeros := 0
	for {
		b, err := s.r.ReadByte()
		if err != nil {
			if len(s.buf) > 0 {
				out := make([]byte, len(s.buf))
				copy(out, s.buf)
				return out, nil
			}
			return nil, err
		}
		if b == 0 {
			zeros++
			s.buf = append(s.buf, b)
			continue
		}
		if b == 1 && zeros >= 2 {
			// Found the next start code. Trim its zero prefix off our payload
			// and push the marker back for the following call.
			trim := zeros
			if trim > 3 {
				trim = 3
			}
			out := make([]byte, len(s.buf)-trim)
			copy(out, s.buf[:len(s.buf)-trim])
			s.pendingStartCode = true
			return out, nil
		}
		zeros = 0
		s.buf = append(s.buf, b)
	}
}

func (s *nalScanner) consumeStartCode() error {
	if s.pendingStartCode {
		s.pendingStartCode = false
		return nil
	}
	// Scan forward to the first start code in the stream.
	zeros := 0
	for {
		b, err := s.r.ReadByte()
		if err != nil {
			return err
		}
		if b == 0 {
			zeros++
			continue
		}
		if b == 1 && zeros >= 2 {
			return nil
		}
		zeros = 0
	}
}
