package main

import (
	"testing"
	"time"
)

func TestAPlanHoldsEachPhaseUntilTheNext(t *testing.T) {
	p, err := ParsePlan("0s rate=0; 20s rate=400k delay=120ms; 110s rate=0")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	cases := []struct {
		at   time.Duration
		rate int
	}{
		{0, 0},
		{19 * time.Second, 0},
		{20 * time.Second, 400_000},
		{109 * time.Second, 400_000},
		{110 * time.Second, 0},
		{time.Hour, 0},
	}
	for _, c := range cases {
		if got := p.At(c.at).Rate; got != c.rate {
			t.Errorf("at %v: rate %d, want %d", c.at, got, c.rate)
		}
	}
	if got := p.At(20 * time.Second).Delay; got != 120*time.Millisecond {
		t.Errorf("delay at 20s is %v, want 120ms", got)
	}
	// Each phase states its whole condition. Inheriting would make a plan
	// unreadable without tracking state through it.
	if got := p.At(110 * time.Second).Delay; got != 0 {
		t.Errorf("delay at 110s is %v, want the phase not to inherit", got)
	}
}

// 400000 and 4000000 are the difference between starving the encoder and not
// shaping at all, and they differ by one keystroke.
func TestRateSuffixes(t *testing.T) {
	cases := map[string]int{
		"400k": 400_000, "400K": 400_000, "1.5M": 1_500_000,
		"1m": 1_000_000, "250000": 250_000, "0": 0,
	}
	for in, want := range cases {
		got, err := parseRate(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%q parsed as %d, want %d", in, got, want)
		}
	}
}

func TestAPlanThatCannotMeanWhatItSaysIsRejected(t *testing.T) {
	cases := map[string]string{
		"no phase at zero":     "20s rate=400k",
		"phases out of order":  "0s rate=0; 30s rate=400k; 10s rate=0",
		"two phases at once":   "0s rate=0; 20s rate=400k; 20s rate=0",
		"unknown key":          "0s bandwidth=400k",
		"not key=value":        "0s rate",
		"loss above one":       "0s loss=1.5",
		"loss below zero":      "0s loss=-0.1",
		"negative rate":        "0s rate=-400k",
		"unparseable offset":   "soon rate=400k",
		"unparseable duration": "0s delay=120",
		"empty":                "",
	}
	for name, src := range cases {
		if _, err := ParsePlan(src); err == nil {
			t.Errorf("%s: %q parsed without error", name, src)
		}
	}
}
