package ios

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSimulatorStopTreatsAlreadyShutDownAsDone(t *testing.T) {
	var calls []string
	simctl = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "shutdown" {
			return []byte("Unable to shutdown device in current state: Shutdown"), errExit
		}
		return nil, nil
	}
	t.Cleanup(func() { simctl = defaultSimctl })

	s := NewSimulator(SimInfo{UDID: "U1", Name: "swipe", State: "Booted"}, "")
	s.booted.Store(true)
	if err := s.Delete(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(calls, ";"); got != "shutdown U1;delete U1" {
		t.Errorf("calls = %q", got)
	}
	if !s.BootsOnOpen() {
		t.Error("still booted after Stop")
	}
}

var errExit = errors.New("exit status 1")
