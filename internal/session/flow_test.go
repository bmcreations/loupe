package session

import (
	"testing"
	"time"
)

var flowT0 = time.Unix(1000, 0)

func ms(n float64) time.Duration { return time.Duration(n * float64(time.Millisecond)) }

func TestFlowAckPopsThroughPts(t *testing.T) {
	var f FlowEst
	for i := uint64(1); i <= 3; i++ {
		f.Sent(i, 100, flowT0)
	}
	f.Ack(2, flowT0.Add(ms(50)))
	s := f.Snapshot(flowT0.Add(ms(60)))
	if s.Backlog != 1 || !s.Acking {
		t.Fatalf("got backlog %d acking %v, want 1 true", s.Backlog, s.Acking)
	}
	if s.OldestMs != 60 {
		t.Fatalf("oldest %v, want 60", s.OldestMs)
	}
}

func TestFlowUnknownAckIgnored(t *testing.T) {
	var f FlowEst
	for i := uint64(1); i <= 3; i++ {
		f.Sent(i, 100, flowT0)
	}
	f.Ack(99, flowT0.Add(ms(50)))
	s := f.Snapshot(flowT0.Add(ms(60)))
	if s.Backlog != 3 || s.Acking {
		t.Fatalf("got backlog %d acking %v, want 3 false", s.Backlog, s.Acking)
	}
}

func TestFlowQueueDelayFromExcess(t *testing.T) {
	var f FlowEst
	at := flowT0
	for i, rtt := range []float64{50, 50, 150} {
		f.Sent(uint64(i+1), 100, at)
		f.Ack(uint64(i+1), at.Add(ms(rtt)))
		at = at.Add(ms(200))
	}
	s := f.Snapshot(at)
	if s.BaseMs != 50 {
		t.Fatalf("base %v, want 50", s.BaseMs)
	}
	if d := s.QueueMs - 30; d > 1e-6 || d < -1e-6 {
		t.Fatalf("queue %v, want 30", s.QueueMs)
	}
}

func TestFlowBaseExpires(t *testing.T) {
	var f FlowEst
	f.Sent(1, 100, flowT0)
	f.Ack(1, flowT0.Add(ms(50)))
	t1 := flowT0.Add(11 * time.Second)
	f.Sent(2, 100, t1)
	f.Ack(2, t1.Add(ms(80)))
	if s := f.Snapshot(t1.Add(ms(80))); s.BaseMs != 80 {
		t.Fatalf("base %v, want 80", s.BaseMs)
	}
}

func TestFlowDeliveredRate(t *testing.T) {
	var f FlowEst
	f.Sent(1, 100, flowT0)
	f.Sent(2, 125000, flowT0)
	f.Ack(1, flowT0.Add(ms(10)))
	f.Ack(2, flowT0.Add(ms(510)))
	if s := f.Snapshot(flowT0.Add(ms(510))); s.DeliveredBps != 2_000_000 {
		t.Fatalf("rate %v, want 2000000", s.DeliveredBps)
	}
}

func TestFlowDeliveredRateSmoothed(t *testing.T) {
	var f FlowEst
	at := flowT0
	send := func(pts uint64, bytes int, ackAfter float64) {
		f.Sent(pts, bytes, at)
		f.Ack(pts, at.Add(ms(ackAfter)))
	}
	send(1, 0, 1) // opens the window at t0+1ms
	at = flowT0.Add(ms(501))
	send(2, 125000, 0) // 2 Mbps window sets the rate
	at = flowT0.Add(ms(1001))
	send(3, 62500, 0) // 1 Mbps window: 0.6*2 + 0.4*1
	if s := f.Snapshot(at); s.DeliveredBps < 1_599_000 || s.DeliveredBps > 1_601_000 {
		t.Fatalf("rate %v, want 1.6 Mbps", s.DeliveredBps)
	}
}

func TestFlowResetClears(t *testing.T) {
	var f FlowEst
	f.Sent(1, 100, flowT0)
	f.Sent(2, 100, flowT0)
	f.Ack(1, flowT0.Add(ms(20)))
	f.Reset()
	f.Ack(2, flowT0.Add(ms(30))) // old stream's ack must not match
	s := f.Snapshot(flowT0.Add(ms(40)))
	if s.Backlog != 0 || s.OldestMs != 0 {
		t.Fatalf("got backlog %d oldest %v after reset", s.Backlog, s.OldestMs)
	}
	f.Sent(2, 100, flowT0.Add(ms(50))) // the new stream reuses pts 2
	if s := f.Snapshot(flowT0.Add(ms(60))); s.Backlog != 1 {
		t.Fatalf("backlog %d, want 1", s.Backlog)
	}
}

func TestFlowInflightBounded(t *testing.T) {
	var f FlowEst
	for i := uint64(0); i < flowMaxInflight+50; i++ {
		f.Sent(i, 1, flowT0)
	}
	if s := f.Snapshot(flowT0); s.Backlog != flowMaxInflight {
		t.Fatalf("backlog %d, want %d", s.Backlog, flowMaxInflight)
	}
}
