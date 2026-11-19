package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// launchd SIGKILLs loupe a few seconds after its SIGTERM. A Firebase
// release that hangs must not hold up the Revyl stop, and neither may
// outlast the budget exit gives them.
func TestExitStopsBilledDevicesTogetherWithinTheBudget(t *testing.T) {
	cli := &fakeRemoteCLI{removeHold: make(chan struct{})}
	t.Cleanup(func() { close(cli.removeHold) })
	frig := remoteServer(t, cli)
	frig.reserve(t)
	waitFor(t, func() bool { return frig.only(t).State == remoteActive })

	rrig := revylServer(t)
	rrig.file("device-start.json", revylStartJSON)
	_ = do(rrig.s, http.MethodPost, "/api/revyl", `{"platform":"android"}`)
	rrig.waitFor("the device to start", func() bool {
		l := rrig.list()
		return len(l) == 1 && l[0].State == revylActive
	})
	rrig.file("device-stop.hold", "")
	t.Cleanup(func() { rrig.remove("device-stop.hold") })

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	began := time.Now()
	done := make(chan struct{})
	go func() {
		StopBilledDevices(ctx, frig.rd, rrig.rv)
		close(done)
	}()
	rrig.waitFor("the Revyl stop to start while the Firebase release hangs", func() bool {
		return rrig.ran("device", "stop") != nil
	})
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the stops outlived their budget")
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("the stops took %v on a 500ms budget", took)
	}
	// The budget cut the remove off, so Google never confirmed it: the row
	// stays releasing for the next start. Waiting for that also keeps the
	// release's save out of the TempDir cleanup.
	waitFor(t, func() bool {
		g := frig.only(t)
		return g.State == remoteReleasing && g.ReleaseError != ""
	})
}

// Exit's stops now run while the HTTP server drains, so a reserve that
// arrived then would start after ReleaseAll looked and bill until it
// expired.
func TestAReserveIsRefusedOnceExitBegins(t *testing.T) {
	rig := remoteServer(t, &fakeRemoteCLI{})
	rig.rd.ReleaseAll(context.Background())

	rec := do(rig.s, http.MethodPost, "/api/remote", `{"project":"demo-project","codename":"tokay","api":36,"model":"Pixel 9"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("reserve during exit = %d %s", rec.Code, rec.Body)
	}
	if l := rig.rd.List(); len(l) != 0 {
		t.Errorf("reservations = %+v", l)
	}
}
