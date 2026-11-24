package target

import (
	"github.com/bmcreations/loupe/internal/scrcpy"
)

// RemoteSession is the part of a scrcpy session the remote track uses.
type RemoteSession interface {
	Frames() <-chan scrcpy.Frame
	SendControl(msg []byte) error
	Close() error
}
