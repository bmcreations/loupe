package target

import (
	"testing"
	"time"
)

// A change is offered once, and neither the device echoing a viewer's set
// nor a read's answer repeating the clipboard is offered as a copy.
func TestClipStateOffersOnlyChanges(t *testing.T) {
	c := newClipState()
	in := make(chan string)
	go c.watch(in)

	// The send returns before watch records "first"; wait for it, or watch
	// can overwrite the set below and offer the echo.
	seen := c.waitChan()
	in <- "first"
	<-seen
	c.set("from viewer")
	in <- "from viewer"
	in <- "from viewer"
	in <- "second"
	close(in)

	var got []string
	for text := range c.changes {
		got = append(got, text)
	}
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("offered %q, want [first second]", got)
	}
}

// A read waits for the next message, not the last one.
func TestClipStateReadWaitsForTheAnswer(t *testing.T) {
	c := newClipState()
	in := make(chan string, 1)
	go c.watch(in)
	wait := c.waitChan()
	in <- "answer"
	select {
	case <-wait:
	case <-time.After(time.Second):
		t.Fatal("no answer")
	}
	if c.current() != "answer" {
		t.Fatalf("current %q", c.current())
	}
}

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
