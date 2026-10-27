package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// describingTarget is a stubTarget that also implements target.Describer, so
// Targets' optional-interface branch can be exercised without a real device.
// It counts calls, which is what pins the caching requirement: describing a
// target must not cost an adb call per /api/targets request.
type describingTarget struct {
	stubTarget
	id    string
	desc  target.Description
	err   error
	calls atomic.Int32
}

func (d *describingTarget) ID() string { return d.id }

func (d *describingTarget) Describe(ctx context.Context) (target.Description, error) {
	d.calls.Add(1)
	if d.err != nil {
		return target.Description{}, d.err
	}
	return d.desc, nil
}

func TestTargetsEmbedsDescriptionFieldsAtTheTopLevel(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&describingTarget{id: "d1", desc: target.Description{
		Form: "phone", OS: "android", OSVersion: "17", APILevel: 37,
	}})

	out := m.Targets(context.Background())
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1", len(out))
	}

	b, err := json.Marshal(out[0])
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"os", "osVersion", "apiLevel", "form"} {
		if _, ok := raw[field]; !ok {
			t.Errorf("field %q missing from JSON: %s", field, b)
		}
	}
	if raw["form"] != "phone" || raw["os"] != "android" || raw["osVersion"] != "17" {
		t.Errorf("unexpected fields: %s", b)
	}
	if raw["apiLevel"].(float64) != 37 {
		t.Errorf("apiLevel = %v, want 37", raw["apiLevel"])
	}
}

func TestATargetThatCannotDescribeItselfOmitsTheFields(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&stubTarget{}) // does not implement target.Describer

	out := m.Targets(context.Background())
	b, err := json.Marshal(out[0])
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"os", "osVersion", "apiLevel", "form"} {
		if _, ok := raw[field]; ok {
			t.Errorf("field %q present with no describer: %s", field, b)
		}
	}
}

func TestAFailedDescribeOmitsTheFieldsRatherThanFailingTheList(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&describingTarget{id: "d1", err: context.DeadlineExceeded})

	out := m.Targets(context.Background())
	if len(out) != 1 || out[0].ID != "d1" {
		t.Fatalf("out = %+v, want the target still listed", out)
	}
	if out[0].OS != "" || out[0].Form != "" {
		t.Errorf("Description = %+v, want empty on error", out[0].Description)
	}
}

// TestTargetsCallsDescribeOnceEachPerRequest pins Targets' own contract: it
// calls Describe exactly once per listed target per request, no more (a
// double-collection bug) and no fewer (a target silently skipped). The
// per-server-life caching that keeps this from costing an adb call on every
// /api/targets poll lives in ADBTarget.Describe, not here -- this stub has no
// cache of its own, which is what makes it able to isolate Targets' behavior
// from Describe's; the caching guarantee itself is pinned directly against
// ADBTarget by TestDescribeIsCachedForTheTargetsLife in internal/target.
func TestTargetsCallsDescribeOnceEachPerRequest(t *testing.T) {
	d := &describingTarget{id: "d1", desc: target.Description{Form: "phone", OS: "android"}}
	m := NewManager(target.Options{})
	m.Register(d)

	const requests = 5
	for i := 0; i < requests; i++ {
		m.Targets(context.Background())
	}

	if got := d.calls.Load(); got != requests {
		t.Fatalf("Describe called %d times across %d requests, want %d (once each)", got, requests, requests)
	}
}

// withDescribeTimeout overrides the package var for the duration of a test,
// so timing assertions run against a value well below 2s without paying for
// a 2s sleep per case.
func withDescribeTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := describeTimeout
	describeTimeout = d
	t.Cleanup(func() { describeTimeout = old })
}

// blockingDescriber blocks in Describe until either its release channel is
// closed (answering successfully) or ctx is done (answering ctx.Err()),
// whichever comes first. A nil release channel blocks forever on a select,
// which is what makes it double as a "hangs indefinitely" describer: the
// only way out is ctx being cancelled or timing out.
type blockingDescriber struct {
	stubTarget
	id      string
	release chan struct{}
}

func (b *blockingDescriber) ID() string { return b.id }

func (b *blockingDescriber) Describe(ctx context.Context) (target.Description, error) {
	select {
	case <-b.release:
		return target.Description{Form: "phone", OS: "android"}, nil
	case <-ctx.Done():
		return target.Description{}, ctx.Err()
	}
}

// sleepingDescriber answers after a fixed delay, or when ctx is done,
// whichever comes first. Several of these registered together are what pins
// Targets' parallelism: N of them finish in about the time of one only if
// they actually run concurrently.
type sleepingDescriber struct {
	stubTarget
	id string
	d  time.Duration
}

func (s *sleepingDescriber) ID() string { return s.id }

func (s *sleepingDescriber) Describe(ctx context.Context) (target.Description, error) {
	select {
	case <-time.After(s.d):
		return target.Description{Form: "phone", OS: "android"}, nil
	case <-ctx.Done():
		return target.Description{}, ctx.Err()
	}
}

// TestATargetsCallReturnsAtTheTimeoutWhileItsNeighbourStillGetsDescribed pins
// two things at once: a hung Describe does not hold up the whole call past
// describeTimeout, and a neighbour target is still described despite sharing
// the call with a target that never answers. Because Targets collects the
// snapshot under m.mu and releases it before describing (see the comment on
// Targets), this also indirectly exercises that release -- if m.mu were held
// across the describe loop, nothing about this test would change, so the
// "m.mu held while describing" mutation is caught by
// TestRegisterDoesNotBlockDuringADescribe instead, not this one.
func TestATargetsCallReturnsAtTheTimeoutWhileItsNeighbourStillGetsDescribed(t *testing.T) {
	withDescribeTimeout(t, 150*time.Millisecond)

	m := NewManager(target.Options{})
	m.Register(&blockingDescriber{id: "hung"}) // nil release: blocks until ctx is done
	m.Register(&describingTarget{id: "fast", desc: target.Description{Form: "phone", OS: "android"}})

	start := time.Now()
	out := m.Targets(context.Background())
	elapsed := time.Since(start)

	if elapsed < describeTimeout {
		t.Fatalf("Targets returned in %s, want at least describeTimeout (%s)", elapsed, describeTimeout)
	}
	if elapsed > describeTimeout+500*time.Millisecond {
		t.Fatalf("Targets took %s, want close to describeTimeout (%s) -- the timeout looks like it was raised or never applied", elapsed, describeTimeout)
	}

	var fast, hung TargetInfo
	for _, ti := range out {
		switch ti.ID {
		case "fast":
			fast = ti
		case "hung":
			hung = ti
		}
	}
	if fast.OS == "" {
		t.Fatalf("neighbour target was not described: %+v", fast)
	}
	if hung.OS != "" {
		t.Fatalf("hung target was unexpectedly described: %+v", hung)
	}
}

// TestNSlowDescribesFinishInAboutTheTimeOfOne pins the parallelism directly.
// Five targets each take 200ms to describe; run serially that is 1s, run in
// parallel it is about 200ms. The threshold sits well below 1s so a
// regression to serial execution fails loudly rather than by a hair.
func TestNSlowDescribesFinishInAboutTheTimeOfOne(t *testing.T) {
	withDescribeTimeout(t, 2*time.Second)

	const n = 5
	const per = 200 * time.Millisecond
	m := NewManager(target.Options{})
	for i := 0; i < n; i++ {
		m.Register(&sleepingDescriber{id: string(rune('a' + i)), d: per})
	}

	start := time.Now()
	out := m.Targets(context.Background())
	elapsed := time.Since(start)

	if len(out) != n {
		t.Fatalf("len(out) = %d, want %d", len(out), n)
	}
	for _, ti := range out {
		if ti.OS == "" {
			t.Errorf("target %s was not described: %+v", ti.ID, ti)
		}
	}
	if elapsed >= per*n {
		t.Fatalf("Targets took %s for %d describes at %s each, want well under the serial total (%s) -- describes are not running in parallel", elapsed, n, per, per*n)
	}
}

// TestRegisterDoesNotBlockDuringADescribe pins that the snapshot-then-release
// pattern in Targets actually releases m.mu before describing, rather than
// holding it across the describe loop: if it held m.mu, Register (which also
// takes m.mu) would block for as long as the in-flight describe does.
func TestRegisterDoesNotBlockDuringADescribe(t *testing.T) {
	m := NewManager(target.Options{})
	release := make(chan struct{})
	m.Register(&blockingDescriber{id: "slow", release: release})

	targetsDone := make(chan struct{})
	go func() {
		defer close(targetsDone)
		m.Targets(context.Background())
	}()

	// Give the Targets goroutine time to reach the describe call and block on
	// it. There is no signal for "describe has started" without adding one
	// only this test would use, so this waits a comfortable margin instead;
	// a Register that is slow only because Targets has not started yet would
	// make the test flaky in the direction of a false pass, never a false
	// failure, which is the safe side to be wrong on here.
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	m.Register(&stubTarget{})
	elapsed := time.Since(start)

	close(release)
	<-targetsDone

	if elapsed > 50*time.Millisecond {
		t.Fatalf("Register took %s while a Describe was in flight, want it to return immediately (m.mu must not be held across the describe loop)", elapsed)
	}
}

// TestATargetsCallWithAnAlreadyCancelledContextReturnsImmediately pins that
// each describe's timeout context is derived from the request context passed
// to Targets, not from context.Background(). A hung describer only ever
// returns when its ctx is done; if the per-describe context ignored the
// already-cancelled request context and used context.Background() instead,
// it would keep running until describeTimeout elapses, which this sets high
// enough to make that difference unmistakable without making a correct run
// slow.
func TestATargetsCallWithAnAlreadyCancelledContextReturnsImmediately(t *testing.T) {
	withDescribeTimeout(t, time.Second)

	m := NewManager(target.Options{})
	m.Register(&blockingDescriber{id: "hung"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	out := m.Targets(ctx)
	elapsed := time.Since(start)

	if elapsed > 150*time.Millisecond {
		t.Fatalf("Targets with an already-cancelled context took %s, want near-immediate -- the per-describe timeout looks like it is built on context.Background() instead of the request context", elapsed)
	}
	if out[0].OS != "" {
		t.Fatalf("hung target was unexpectedly described: %+v", out[0])
	}
}

// TestTargetsIsSortedByID pins the fix for the drawer reordering on every
// open: Manager.targets is a map, so ranging it directly has no stable order,
// and the old code fed that order straight into the response. Registering
// out of ID order and asserting the response comes back sorted is what would
// catch a regression back to a bare `for id, t := range m.targets`.
func TestTargetsIsSortedByID(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&describingTarget{id: "zzz"})
	m.Register(&describingTarget{id: "aaa"})
	m.Register(&describingTarget{id: "mmm"})

	out := m.Targets(context.Background())
	if len(out) != 3 {
		t.Fatalf("len(out) = %d, want 3", len(out))
	}
	got := []string{out[0].ID, out[1].ID, out[2].ID}
	want := []string{"aaa", "mmm", "zzz"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Targets() order = %v, want %v", got, want)
		}
	}
}

// bootableStub is a stubTarget that also implements target.Booter, standing
// in for an Emulator without booting anything real.
type bootableStub struct {
	stubTarget
	boots bool
}

func (b *bootableStub) BootsOnOpen() bool { return b.boots }

func TestTargetsReportsBootsOnOpenForABooter(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&bootableStub{boots: true})

	out := m.Targets(context.Background())
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1", len(out))
	}
	if !out[0].BootsOnOpen {
		t.Fatalf("BootsOnOpen = false, want true for a Booter reporting true")
	}

	b, err := json.Marshal(out[0])
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if v, ok := raw["bootsOnOpen"]; !ok || v != true {
		t.Errorf("bootsOnOpen missing or not true in JSON: %s", b)
	}
}

// TestTargetsOmitsBootsOnOpenForANonBooter is the other side: a target.Target
// that is not a target.Booter at all -- a device, an already-running emulator
// adopted as ADBTarget, or synthetic -- must not report bootsOnOpen:false in
// the JSON. "Not applicable" and "answered false" are different things, and
// only omitempty on a false zero value keeps them distinguishable on the
// wire.
func TestTargetsOmitsBootsOnOpenForANonBooter(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&stubTarget{}) // does not implement target.Booter

	out := m.Targets(context.Background())
	b, err := json.Marshal(out[0])
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["bootsOnOpen"]; ok {
		t.Errorf("bootsOnOpen present in JSON for a non-Booter target: %s", b)
	}
}

// TestTargetsDoesNotWaitOutAnInProgressEmulatorBoot is the end-to-end
// regression for the review of fe297dd/58c3646, item 1: a real
// *target.Emulator, mid-boot, must not make /api/targets wait for the boot to
// finish. Emulator.Describe used to take e.mu unconditionally before checking
// anything, and ensureBooted holds e.mu for as long as WaitForBoot takes --
// which ignores ctx and can run for the full BootBudget -- so Targets' own
// describeTimeout was never reached: it waited behind e.mu instead. The
// reviewer's probe measured this directly, 2.83s against a 200ms timeout.
//
// The fake adb here reports emulator-5554 already attached ("device" state)
// but never answers sys.boot_completed with 1, so ensureBooted's WaitForBoot
// loops -- holding e.mu -- for the whole of a 1s BootBudget. Targets is
// called 100ms into that, once ensureBooted has had time to acquire e.mu and
// start polling, with describeTimeout set to 100ms. Fixed, Describe never
// touches e.mu at all while unattached, so Targets returns in a few
// milliseconds; broken, it would return only when e.mu is released, at
// around the 1s BootBudget -- an order of magnitude past describeTimeout.
func TestTargetsDoesNotWaitOutAnInProgressEmulatorBoot(t *testing.T) {
	withDescribeTimeout(t, 100*time.Millisecond)

	dir := t.TempDir()
	fakeADB := filepath.Join(dir, "adb")
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  \"devices -l\")\n" +
		"    echo \"List of devices attached\"\n" +
		"    echo \"emulator-5554 device\"\n" +
		"    ;;\n" +
		"  *\"shell getprop sys.boot_completed\"*)\n" +
		"    echo \"0\"\n" +
		"    ;;\n" +
		"esac\n"
	if err := os.WriteFile(fakeADB, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	e := target.NewEmulator(target.ADB{Path: fakeADB}, target.EmulatorConfig{
		AVD:        "test_avd",
		Port:       5554,
		BootBudget: time.Second,
	}, "", "")

	bootDone := make(chan struct{})
	go func() {
		defer close(bootDone)
		// Open calls ensureBooted first and only reaches ADBTarget if that
		// succeeds; here it always fails once BootBudget elapses (the fake
		// adb never reports boot_completed=1), so this never actually opens
		// a stream. It exists only to hold e.mu for the boot's duration, the
		// same as a real Open racing a real /api/targets request would.
		_, _ = e.Open(context.Background(), target.Options{})
	}()
	t.Cleanup(func() { <-bootDone })

	// Give the boot goroutine time to acquire e.mu and enter the polling
	// loop, so Targets below genuinely races a held lock rather than an
	// empty one -- the same margin-for-synchronization tradeoff
	// TestRegisterDoesNotBlockDuringADescribe already accepts in this file.
	time.Sleep(100 * time.Millisecond)

	m := NewManager(target.Options{})
	m.Register(e)

	start := time.Now()
	out := m.Targets(context.Background())
	elapsed := time.Since(start)

	if elapsed > describeTimeout+400*time.Millisecond {
		t.Fatalf("Targets took %s while an emulator boot was in progress, want well under describeTimeout+margin (%s) -- Describe looks like it is blocking on the boot lock again", elapsed, describeTimeout+400*time.Millisecond)
	}
	if len(out) != 1 || out[0].OS != "" {
		t.Fatalf("out = %+v, want one target with no Description (unattached mid-boot)", out)
	}
}

// TestAHeldSessionOutlivesItsLastViewer: a recorder's hold keeps a session in
// m.sessions after its last viewer leaves. Releasing the hold and calling
// Release again lets the reaper close it after the grace.
func TestAHeldSessionOutlivesItsLastViewer(t *testing.T) {
	orig := idleGrace
	idleGrace = 20 * time.Millisecond
	t.Cleanup(func() { idleGrace = orig })

	m := NewManager(target.Options{})
	m.Register(&fakeAgentTarget{id: "dev1", screenW: 1080, screenH: 2400})
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	releaseHold := sess.Hold()
	m.Release("dev1")

	// Several grace periods: the reaper would have fired by now if it ignored
	// the hold.
	time.Sleep(10 * idleGrace)
	if _, ok := m.Session("dev1"); !ok {
		t.Fatal("held session was reaped with no viewers")
	}
	select {
	case <-sess.Done():
		t.Fatal("held session was closed with no viewers")
	default:
	}

	releaseHold()
	m.Release("dev1")
	select {
	case <-sess.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("session not closed after its hold was released")
	}
	if _, ok := m.Session("dev1"); ok {
		t.Fatal("session still in m.sessions after the reaper ran")
	}
}

// TestAStaleReaperLeavesAReacquiredSessionOpen: a reaper timer that fired
// while Acquire held m.mu finds its entry gone once it gets the lock. Acquire
// has returned the session, and the viewer has not attached yet, so the
// session has no viewers; the stale timer must not close it.
func TestAStaleReaperLeavesAReacquiredSessionOpen(t *testing.T) {
	orig := idleGrace
	idleGrace = time.Hour
	t.Cleanup(func() { idleGrace = orig })

	m := NewManager(target.Options{})
	t.Cleanup(m.Shutdown)
	m.Register(&fakeAgentTarget{id: "dev1", screenW: 1080, screenH: 2400})
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	m.Release("dev1")
	m.mu.Lock()
	stale := m.reapers["dev1"]
	m.mu.Unlock()
	if stale == nil {
		t.Fatal("Release scheduled no reaper")
	}

	again, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire again: %v", err)
	}
	if again != sess {
		t.Fatal("Acquire built a new session instead of reusing the pending one")
	}

	// The stale timer's callback, as it runs once Acquire lets go of m.mu.
	m.mu.Lock()
	dead := m.reapLocked("dev1", sess, stale)
	m.mu.Unlock()
	if dead != nil {
		t.Fatal("a stale reaper chose to close the reacquired session")
	}
	if _, ok := m.Session("dev1"); !ok {
		t.Fatal("a stale reaper removed the reacquired session")
	}

	// The next Release schedules a timer of its own, which does act.
	m.Release("dev1")
	m.mu.Lock()
	fresh := m.reapers["dev1"]
	dead = m.reapLocked("dev1", sess, fresh)
	m.mu.Unlock()
	if dead != sess {
		t.Fatal("the current reaper did not choose the idle session")
	}
	fresh.Stop()
	_ = dead.Close()
}

func TestUnregisterRemovesTheTargetAndEndsItsSession(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&describingTarget{id: "sim:U1"})
	sess, err := m.Acquire(context.Background(), "sim:U1")
	if err != nil {
		t.Fatal(err)
	}
	m.Unregister("sim:U1")
	if _, ok := m.Target("sim:U1"); ok {
		t.Error("target still registered")
	}
	if _, ok := m.Session("sim:U1"); ok {
		t.Error("session still listed")
	}
	select {
	case <-sess.Done():
	case <-time.After(time.Second):
		t.Error("session not closed")
	}
	for _, ti := range m.Targets(context.Background()) {
		if ti.ID == "sim:U1" {
			t.Error("still in Targets")
		}
	}
	if _, ok := m.Info(context.Background(), "sim:U1"); ok {
		t.Error("Info still answers for it")
	}
}

// gatedTarget's Open signals that it started, then waits for release.
type gatedTarget struct {
	describingTarget
	opening chan struct{}
	release chan struct{}
}

func (g *gatedTarget) Open(ctx context.Context, opts target.Options) (target.Stream, error) {
	close(g.opening)
	<-g.release
	return g.describingTarget.Open(ctx, opts)
}

func TestUnregisterDuringAcquireLeavesNoSession(t *testing.T) {
	m := NewManager(target.Options{})
	g := &gatedTarget{describingTarget: describingTarget{id: "sim:race"}, opening: make(chan struct{}), release: make(chan struct{})}
	m.Register(g)
	errc := make(chan error, 1)
	go func() {
		_, err := m.Acquire(context.Background(), "sim:race")
		errc <- err
	}()
	<-g.opening
	m.Unregister("sim:race")
	close(g.release)
	select {
	case err := <-errc:
		if err == nil {
			t.Error("Acquire succeeded for a target unregistered while it opened")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire did not return")
	}
	if _, ok := m.Session("sim:race"); ok {
		t.Error("orphaned session published")
	}
}

func TestAShutDownTargetRefusesToOpenForAWhile(t *testing.T) {
	orig := shutDownGrace
	shutDownGrace = 50 * time.Millisecond
	t.Cleanup(func() { shutDownGrace = orig })

	m := NewManager(target.Options{})
	m.Register(&describingTarget{id: "avd:cold"})
	if _, err := m.Acquire(context.Background(), "avd:cold"); err != nil {
		t.Fatal(err)
	}
	m.FinishEnded("avd:cold", m.EndSession("avd:cold", "shutDown"))
	_, err := m.Acquire(context.Background(), "avd:cold")
	if !errors.Is(err, ErrJustShutDown) {
		t.Fatalf("Acquire right after shutDown = %v, want ErrJustShutDown", err)
	}
	if _, ok := m.Session("avd:cold"); ok {
		t.Error("refused Acquire still opened a session")
	}
	time.Sleep(2 * shutDownGrace)
	if _, err := m.Acquire(context.Background(), "avd:cold"); err != nil {
		t.Errorf("Acquire after the grace: %v", err)
	}
}

func TestClearShutDownLiftsAShutDownGuardButNotADeletedOne(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&describingTarget{id: "avd:cold"})
	m.Register(&describingTarget{id: "avd:gone"})
	for _, id := range []string{"avd:cold", "avd:gone"} {
		if _, err := m.Acquire(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	m.FinishEnded("avd:cold", m.EndSession("avd:cold", "shutDown"))
	m.FinishEnded("avd:gone", m.EndSession("avd:gone", "deleted"))
	m.ClearShutDown("avd:cold")
	m.ClearShutDown("avd:gone")
	if _, err := m.Acquire(context.Background(), "avd:cold"); err != nil {
		t.Errorf("Start inside the grace after a shut down: %v", err)
	}
	if _, err := m.Acquire(context.Background(), "avd:gone"); !errors.Is(err, ErrJustShutDown) {
		t.Errorf("a deleted guard was lifted: %v", err)
	}
}

func TestEndSessionLeavesTheTargetRegistered(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&describingTarget{id: "avd:swipe"})
	sess, err := m.Acquire(context.Background(), "avd:swipe")
	if err != nil {
		t.Fatal(err)
	}
	m.Release("avd:swipe") // arms the idle reaper, which EndSession must cancel
	m.EndSession("avd:swipe", "shutDown")
	if _, ok := m.Session("avd:swipe"); ok {
		t.Error("session survived")
	}
	if _, ok := m.Target("avd:swipe"); !ok {
		t.Error("target went with it")
	}
	select {
	case <-sess.Done():
	case <-time.After(time.Second):
		t.Error("session not closed")
	}
	if ti, ok := m.Info(context.Background(), "avd:swipe"); !ok || ti.ID != "avd:swipe" {
		t.Errorf("Info = %+v, %v", ti, ok)
	}
	m.EndSession("avd:none", "shutDown") // no session: a no-op
}

func TestShutDownDuringOpenRefusesToPublish(t *testing.T) {
	m := NewManager(target.Options{})
	g := &gatedTarget{describingTarget: describingTarget{id: "avd:midopen"}, opening: make(chan struct{}), release: make(chan struct{})}
	m.Register(g)
	errc := make(chan error, 1)
	go func() {
		_, err := m.Acquire(context.Background(), "avd:midopen")
		errc <- err
	}()
	<-g.opening
	m.EndSession("avd:midopen", "shutDown") // no session yet: only shutAt records it
	close(g.release)
	select {
	case err := <-errc:
		if !errors.Is(err, ErrJustShutDown) {
			t.Errorf("Acquire = %v, want ErrJustShutDown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire did not return")
	}
	if _, ok := m.Session("avd:midopen"); ok {
		t.Error("a session was published after the shut down")
	}
}
