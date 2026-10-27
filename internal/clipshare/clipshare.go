// Package clipshare holds the one clipboard a server's targets and its Mac
// share, and hands each change to whatever writes it onward. The sinks
// themselves live with what they reach: the manager writes targets, and
// RunHostPasteboard the Mac's pasteboard.
package clipshare

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"sync"
	"time"
)

// ErrDisabled is a Wait while sharedClipboard is off, or one that was
// waiting when it was turned off.
var ErrDisabled = errors.New("shared clipboard is off")

// Value is one shared clipboard. At is the origin's wall clock in Unix
// milliseconds. Origin names the sink it came from (target:<id>, mac), so it
// is not written back there. Seq counts the values this server accepted.
type Value struct {
	Seq    uint64 `json:"seq"`
	ID     string `json:"id"`
	Text   string `json:"text"`
	Origin string `json:"origin"`
	At     int64  `json:"at"`
}

// Service is the shared value and its subscribers. The zero value is not
// usable; New picks the instance its ids carry.
type Service struct {
	instance string

	mu      sync.Mutex
	enabled bool
	cur     Value
	seq     uint64
	n       uint64
	// changed is closed and replaced on every accepted value and every
	// SetEnabled, so a waiter re-checks both.
	changed chan struct{}
	subs    map[*subscriber]struct{}
}

type subscriber struct {
	ch   chan Value
	done chan struct{}
}

// New returns a Service that is off.
func New() *Service {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return &Service{
		instance: hex.EncodeToString(b),
		changed:  make(chan struct{}),
		subs:     map[*subscriber]struct{}{},
	}
}

// SetEnabled turns sharing on or off. Off forgets the value, so turning it
// back on does not hand a newly opened target something copied before, and
// wakes every Wait with ErrDisabled.
func (s *Service) SetEnabled(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enabled == on {
		return
	}
	s.enabled = on
	if !on {
		s.cur = Value{}
	}
	s.wakeLocked()
}

// Enabled reports whether sharing is on.
func (s *Service) Enabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enabled
}

func (s *Service) wakeLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// changes is closed at the next accepted value or SetEnabled.
func (s *Service) changes() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

// Publish offers v as the shared clipboard and returns it as stored. It fills
// in an empty ID and At and always assigns Seq. It drops v, returning false,
// while sharing is off, when v's ID is the current one (a value of this
// server's coming back), when its text is the current text (a target echoing
// a write), or when it is older than the current one.
func (s *Service) Publish(v Value) (Value, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled {
		return Value{}, false
	}
	if s.cur.Seq != 0 {
		if v.ID != "" && v.ID == s.cur.ID {
			return Value{}, false
		}
		if v.Text == s.cur.Text {
			return Value{}, false
		}
	}
	if v.At == 0 {
		v.At = time.Now().UnixMilli()
	}
	if s.cur.Seq != 0 && v.At < s.cur.At {
		return Value{}, false
	}
	if v.ID == "" {
		s.n++
		v.ID = s.instance + "-" + strconv.FormatUint(s.n, 10)
	}
	s.seq++
	v.Seq = s.seq
	s.cur = v
	s.wakeLocked()
	for sub := range s.subs {
		offer(sub.ch, v)
	}
	return v, true
}

// offer queues v, giving up the oldest queued value when a subscriber is
// four behind: it wants the newest.
func offer(ch chan Value, v Value) {
	select {
	case ch <- v:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- v:
	default:
	}
}

// Current is the shared value, and false when there is none.
func (s *Service) Current() (Value, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur, s.cur.Seq != 0 && s.cur.Text != ""
}

// Wait returns the current value once its Seq is past after and its text is
// not empty, blocking until then. It returns ErrDisabled while sharing is
// off, and ctx's error when ctx ends first.
func (s *Service) Wait(ctx context.Context, after uint64) (Value, error) {
	for {
		s.mu.Lock()
		if !s.enabled {
			s.mu.Unlock()
			return Value{}, ErrDisabled
		}
		if s.cur.Seq > after && s.cur.Text != "" {
			v := s.cur
			s.mu.Unlock()
			return v, nil
		}
		ch := s.changed
		s.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return Value{}, ctx.Err()
		}
	}
}

// Subscribe calls fn with each accepted value, in order, on a goroutine of
// its own. cancel stops it; a call already running finishes.
func (s *Service) Subscribe(fn func(Value)) (cancel func()) {
	sub := &subscriber{ch: make(chan Value, 4), done: make(chan struct{})}
	s.mu.Lock()
	s.subs[sub] = struct{}{}
	s.mu.Unlock()
	go func() {
		for {
			select {
			case v := <-sub.ch:
				select {
				case <-sub.done:
					return
				default:
				}
				fn(v)
			case <-sub.done:
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.subs, sub)
			s.mu.Unlock()
			close(sub.done)
		})
	}
}
