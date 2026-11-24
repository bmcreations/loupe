package target

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckURL(t *testing.T) {
	for u, ok := range map[string]bool{
		"https://example.com/":                      true,
		"myapp://settings/profile?id=4":             true,
		"mailto:a@example.com":                      true,
		"myapp:settings":                            true,
		"tel:+15555550100":                          true,
		"https://example.com/caf%C3%A9":             true,
		"https://example.com/café":                  true,
		"":                                          false,
		"example.com":                               false,
		"://example.com":                            false,
		"1http://example.com":                       false,
		"https:":                                    false,
		"https://":                                  false,
		"https://example.com/a b":                   false,
		"https://example.com/\t":                    false,
		"https://example.com/\x7f":                  false,
		"https://example.com/\u0085":                false,
		"https://example.com/\xff":                  false,
		"https://" + strings.Repeat("a", MaxURLLen): false,
	} {
		err := CheckURL(u)
		if (err == nil) != ok {
			t.Errorf("CheckURL(%q) = %v, want ok=%v", u, err, ok)
		}
		if err != nil && !errors.Is(err, ErrBadURL) {
			t.Errorf("CheckURL(%q) = %v, does not wrap ErrBadURL", u, err)
		}
	}
}

func TestURLPartsAndRedactURL(t *testing.T) {
	for _, tc := range []struct{ u, scheme, host, redacted string }{
		{"HTTPS://Example.com:8443/a?t=1", "https", "example.com", "https://example.com/…"},
		{"myapp://settings/x", "myapp", "settings", "myapp://settings/…"},
		{"mailto:a@example.com", "mailto", "", "mailto:…"},
	} {
		scheme, host := URLParts(tc.u)
		if scheme != tc.scheme || !strings.EqualFold(host, tc.host) {
			t.Errorf("URLParts(%q) = %q, %q, want %q, %q", tc.u, scheme, host, tc.scheme, tc.host)
		}
		if got := RedactURL(tc.u); !strings.EqualFold(got, tc.redacted) {
			t.Errorf("RedactURL(%q) = %q, want %q", tc.u, got, tc.redacted)
		}
	}
}
