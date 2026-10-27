package main

import (
	"context"
	"fmt"

	"github.com/bmcreations/loupe/internal/trace"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/qlog"
	"github.com/quic-go/quic-go/qlogwriter"
)

// quicTracer records the few QUIC events that tell you whether a handshake
// reached the server at all.
//
// A WebTransport session that fails below HTTP leaves no `wt-attempt`, because
// that only fires once a request reaches the handler. Without this, "the client
// never sent a packet" and "the handshake arrived and was rejected" are the
// same empty trace.
//
// quic-go exposes this as a qlog sink rather than a set of callbacks, so every
// event arrives through one RecordEvent. Only the four low-frequency ones are
// forwarded; the per-packet events fire thousands of times a second and would
// bury the trace.
func quicTracer(tr *trace.Writer) func(context.Context, bool, quic.ConnectionID) qlogwriter.Trace {
	if tr == nil {
		return nil
	}
	return func(_ context.Context, _ bool, connID quic.ConnectionID) qlogwriter.Trace {
		return &quicTrace{tr: tr, conn: connID.String()}
	}
}

type quicTrace struct {
	tr   *trace.Writer
	conn string
}

func (t *quicTrace) AddProducer() qlogwriter.Recorder { return t }

// SupportsSchemas gates which events quic-go bothers to build. Only QUIC's own
// schema is wanted; anything else would be discarded in RecordEvent anyway.
func (t *quicTrace) SupportsSchemas(schema string) bool { return schema == qlog.EventSchema }

func (t *quicTrace) Close() error { return nil }

func (t *quicTrace) RecordEvent(ev qlogwriter.Event) {
	switch e := ev.(type) {
	case qlog.StartedConnection:
		remote := e.Remote.IPv4
		if !remote.IsValid() {
			remote = e.Remote.IPv6
		}
		t.rec("started", map[string]any{"remote": remote.String()})
	case qlog.VersionInformation:
		t.rec("version", map[string]any{"chosen": e.ChosenVersion.String()})
	case qlog.ALPNInformation:
		t.rec("alpn", map[string]any{"proto": e.ChosenALPN})
	case qlog.ConnectionClosed:
		m := map[string]any{"by": string(e.Initiator)}
		switch {
		case e.ApplicationError != nil:
			m["err"] = fmt.Sprintf("application error 0x%x", uint64(*e.ApplicationError))
		case e.ConnectionError != nil:
			m["err"] = fmt.Sprintf("transport error 0x%x", uint64(*e.ConnectionError))
		case e.Trigger != "":
			m["err"] = string(e.Trigger)
		}
		if e.Reason != "" {
			m["reason"] = e.Reason
		}
		t.rec("closed", m)
	}
}

func (t *quicTrace) rec(event string, extra map[string]any) {
	m := map[string]any{"kind": "quic", "event": event, "conn": t.conn}
	for k, v := range extra {
		m[k] = v
	}
	t.tr.Record(m)
}
