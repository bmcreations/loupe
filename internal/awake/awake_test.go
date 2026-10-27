package awake

import (
	"sync/atomic"
	"testing"
	"time"
)

func fake() (*Keeper, *atomic.Int32) {
	var held atomic.Int32
	k := New(func() (func(), error) {
		held.Add(1)
		return func() { held.Add(-1) }, nil
	}, nil)
	return k, &held
}

func TestHoldsTakeOneAssertionAndDropItWithTheLast(t *testing.T) {
	k, held := fake()
	a := k.Hold()
	b := k.Hold()
	if held.Load() != 1 {
		t.Fatalf("two viewers took %d assertions, want 1", held.Load())
	}
	a()
	a() // a second release must not drop b's hold
	if held.Load() != 1 {
		t.Fatal("releasing one viewer twice dropped the other's hold")
	}
	b()
	if held.Load() != 0 || k.Active() {
		t.Fatal("assertion still held with no viewers")
	}
}

func TestTouchHoldsUntilItsWindowEnds(t *testing.T) {
	k, held := fake()
	k.Touch(60 * time.Millisecond)
	if held.Load() != 1 {
		t.Fatal("agent input did not take the assertion")
	}
	done := k.Hold()
	done() // a viewer leaving inside the window must not drop it
	if held.Load() != 1 {
		t.Fatal("assertion dropped inside the agent's window")
	}
	deadline := time.Now().Add(2 * time.Second)
	for held.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("assertion still held after the window ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNilKeeperIsANoOp(t *testing.T) {
	var k *Keeper
	k.Hold()()
	k.Touch(time.Second)
	if k.Active() {
		t.Fatal("nil keeper reports active")
	}
}
