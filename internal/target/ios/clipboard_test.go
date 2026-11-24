package ios

import (
	"testing"
)

func TestSimulatorEchoOfSet(t *testing.T) {
	s := &Simulator{}
	text := "pasted"
	s.pbSet.Store(&text)
	if !s.echoOfSet("pasted") {
		t.Fatal("the set's own echo was offered")
	}
	if s.echoOfSet("pasted") {
		t.Fatal("a second, real copy of the same text was swallowed")
	}
}
