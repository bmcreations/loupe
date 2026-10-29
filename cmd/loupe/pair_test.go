package main

import "testing"

func TestPairLink(t *testing.T) {
	for _, tc := range []struct {
		scheme  string
		targets []string
		want    string
	}{
		{"https", nil, "https://mac.tail1.ts.net:18456/pair#code=K7QM-4XHT-9PZA"},
		{handsetScheme, []string{"avd:Pixel_7a"}, "loupe-handset://mac.tail1.ts.net:18456/pair?target=avd%3APixel_7a#code=K7QM-4XHT-9PZA"},
	} {
		if got := pairLink(tc.scheme, "mac.tail1.ts.net", "18456", tc.targets, "K7QM-4XHT-9PZA"); got != tc.want {
			t.Errorf("pairLink(%s) = %s, want %s", tc.scheme, got, tc.want)
		}
	}
}
