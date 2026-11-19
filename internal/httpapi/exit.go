package httpapi

import (
	"context"
	"sync"
	"time"
)

// ExitStopBudget is how long exit gives the billed-device stops, counted
// from the signal. Under launchd it does not bind today: launchd SIGKILLs
// loupe ExitTimeOut after its SIGTERM, neither login item's plist sets
// one, and the loaded job's default is 5 s. An ExitTimeOut of 30 s would
// cover this budget and the rest of shutdown, 6 s or more after it.
const ExitStopBudget = 10 * time.Second

// StopBilledDevices ends the cloud devices loupe holds, which bill by the
// minute, at exit. The Firebase releases and Revyl stops run together, so
// one provider hanging does not hold up the other. It returns when both
// are done or ctx ends.
func StopBilledDevices(ctx context.Context, rd *RemoteDevices, rv *RevylDevices) {
	var wg sync.WaitGroup
	if rd != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rd.ReleaseAll(ctx)
		}()
	}
	if rv != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rv.StopAll(ctx)
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
