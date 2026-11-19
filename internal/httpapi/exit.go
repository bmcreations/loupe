package httpapi

import (
	"context"
	"sync"
	"time"
)

// ExitStopBudget is how long exit gives the billed-device stops, counted
// from the signal. Under launchd it binds only with a long enough
// ExitTimeOut: launchd SIGKILLs a job that long after its SIGTERM, 5 s
// unless the plist sets one. launchd/dev.bmcreations.loupe.plist sets 30 s,
// and loupe-supervisor.sh, which that plist runs, passes the SIGTERM on up
// to 2 s late and gives loupe 25 s to exit before it SIGKILLs it.
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
