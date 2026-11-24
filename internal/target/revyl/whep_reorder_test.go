package revyl

import (
	"fmt"
	"testing"
	"time"

	"github.com/pion/rtp"
)

type reorderOut struct{ got []string }

func (o *reorderOut) out(pkt *rtp.Packet, lost bool) {
	s := fmt.Sprint(pkt.SequenceNumber)
	if lost {
		s += "!"
	}
	o.got = append(o.got, s)
}

func (o *reorderOut) take() string {
	s := fmt.Sprint(o.got)
	o.got = nil
	return s
}

func seqPkt(n uint16) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{SequenceNumber: n}}
}

func TestReorderPassesInOrderPacketsAtOnce(t *testing.T) {
	var ro reorder
	var o reorderOut
	now := time.Now()
	for _, n := range []uint16{65534, 65535, 0, 1} {
		ro.push(seqPkt(n), now, o.out)
	}
	if got := o.take(); got != "[65534 65535 0 1]" {
		t.Fatalf("got %s", got)
	}
	if _, held := ro.deadline(); held {
		t.Fatal("in-order packets held")
	}
}

func TestReorderWaitsForALateResend(t *testing.T) {
	var ro reorder
	var o reorderOut
	now := time.Now()
	ro.push(seqPkt(10), now, o.out)
	ro.push(seqPkt(12), now, o.out)
	ro.push(seqPkt(13), now.Add(10*time.Millisecond), o.out)
	if got := o.take(); got != "[10]" {
		t.Fatalf("before the resend: %s", got)
	}
	// The NACK resend of 11 comes back 120 ms later, inside the hold.
	ro.expire(now.Add(120*time.Millisecond), o.out)
	ro.push(seqPkt(11), now.Add(120*time.Millisecond), o.out)
	if got := o.take(); got != "[11 12 13]" {
		t.Fatalf("after the resend: %s, want no loss", got)
	}
	// A second copy of 11 is dropped.
	ro.push(seqPkt(11), now.Add(130*time.Millisecond), o.out)
	if got := o.take(); got != "[]" {
		t.Fatalf("duplicate: %s", got)
	}
}

func TestReorderGivesUpOnAGapAfterTheHoldWithNoMorePackets(t *testing.T) {
	var ro reorder
	var o reorderOut
	now := time.Now()
	ro.push(seqPkt(1), now, o.out)
	ro.push(seqPkt(3), now, o.out) // then the screen goes still
	at, held := ro.deadline()
	if !held || at != now.Add(whepReorderHold) {
		t.Fatalf("deadline %v %v", at, held)
	}
	ro.expire(at.Add(-time.Millisecond), o.out)
	if got := o.take(); got != "[1]" {
		t.Fatalf("before the deadline: %s", got)
	}
	ro.expire(at, o.out)
	if got := o.take(); got != "[3!]" {
		t.Fatalf("at the deadline: %s, want 3 marked after a loss", got)
	}
	// The lost packet turning up afterwards is dropped.
	ro.push(seqPkt(2), at.Add(time.Millisecond), o.out)
	ro.push(seqPkt(4), at.Add(time.Millisecond), o.out)
	if got := o.take(); got != "[4]" {
		t.Fatalf("after: %s", got)
	}
}

func TestReorderHoldsASecondGapAfterTheFirstIsGivenUp(t *testing.T) {
	var ro reorder
	var o reorderOut
	now := time.Now()
	ro.push(seqPkt(1), now, o.out)
	ro.push(seqPkt(3), now, o.out)
	ro.push(seqPkt(5), now.Add(100*time.Millisecond), o.out)
	at, _ := ro.deadline()
	ro.expire(at, o.out)
	if got := o.take(); got != "[1 3!]" {
		t.Fatalf("first gap: %s", got)
	}
	if _, held := ro.deadline(); !held {
		t.Fatal("5 not held")
	}
	ro.push(seqPkt(4), at.Add(time.Millisecond), o.out)
	if got := o.take(); got != "[4 5]" {
		t.Fatalf("second gap filled: %s", got)
	}
}

func TestReorderTimesEachGapFromItsOwnPackets(t *testing.T) {
	// Gaps one after another must not stack their holds: 5 arrived with 3,
	// so once 3's hold has run out, so has 5's.
	var ro reorder
	var o reorderOut
	now := time.Now()
	ro.push(seqPkt(1), now, o.out)
	ro.push(seqPkt(3), now, o.out)
	ro.push(seqPkt(5), now.Add(50*time.Millisecond), o.out)
	ro.push(seqPkt(7), now.Add(150*time.Millisecond), o.out)
	ro.expire(now.Add(whepReorderHold+50*time.Millisecond), o.out)
	if got := o.take(); got != "[1 3! 5!]" {
		t.Fatalf("got %s", got)
	}
	if at, held := ro.deadline(); !held || at != now.Add(150*time.Millisecond+whepReorderHold) {
		t.Fatalf("7's deadline %v %v", at, held)
	}
}

func TestReorderGivesUpWhenTooMuchIsHeld(t *testing.T) {
	var ro reorder
	var o reorderOut
	now := time.Now()
	ro.push(seqPkt(0), now, o.out)
	for n := uint16(2); n < 2+whepReorderMax; n++ {
		ro.push(seqPkt(n), now, o.out)
	}
	if got := len(o.got); got != 1+whepReorderMax {
		t.Fatalf("handed on %d, want %d", got, 1+whepReorderMax)
	}
	if o.got[1] != "2!" {
		t.Fatalf("first after the gap %s", o.got[1])
	}
}
