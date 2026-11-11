package target

import (
	"context"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/camfeed"
)

type recordingSink struct {
	frames [][]byte
	stops  int
}

func (s *recordingSink) Send(frame []byte) error { s.frames = append(s.frames, frame); return nil }
func (s *recordingSink) Stop()                   { s.stops++ }

func withFrameSink(t *testing.T) *recordingSink {
	t.Helper()
	s := &recordingSink{}
	old := frameSink
	frameSink = s
	t.Cleanup(func() {
		frameSink = old
		handsetLiveMu.Lock()
		handsetLives = map[string]handsetLive{}
		handsetLiveMu.Unlock()
	})
	return s
}

func TestFeedCameraFrameOnlyForHandsetCamera(t *testing.T) {
	dir := withCameraDir(t)
	sink := withFrameSink(t)
	withHostCameras(t, []HostCamera{{"FaceTime HD Camera", "webcam0"}, {camfeed.Name, "webcam1"}}, nil)
	withProcesses(t, nil)

	// Nothing staged, then another Mac camera: the frame is not handled.
	e := newTestEmulator()
	if _, handled, _ := e.FeedCameraFrame(context.Background(), []byte("f")); handled {
		t.Error("handled with nothing staged")
	}
	stageHost(t, dir, "Pixel_9", "FaceTime HD Camera")
	if _, handled, _ := e.FeedCameraFrame(context.Background(), []byte("f")); handled {
		t.Error("handled with another Mac camera staged")
	}
	if len(sink.frames) != 0 {
		t.Fatalf("sink got %d frames", len(sink.frames))
	}

	// Handset Camera staged and the emulator not running: on boot.
	stageHost(t, dir, "Pixel_9", camfeed.Name)
	applies, handled, err := e.FeedCameraFrame(context.Background(), []byte("f1"))
	if !handled || err != nil || applies != CameraOnBoot {
		t.Errorf("%v %v %v; want handled, on boot", applies, handled, err)
	}
	if len(sink.frames) != 1 || string(sink.frames[0]) != "f1" {
		t.Errorf("sink got %q", sink.frames)
	}
}

func TestFeedCameraFrameLiveWhenBootedOnIt(t *testing.T) {
	dir := withCameraDir(t)
	withFrameSink(t)
	withHostCameras(t, []HostCamera{{camfeed.Name, "webcam3"}}, nil)
	withProcesses(t, []process{{args: []string{"qemu-system-aarch64", "-avd", "Pixel_9", "-camera-back", "webcam3"}}})
	stageHost(t, dir, "Pixel_9", camfeed.Name)
	applies, handled, err := newTestEmulator().FeedCameraFrame(context.Background(), []byte("f"))
	if !handled || err != nil || applies != CameraLive {
		t.Errorf("%v %v %v; want handled, live", applies, handled, err)
	}
}

// Past the TTL a frame gets the cached answer at once and the look runs in
// the background, so a frame never waits on ps and -webcam-list.
func TestStaleLiveStateRefreshesInBackground(t *testing.T) {
	dir := withCameraDir(t)
	withFrameSink(t)
	withHostCameras(t, []HostCamera{{camfeed.Name, "webcam3"}}, nil)
	withProcesses(t, []process{{args: []string{"qemu-system-aarch64", "-avd", "Pixel_9", "-camera-back", "webcam3"}}})
	stageHost(t, dir, "Pixel_9", camfeed.Name)
	handsetLiveMu.Lock()
	handsetLives["Pixel_9"] = handsetLive{at: time.Now().Add(-time.Minute), applies: CameraOnBoot}
	handsetLiveMu.Unlock()

	e := newTestEmulator()
	if applies, _, _ := e.FeedCameraFrame(context.Background(), []byte("f")); applies != CameraOnBoot {
		t.Fatalf("applies %v; want the cached on boot", applies)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		handsetLiveMu.Lock()
		got := handsetLives["Pixel_9"]
		handsetLiveMu.Unlock()
		if got.applies == CameraLive && !got.refreshing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cache is %+v; want refreshed to live", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if applies, _, _ := e.FeedCameraFrame(context.Background(), []byte("f")); applies != CameraLive {
		t.Errorf("applies %v; want live after the refresh", applies)
	}
}

// Clearing the camera or switching to another Mac camera stops the feed, so
// Handset Camera shows its placeholder instead of the last frame.
func TestLeavingHandsetCameraStopsTheFeed(t *testing.T) {
	dir := withCameraDir(t)
	sink := withFrameSink(t)
	withHostCameras(t, []HostCamera{{"FaceTime HD Camera", "webcam0"}, {camfeed.Name, "webcam1"}}, nil)
	withProcesses(t, nil)
	e := newTestEmulator()

	stageHost(t, dir, "Pixel_9", camfeed.Name)
	if _, err := e.SetHostCamera(context.Background(), "FaceTime HD Camera"); err != nil {
		t.Fatal(err)
	}
	if sink.stops != 1 {
		t.Errorf("switch: %d stops; want 1", sink.stops)
	}
	stageHost(t, dir, "Pixel_9", camfeed.Name)
	if _, err := e.ClearCamera(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sink.stops != 2 {
		t.Errorf("clear: %d stops; want 2", sink.stops)
	}
	// Re-picking Handset Camera does not stop it.
	if _, err := e.SetHostCamera(context.Background(), camfeed.Name); err != nil {
		t.Fatal(err)
	}
	if sink.stops != 2 {
		t.Errorf("re-pick: %d stops; want 2", sink.stops)
	}
}

// Stopping the feed blanks Handset Camera and leaves it staged, so the next
// frame plays live with no restart.
func TestStopCameraFeedKeepsHandsetCameraStaged(t *testing.T) {
	dir := withCameraDir(t)
	sink := withFrameSink(t)
	withHostCameras(t, []HostCamera{{"FaceTime HD Camera", "webcam0"}, {camfeed.Name, "webcam1"}}, nil)
	withProcesses(t, nil)
	e := newTestEmulator()

	if e.StopCameraFeed() || sink.stops != 0 {
		t.Errorf("nothing staged: stopped %d; want not handled", sink.stops)
	}
	stageHost(t, dir, "Pixel_9", "FaceTime HD Camera")
	if e.StopCameraFeed() || sink.stops != 0 {
		t.Errorf("another Mac camera: stopped %d; want not handled", sink.stops)
	}
	stageHost(t, dir, "Pixel_9", camfeed.Name)
	if !e.StopCameraFeed() || sink.stops != 1 {
		t.Errorf("Handset Camera: stopped %d; want handled once", sink.stops)
	}
	if got := stagedHostCamera("Pixel_9"); got != camfeed.CameraName() {
		t.Errorf("staged %q after stop; want Handset Camera kept", got)
	}
}
