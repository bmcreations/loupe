package target

import (
	"context"
	"sync"
	"time"

	"github.com/bmcreations/loupe/internal/camfeed"
)

// LiveCameraFeeder is a CameraFeeder whose camera can take a stream of
// frames without a restart. FeedCameraFrame reports handled false when the
// target is not set up for it, and the frame should be staged with SetCamera.
type LiveCameraFeeder interface {
	CameraFeeder
	// FeedCameraFrame passes frame, an encoded PNG or JPEG as the client
	// sent it, to the live camera.
	FeedCameraFrame(ctx context.Context, frame []byte) (applies CameraApplies, handled bool, err error)
}

// frameSink is where Handset Camera's frames go. A var so a test can catch
// them.
var frameSink interface {
	Send(frame []byte) error
	Stop()
} = camfeed.Default

// handsetLiveTTL is how long FeedCameraFrame trusts its last look at whether
// the running emulator was booted on Handset Camera. Looking runs ps and
// `emulator -webcam-list`, and a client sends several frames a second.
const handsetLiveTTL = 2 * time.Second

type handsetLive struct {
	at      time.Time
	applies CameraApplies
}

var (
	handsetLiveMu sync.Mutex
	handsetLives  = map[string]handsetLive{}
)

// FeedCameraFrame implements LiveCameraFeeder. An AVD whose staged Mac camera
// is Handset Camera takes each frame through camfeed: live when the running
// emulator was booted on it, and at the next boot otherwise, which shows the
// newest frame sent. Any other AVD is not handled.
func (e *Emulator) FeedCameraFrame(ctx context.Context, frame []byte) (CameraApplies, bool, error) {
	if stagedHostCamera(e.cfg.AVD) != camfeed.CameraName() {
		return "", false, nil
	}
	if err := frameSink.Send(frame); err != nil {
		return "", true, err
	}
	return e.handsetApplies(ctx), true, nil
}

// handsetApplies is CameraLive when the running emulator has -camera-back
// for Handset Camera's label, and CameraOnBoot otherwise.
func (e *Emulator) handsetApplies(ctx context.Context) CameraApplies {
	handsetLiveMu.Lock()
	last, ok := handsetLives[e.cfg.AVD]
	handsetLiveMu.Unlock()
	if ok && time.Since(last.at) < handsetLiveTTL {
		return last.applies
	}
	applies := CameraOnBoot
	if args := runningAVDArgs(ctx, e.cfg.AVD); args != nil {
		if cams, err := e.HostCameras(ctx); err == nil {
			if cam, ok := findHostCamera(cams, camfeed.CameraName()); ok && bootedWithHostCamera(args, cam.Label) {
				applies = CameraLive
			}
		}
	}
	handsetLiveMu.Lock()
	handsetLives[e.cfg.AVD] = handsetLive{at: time.Now(), applies: applies}
	handsetLiveMu.Unlock()
	return applies
}

// stopHandsetFeed has Handset Camera show its placeholder when this AVD had
// it staged. The feed is the Mac's, so another AVD on the camera sees the
// placeholder too.
func (e *Emulator) stopHandsetFeed() {
	if stagedHostCamera(e.cfg.AVD) == camfeed.CameraName() {
		frameSink.Stop()
	}
	handsetLiveMu.Lock()
	delete(handsetLives, e.cfg.AVD)
	handsetLiveMu.Unlock()
}
