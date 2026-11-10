package target

import (
	"context"
	"strings"
	"sync"
)

// ModelResolver fills in DeviceInfo.Model for a device adb lists without
// one. `adb devices -l` reads the model from the connection banner, and a
// device reached through a forwarding tunnel can arrive without it: a Firebase
// remote device attached by `android device remote connect` lists as
//
//	localhost:65090        device transport_id:21
//
// which left the serial, a loopback port, as the only name it had. The model
// is still there to ask for.
//
// Like AVDResolver, each connection is asked once, keyed on the serial and
// adb's transport_id together, and entries for serials adb no longer lists
// are dropped on the next call.
type ModelResolver struct {
	adb    ADB
	mu     sync.Mutex
	models map[string]string
}

func NewModelResolver(adb ADB) *ModelResolver {
	return &ModelResolver{adb: adb, models: map[string]string{}}
}

// Resolve returns devices with Model set on every ready device that adb
// listed without one and that answered getprop. Emulators are left alone:
// they are named by their AVD.
func (r *ModelResolver) Resolve(ctx context.Context, devices []DeviceInfo) []DeviceInfo {
	out := make([]DeviceInfo, len(devices))
	copy(out, devices)

	r.mu.Lock()
	defer r.mu.Unlock()
	live := map[string]bool{}
	for i, d := range out {
		if d.Emulator || d.Model != "" {
			continue
		}
		key := d.Serial + "/" + d.TransportID
		live[key] = true
		if model, ok := r.models[key]; ok {
			out[i].Model = model
			continue
		}
		if !d.Ready() {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, resolveTimeout)
		model, err := r.adb.Shell(cctx, d.Serial, "getprop", "ro.product.model")
		cancel()
		// Spaces become underscores, as adb writes its own model field, so
		// Label reads one form.
		model = strings.ReplaceAll(strings.TrimSpace(model), " ", "_")
		if err != nil || model == "" {
			continue
		}
		r.models[key] = model
		out[i].Model = model
	}
	for key := range r.models {
		if !live[key] {
			delete(r.models, key)
		}
	}
	return out
}
