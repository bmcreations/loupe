package target

import (
	"context"
)

// LiveCameraFeeder is a CameraFeeder whose camera can take a stream of
// frames without a restart. FeedCameraFrame reports handled false when the
// target is not set up for it, and the frame should be staged with SetCamera.
type LiveCameraFeeder interface {
	CameraFeeder
	// FeedCameraFrame passes frame, an encoded PNG or JPEG as the client
	// sent it, to the live camera. A held frame is a still image that stays
	// up until the next frame or StopCameraFeed; otherwise the camera shows
	// its placeholder a while after frames stop.
	FeedCameraFrame(ctx context.Context, frame []byte, hold bool) (applies CameraApplies, handled bool, err error)
	// StopCameraFeed has the live camera show its placeholder and keeps it
	// staged, so frames can resume without a restart. It reports false when
	// the target has no live camera staged.
	StopCameraFeed() bool
}
