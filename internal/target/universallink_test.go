package target

import (
	"context"
	"errors"
	"io"
	"log"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
)

// Flipcash's simulator build's __TEXT,__entitlements, trimmed to the keys
// around the two read, with an array and a dict that must not leak into
// them.
const flipcashEntitlements = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>application-identifier</key>
	<string>SC677X4LH6.com.flipcash.app.ios</string>
	<key>aps-environment</key>
	<string>development</string>
	<key>com.apple.developer.associated-domains</key>
	<array>
		<string>applinks:send.flipcash.com</string>
		<string>applinks:app.flipcash.com</string>
		<string>webcredentials:send.flipcash.com</string>
	</array>
	<key>nested</key>
	<dict>
		<key>application-identifier</key>
		<string>not.this.one</string>
	</dict>
	<key>keychain-access-groups</key>
	<array>
		<string>applinks:not.a.domain</string>
	</array>
</dict>
</plist>
`

func TestParseAppLinkEntitlements(t *testing.T) {
	got, err := parseAppLinkEntitlements([]byte(flipcashEntitlements))
	if err != nil {
		t.Fatal(err)
	}
	want := appLinkEntitlements{
		AppID:   "SC677X4LH6.com.flipcash.app.ios",
		Domains: []string{"send.flipcash.com", "app.flipcash.com"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseAppLinkEntitlementsNone(t *testing.T) {
	got, err := parseAppLinkEntitlements([]byte(`<plist version="1.0"><dict></dict></plist>`))
	if err != nil || got.AppID != "" || len(got.Domains) != 0 {
		t.Errorf("got %+v, %v; want nothing", got, err)
	}
}

func TestClaimsHost(t *testing.T) {
	for _, c := range []struct {
		entry, host string
		want        bool
	}{
		{"app.flipcash.com", "app.flipcash.com", true},
		{"app.flipcash.com", "APP.Flipcash.com", true},
		{"app.flipcash.com", "flipcash.com", false},
		{"app.flipcash.com?mode=developer", "app.flipcash.com", true},
		{"*.example.com", "a.example.com", true},
		{"*.example.com", "a.b.example.com", true},
		{"*.example.com", "example.com", false},
		{"*.example.com", "badexample.com", false},
		{"*.example.com?mode=managed", "a.example.com", true},
	} {
		if got := claimsHost(c.entry, c.host); got != c.want {
			t.Errorf("claimsHost(%q, %q) = %v, want %v", c.entry, c.host, got, c.want)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	for _, c := range []struct {
		pat, s string
		cs     bool
		want   bool
	}{
		{"/chat/*", "/chat/a/b", true, true}, // * crosses a slash
		{"/chat/*", "/chat/", true, true},
		{"/chat/*", "/chat", true, false},
		{"/*/metadata.json", "/x/y/metadata.json", true, true},
		{"?*", "", true, false},
		{"?*", "x", true, true},
		{"/e=?*", "/e=abc", true, true},
		{"/blog", "/Blog", true, false},
		{"/blog", "/Blog", false, true},
		{"*", "", true, true},
		{"/a?c", "/abc", true, true},
		{"/a?c", "/ac", true, false},
	} {
		if got := globMatch(c.pat, c.s, c.cs); got != c.want {
			t.Errorf("globMatch(%q, %q, %v) = %v, want %v", c.pat, c.s, c.cs, got, c.want)
		}
	}
}

// testdata/aasa-app.flipcash.com.json is what
// https://app-site-association.cdn-apple.com/a/v1/app.flipcash.com served.
func TestAASAClaimsFlipcash(t *testing.T) {
	file, err := os.ReadFile("testdata/aasa-app.flipcash.com.json")
	if err != nil {
		t.Fatal(err)
	}
	const app = "SC677X4LH6.com.flipcash.app.ios"
	for _, c := range []struct {
		url   string
		appID string
		want  bool
	}{
		{"https://app.flipcash.com/chat/7994e91b-d062-82e3-b9df-d69f9bf4e98f", app, true},
		{"https://app.flipcash.com/chat/invite#secret", app, true},
		{"https://app.flipcash.com/chat/invite", app, false},
		{"https://app.flipcash.com/chat", app, false},
		{"https://app.flipcash.com/blog", app, false},
		{"https://app.flipcash.com/Blog", app, false}, // caseSensitive false
		{"https://app.flipcash.com/", app, false},
		{"https://app.flipcash.com", app, false}, // no path is "/"
		{"https://app.flipcash.com/x/metadata.json", app, false},
		{"https://app.flipcash.com/login#/e=abc", app, true},
		{"https://app.flipcash.com/someuser", app, true}, // the trailing "/*"
		{"https://app.flipcash.com/chat/abc", "SC677X4LH6.com.other.app", false},
	} {
		u, _ := url.Parse(c.url)
		got, err := aasaClaims(file, c.appID, u)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%s for %s = %v, want %v", c.url, c.appID, got, c.want)
		}
	}
}

func TestAASAClaimsQueryAndLegacyPaths(t *testing.T) {
	const file = `{"applinks":{"apps":[],"details":[
		{"appIDs":["T.a"],"components":[
			{"/":"/q","?":{"ref":"?*"}},
			{"/":"/s","?":"x=*"}
		]},
		{"appID":"T.b","paths":["NOT /private/*","/*"]}
	]}}`
	for _, c := range []struct {
		url, appID string
		want       bool
	}{
		{"https://h/q?ref=1", "T.a", true},
		{"https://h/q?other=1", "T.a", false},
		{"https://h/q?ref=", "T.a", false},
		{"https://h/s?x=2", "T.a", true},
		{"https://h/s?y=2", "T.a", false},
		{"https://h/private/k", "T.b", false},
		{"https://h/public", "T.b", true},
	} {
		u, _ := url.Parse(c.url)
		got, err := aasaClaims([]byte(file), c.appID, u)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%s for %s = %v, want %v", c.url, c.appID, got, c.want)
		}
	}
}

// stubUniversalLinks stands in for simctl, the apps' executables and the
// network.
func stubUniversalLinks(t *testing.T, apps []simApp, ents map[string]appLinkEntitlements, aasa map[string]string) {
	t.Helper()
	oldList, oldEnts, oldAASA, oldRun := simListApps, simAppEntitlements, aasaFor, simopenRun
	t.Cleanup(func() { simListApps, simAppEntitlements, aasaFor, simopenRun = oldList, oldEnts, oldAASA, oldRun })
	simListApps = func(ctx context.Context, udid string) ([]simApp, error) { return apps, nil }
	simAppEntitlements = func(exe string) (appLinkEntitlements, error) { return ents[exe], nil }
	aasaFor = func(ctx context.Context, host string) ([]byte, error) {
		if f, ok := aasa[host]; ok {
			return []byte(f), nil
		}
		return nil, errors.New("apple-app-site-association for " + host + ": 404")
	}
}

func TestUniversalLinkApp(t *testing.T) {
	file, err := os.ReadFile("testdata/aasa-app.flipcash.com.json")
	if err != nil {
		t.Fatal(err)
	}
	stubUniversalLinks(t,
		[]simApp{{"com.example.other", "/o"}, {"com.flipcash.app.ios", "/f"}},
		map[string]appLinkEntitlements{
			"/o": {AppID: "T.com.example.other", Domains: []string{"other.com"}},
			"/f": {AppID: "SC677X4LH6.com.flipcash.app.ios", Domains: []string{"app.flipcash.com"}},
		},
		map[string]string{"app.flipcash.com": string(file)})
	for _, c := range []struct{ url, want string }{
		{"https://app.flipcash.com/chat/7994e91b", "com.flipcash.app.ios"},
		{"https://app.flipcash.com/blog", ""},
		{"https://example.com/chat/1", ""},
	} {
		u, _ := url.Parse(c.url)
		got, err := universalLinkApp(context.Background(), "u", u)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%s = %q, want %q", c.url, got, c.want)
		}
	}
}

// A helper that fails leaves the URL to simctl, and nothing it said
// carries the path.
func TestOpenUniversalLinkFallsBack(t *testing.T) {
	stubUniversalLinks(t,
		[]simApp{{"com.a", "/a"}},
		map[string]appLinkEntitlements{"/a": {AppID: "T.com.a", Domains: []string{"h.com"}}},
		map[string]string{"h.com": `{"applinks":{"details":[{"appIDs":["T.com.a"],"components":[{"/":"/*"}]}]}}`})
	const u = "https://h.com/x?token=SECRET"
	var ran []string
	simopenRun = func(ctx context.Context, udid, url, bundle string) error {
		ran = append(ran, bundle)
		return redactErr(errors.New("simopen: exit 1: open failed for "+url), url)
	}
	s := NewSimulator(SimInfo{UDID: "u"}, "")
	var logged strings.Builder
	captureLog(t, &logged)
	if bundle, ok := s.openUniversalLink(context.Background(), u); ok || bundle != "" {
		t.Errorf("got %q, %v; want a fallback", bundle, ok)
	}
	if !reflect.DeepEqual(ran, []string{"com.a"}) {
		t.Errorf("ran %v", ran)
	}
	if strings.Contains(logged.String(), "SECRET") || strings.Contains(logged.String(), "/x") {
		t.Errorf("log names the path: %s", logged.String())
	}

	simopenRun = func(ctx context.Context, udid, url, bundle string) error { return nil }
	if bundle, ok := s.openUniversalLink(context.Background(), u); !ok || bundle != "com.a" {
		t.Errorf("got %q, %v; want com.a", bundle, ok)
	}
	// A custom scheme never reaches the lookup.
	simListApps = func(ctx context.Context, udid string) ([]simApp, error) {
		t.Fatal("listed apps for a custom scheme")
		return nil, nil
	}
	if _, ok := s.openUniversalLink(context.Background(), "myapp://h.com/x"); ok {
		t.Error("custom scheme opened as a universal link")
	}
}

func captureLog(t *testing.T, w io.Writer) {
	t.Helper()
	old := log.Writer()
	log.SetOutput(w)
	t.Cleanup(func() { log.SetOutput(old) })
}

// `simctl listapps | plutil -convert json` on the Flipcash simulator,
// trimmed to one system app and one installed one.
func TestParseSimListApps(t *testing.T) {
	const listapps = `{
		"com.apple.reminders":{"ApplicationType":"System","Bundle":"file:///private/var/run/RuntimeRoot/Applications/Reminders.app/","CFBundleExecutable":"Reminders"},
		"com.flipcash.app.ios":{"ApplicationType":"User","Bundle":"file:///Users/bmc/Library/Developer/CoreSimulator/Devices/F3D7/data/Containers/Bundle/Application/1BBB/Flip%20cash.app/","CFBundleExecutable":"Flipcash"},
		"com.example.nobundle":{"ApplicationType":"User","CFBundleExecutable":"X"}
	}`
	got, err := parseSimListApps([]byte(listapps))
	if err != nil {
		t.Fatal(err)
	}
	want := []simApp{{"com.flipcash.app.ios", "/Users/bmc/Library/Developer/CoreSimulator/Devices/F3D7/data/Containers/Bundle/Application/1BBB/Flip cash.app/Flipcash"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
