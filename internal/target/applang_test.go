package target

import (
	"context"
	"strings"
	"testing"
)

func TestParseAppLanguage(t *testing.T) {
	cases := []struct {
		name, sdk, locales string
		want               appLanguageState
	}{
		{"set", "34", "Locales for com.example.app for user 0 are [fr-FR,en-US]",
			appLanguageState{sdk: 34, pkg: "com.example.app", current: "fr-FR"}},
		{"follows the system", "34", "Locales for com.example.app for user 0 are []",
			appLanguageState{sdk: 34, pkg: "com.example.app"}},
		// $PKG named another app than the one Go parsed: report no app
		// rather than another app's language.
		{"other package", "34", "Locales for com.other for user 0 are [de-DE]",
			appLanguageState{sdk: 34}},
		{"no reply", "34", "", appLanguageState{sdk: 34}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseAppLanguage(c.sdk, appDumpFixture, c.locales); got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestAppLanguageSettingSaysWhyItCannotBeSet(t *testing.T) {
	if s := appLanguageSetting(appLanguageState{sdk: 32, pkg: "com.example.app"}); s.Supported || !strings.Contains(s.Reason, "Android 13") {
		t.Fatalf("old device: %+v", s)
	}
	if s := appLanguageSetting(appLanguageState{sdk: 34}); s.Supported || s.Reason != "no app in front" {
		t.Fatalf("no app: %+v", s)
	}
	s := appLanguageSetting(appLanguageState{sdk: 34, pkg: "com.example.app", current: "ja-JP"})
	if !s.Supported || s.Package != "com.example.app" || s.Value != "ja-JP" {
		t.Fatalf("supported: %+v", s)
	}
}

// fakeAppLanguage answers appLanguageScript for com.example.app and records
// set-app-locales calls.
type fakeAppLanguage struct {
	current string
	sets    []string
}

func (f *fakeAppLanguage) sh(_ context.Context, args ...string) (string, error) {
	joined := joinArgs(args)
	if nonce, ok := extractNonce(joined); ok {
		locales := "Locales for com.example.app for user 0 are [" + f.current + "]"
		resp, _ := nonceFakeResponse(nonce, []string{"34", appDumpFixture, locales})
		return resp, nil
	}
	if rest, ok := strings.CutPrefix(joined, "cmd locale set-app-locales "); ok {
		f.sets = append(f.sets, rest)
		_, lang, _ := strings.Cut(rest, " --locales ")
		f.current = lang
	}
	return "", nil
}

func TestAppLanguageWritesAndRestoresTheFrontApp(t *testing.T) {
	f := &fakeAppLanguage{current: "fr-FR"}
	a := &androidSettings{sh: f.sh}
	field, ok := a.fieldByID("appLanguage")
	if !ok {
		t.Fatal("appLanguage is not a field")
	}
	ctx := context.Background()
	restore, err := field.readRestore(ctx, f.sh)
	if err != nil {
		t.Fatal(err)
	}
	if err := field.write(ctx, f.sh, "ar-XB"); err != nil {
		t.Fatal(err)
	}
	if err := restore(ctx, f.sh); err != nil {
		t.Fatal(err)
	}
	want := []string{"com.example.app --locales ar-XB", "com.example.app --locales fr-FR"}
	if strings.Join(f.sets, "|") != strings.Join(want, "|") {
		t.Fatalf("sets %q, want %q", f.sets, want)
	}
	// "" puts the app back on the system language: no --locales at all.
	if err := field.write(ctx, f.sh, ""); err != nil {
		t.Fatal(err)
	}
	if last := f.sets[len(f.sets)-1]; last != "com.example.app" {
		t.Fatalf("follow-system write was %q", last)
	}
}

func TestAppLanguageRejectsAnUnofferedValue(t *testing.T) {
	a := &androidSettings{}
	field, _ := a.fieldByID("appLanguage")
	for _, v := range []any{"fr-FR; reboot", "xx-YY", 3} {
		if err := field.validate(field, v); err == nil {
			t.Fatalf("%v passed validation", v)
		}
	}
	if err := field.validate(field, "pt-BR"); err != nil {
		t.Fatal(err)
	}
}
