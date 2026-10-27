package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Profile is one link condition.
type Profile struct {
	Name     string
	Rate     int
	Delay    time.Duration
	Jitter   time.Duration
	Loss     float64
	MaxQueue time.Duration
}

// Phase is a Profile that starts at an offset from the shaper's first packet.
type Phase struct {
	At     time.Time // zero value unused; see Plan.
	Offset time.Duration
	Profile
}

// Plan is a schedule of link conditions.
//
// A schedule rather than a single setting, because backing off and recovering
// are different behaviours and only one of them can be observed on a link that
// stays bad. upWindow is 60s (15s for a simulator's live encoder), so a run
// that never recovers its link can spend any amount of time without ever
// reaching the branch that probes upward.
type Plan []Phase

// ParsePlan reads a schedule written as semicolon-separated phases:
//
//	0s rate=0; 20s rate=400k delay=120ms jitter=40ms queue=300ms; 110s rate=0
//
// rate=0 means unshaped. Keys are rate, delay, jitter, loss and queue; an
// omitted key is zero, so each phase states its whole condition rather than
// inheriting, which keeps a written plan readable without tracking state.
func ParsePlan(s string) (Plan, error) {
	var plan Plan
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		fields := strings.Fields(part)
		off, err := time.ParseDuration(fields[0])
		if err != nil {
			return nil, fmt.Errorf("phase %q: offset: %w", part, err)
		}
		ph := Phase{Offset: off, Profile: Profile{Name: part}}
		for _, kv := range fields[1:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return nil, fmt.Errorf("phase %q: %q is not key=value", part, kv)
			}
			switch k {
			case "rate":
				if ph.Rate, err = parseRate(v); err != nil {
					return nil, fmt.Errorf("phase %q: rate: %w", part, err)
				}
			case "delay":
				ph.Delay, err = time.ParseDuration(v)
			case "jitter":
				ph.Jitter, err = time.ParseDuration(v)
			case "queue":
				ph.MaxQueue, err = time.ParseDuration(v)
			case "loss":
				ph.Loss, err = strconv.ParseFloat(v, 64)
				if err == nil && (ph.Loss < 0 || ph.Loss > 1) {
					err = fmt.Errorf("%v is not between 0 and 1", ph.Loss)
				}
			default:
				err = fmt.Errorf("unknown key %q", k)
			}
			if err != nil {
				return nil, fmt.Errorf("phase %q: %s: %w", part, k, err)
			}
		}
		plan = append(plan, ph)
	}
	if len(plan) == 0 {
		return nil, fmt.Errorf("plan is empty")
	}
	if plan[0].Offset != 0 {
		return nil, fmt.Errorf("first phase starts at %v, not 0s: the link has to have a condition before the first packet", plan[0].Offset)
	}
	for i := 1; i < len(plan); i++ {
		if plan[i].Offset <= plan[i-1].Offset {
			return nil, fmt.Errorf("phase %d starts at %v, at or before the phase in front of it (%v)",
				i, plan[i].Offset, plan[i-1].Offset)
		}
	}
	return plan, nil
}

// At reports the profile in force at elapsed.
func (p Plan) At(elapsed time.Duration) Profile {
	cur := p[0].Profile
	for _, ph := range p {
		if ph.Offset > elapsed {
			break
		}
		cur = ph.Profile
	}
	return cur
}

// parseRate reads a bits-per-second figure, with k and M suffixes because a
// plan is written by hand and 400000 is easy to mistype as 4000000 -- which is
// the difference between starving the encoder and not shaping at all.
func parseRate(s string) (int, error) {
	mult := 1.0
	switch {
	case strings.HasSuffix(s, "k"), strings.HasSuffix(s, "K"):
		mult, s = 1e3, s[:len(s)-1]
	case strings.HasSuffix(s, "m"), strings.HasSuffix(s, "M"):
		mult, s = 1e6, s[:len(s)-1]
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	if f < 0 {
		return 0, fmt.Errorf("%v is negative", f)
	}
	return int(f * mult), nil
}
