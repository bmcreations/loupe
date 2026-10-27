package target

import (
	"context"
	"sync"
	"time"
)

// AVDResolver fills in DeviceInfo.AVD for each running emulator, which is how
// an emulator serial is mapped to the AVD target it belongs to.
//
// A name costs a getprop, and the device list arrives on every adb state
// change, so each is asked once per connection: the key is the serial and
// adb's transport_id together. A serial whose transport changed is a new
// emulator on a reused port, possibly a different AVD, and is asked again.
// Entries for serials adb no longer lists are dropped on the next call, which
// also covers an adb too old to report transport_id.
type AVDResolver struct {
	adb   ADB
	mu    sync.Mutex
	names map[string]string
}

func NewAVDResolver(adb ADB) *AVDResolver {
	return &AVDResolver{adb: adb, names: map[string]string{}}
}

// resolveTimeout bounds one getprop. An emulator that is still booting can
// hold a shell open; it is asked again on the next listing.
const resolveTimeout = 2 * time.Second

// Resolve returns devices with AVD set on every ready emulator it can name.
// An emulator that is not ready cannot answer getprop and keeps whatever this
// connection was last named, if anything.
func (r *AVDResolver) Resolve(ctx context.Context, devices []DeviceInfo) []DeviceInfo {
	out := make([]DeviceInfo, len(devices))
	copy(out, devices)

	r.mu.Lock()
	defer r.mu.Unlock()
	live := map[string]bool{}
	for i, d := range out {
		if !d.Emulator {
			continue
		}
		key := d.Serial + "/" + d.TransportID
		live[key] = true
		if name, ok := r.names[key]; ok {
			out[i].AVD = name
			continue
		}
		if !d.Ready() {
			continue
		}
		// getprop, then the console: an emulator a previous server process
		// paused answers only the console, and unnamed it would never attach
		// and every open would try to boot a second copy.
		name, err := nameEmulator(ctx, r.adb, d.Serial)
		if err != nil {
			continue
		}
		r.names[key] = name
		out[i].AVD = name
	}
	for key := range r.names {
		if !live[key] {
			delete(r.names, key)
		}
	}
	return out
}
