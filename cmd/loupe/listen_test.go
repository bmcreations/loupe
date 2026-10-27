package main

import "testing"

func TestLoopbackOnly(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:18472": true,
		"127.0.0.2:18472": true,
		"[::1]:18472":     true,
		"localhost:18472": true,
		"LOCALHOST:18472": true,
		// No host is every interface.
		":8080":           false,
		":18470":          false,
		"0.0.0.0:18470":   false,
		"[::]:18470":      false,
		"100.64.0.10:80":  false,
		"mac.local:18470": false,
		"not an address":  false,
	}
	for addr, want := range cases {
		if got := loopbackOnly(addr); got != want {
			t.Errorf("loopbackOnly(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestPlainHTTPRefusal(t *testing.T) {
	cases := []struct {
		addr          string
		tls, insecure bool
		refused       bool
	}{
		{":8080", false, false, true},
		{":8080", false, true, false},
		{":8080", true, false, false},
		{"127.0.0.1:18472", false, false, false},
		{"0.0.0.0:18470", false, false, true},
	}
	for _, c := range cases {
		err := checkPlainHTTP(c.addr, c.tls, c.insecure)
		if (err != nil) != c.refused {
			t.Errorf("checkPlainHTTP(%q, tls=%v, insecure=%v) = %v, want refused=%v",
				c.addr, c.tls, c.insecure, err, c.refused)
		}
	}
}
