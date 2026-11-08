package target

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// appLanguage sets the language of the app in front, through Android 13's
// per-app locales (`cmd locale set-app-locales`). The system language can't
// be changed from adb on a Play image: there is no root, no shell command
// for it, and writing system_locales takes effect only after a reboot. A
// per-app locale applies at once, which is what testing an app in another
// language needs.
//
// The value "" is the app following the system language.

// appLanguageMinSDK is the first SDK with per-app locales.
const appLanguageMinSDK = 33

// appLanguageOptions is what the setting offers. A device has no shell
// command that lists the locales it supports, so this is a fixed list: the
// common app-store locales, two right-to-left ones, and the two pseudo
// locales (en-XA stretches text, ar-XB mirrors the layout), which work in an
// app built with pseudoLocalesEnabled.
var appLanguageOptions = []string{
	"",
	"en-US", "en-GB", "es-ES", "es-419", "fr-FR", "de-DE", "it-IT", "pt-BR", "nl-NL",
	"sv-SE", "pl-PL", "tr-TR", "ru-RU", "ja-JP", "ko-KR", "zh-CN", "zh-TW", "hi-IN",
	"th-TH", "vi-VN", "id-ID", "ar-EG", "he-IL",
	"en-XA", "ar-XB",
}

// packageNameRe is what an Android package name can be. The package comes
// from device output, so it is checked before it goes into a shell command.
var packageNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z0-9_]+)+$`)

// appLocalesRe reads `cmd locale get-app-locales`:
//
//	Locales for com.example for user 0 are [fr-FR,en-US]
var appLocalesRe = regexp.MustCompile(`^Locales for (\S+) for user \d+ are \[([^\]]*)\]`)

// appLanguageCommands are the reads appLanguage needs: the SDK level, the
// activities dump, and the front app's locales. settingsScript runs them as
// three sections of its one shell call; appLanguageScript runs them alone
// for a write. The package for get-app-locales is found shell-side the same
// way appScript finds it, on the line before the third command;
// parseAppLanguage checks it against the package Go parses from the dump,
// since the two can disagree (see appScript).
func appLanguageCommands(b *strings.Builder, nonce string, first int) {
	b.WriteString(sectionEcho(nonce, first) + "; getprop ro.build.version.sdk\n")
	b.WriteString(sectionEcho(nonce, first+1) + "; dumpsys activity activities\n")
	b.WriteString(`PKG=$(dumpsys activity activities | sed -n '/^Display #0 /,/^Display #/p' | grep -o 'topResumedActivity=ActivityRecord{[^}]*}' | head -1 | sed -E 's#.* u[0-9]+ ([^/]+)/.*#\1#')` + "\n")
	b.WriteString(sectionEcho(nonce, first+2) + `; [ -n "$PKG" ] && cmd locale get-app-locales "$PKG"` + "\n")
}

func appLanguageScript(nonce string) string {
	var b strings.Builder
	appLanguageCommands(&b, nonce, 0)
	b.WriteString(sectionEcho(nonce, 3) + "\n")
	return b.String()
}

// appLanguageState is what readAppLanguage found. pkg is "" when no app is
// in front, or when the locale read named a different app than the one in
// front.
type appLanguageState struct {
	sdk     int
	pkg     string
	current string
}

func readAppLanguage(ctx context.Context, sh shellFunc) (appLanguageState, error) {
	nonce, err := newNonce()
	if err != nil {
		return appLanguageState{}, fmt.Errorf("app language: %w", err)
	}
	out, shErr := sh(ctx, appLanguageScript(nonce))
	sec, complete := splitSections(out, nonce, 3)
	if !complete {
		if shErr != nil {
			return appLanguageState{}, fmt.Errorf("app language: %w", shErr)
		}
		return appLanguageState{}, fmt.Errorf("app language: script did not complete")
	}
	return parseAppLanguage(sec[0], sec[1], sec[2]), nil
}

// parseAppLanguage reads the three sections appLanguageCommands prints.
func parseAppLanguage(sdk, dump, locales string) appLanguageState {
	st := appLanguageState{}
	st.sdk, _ = strconv.Atoi(strings.TrimSpace(sdk))
	pkg, _, ok := resumedActivity(dump)
	if !ok || !packageNameRe.MatchString(pkg) {
		return st
	}
	named, current, ok := parseAppLocales(locales)
	if !ok || named != pkg {
		return st
	}
	st.pkg, st.current = pkg, current
	return st
}

// parseAppLocales returns the package get-app-locales named and the first
// locale it is set to ("" when it follows the system).
func parseAppLocales(out string) (pkg, first string, ok bool) {
	for _, line := range strings.Split(out, "\n") {
		m := appLocalesRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		first, _, _ = strings.Cut(m[2], ",")
		return m[1], strings.TrimSpace(first), true
	}
	return "", "", false
}

// appLanguageSetting reports st as a Setting.
func appLanguageSetting(st appLanguageState) Setting {
	s := Setting{
		ID: "appLanguage", Type: SettingEnum, Value: st.current, Options: appLanguageOptions,
		Package: st.pkg, Supported: true, Applies: "immediately",
	}
	switch {
	case st.sdk < appLanguageMinSDK:
		s.Supported, s.Reason = false, "per-app language needs Android 13"
	case st.pkg == "":
		s.Supported, s.Reason = false, "no app in front"
	}
	return s
}

func setAppLanguage(ctx context.Context, sh shellFunc, pkg, lang string) error {
	if !packageNameRe.MatchString(pkg) {
		return fmt.Errorf("no app in front")
	}
	cmd := "cmd locale set-app-locales " + pkg
	if lang != "" {
		cmd += " --locales " + lang
	}
	out, err := sh(ctx, cmd)
	if err != nil {
		return fmt.Errorf("set-app-locales: %w", err)
	}
	if reply := strings.TrimSpace(out); reply != "" {
		return fmt.Errorf("set-app-locales: %s", reply)
	}
	return nil
}

// usableAppLanguage reads the front app and errors when the setting can't be
// written right now.
func usableAppLanguage(ctx context.Context, sh shellFunc) (appLanguageState, error) {
	st, err := readAppLanguage(ctx, sh)
	if err != nil {
		return st, err
	}
	if st.sdk < appLanguageMinSDK {
		return st, fmt.Errorf("per-app language needs Android 13")
	}
	if st.pkg == "" {
		return st, fmt.Errorf("no app in front")
	}
	return st, nil
}

// appLanguageField writes the language of whichever app is in front when the
// write runs, and its rollback restores that same app's previous language.
var appLanguageField = settingField{
	id:  "appLanguage",
	typ: SettingEnum,
	readRestore: func(ctx context.Context, sh shellFunc) (func(ctx context.Context, sh shellFunc) error, error) {
		st, err := usableAppLanguage(ctx, sh)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, sh shellFunc) error {
			return setAppLanguage(ctx, sh, st.pkg, st.current)
		}, nil
	},
	write: func(ctx context.Context, sh shellFunc, v any) error {
		st, err := usableAppLanguage(ctx, sh)
		if err != nil {
			return err
		}
		return setAppLanguage(ctx, sh, st.pkg, v.(string))
	},
	validate: func(_ settingField, v any) error {
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("want a string, got %T", v)
		}
		if !slices.Contains(appLanguageOptions, s) {
			return fmt.Errorf("appLanguage %q is not one of the offered languages", s)
		}
		return nil
	},
}
