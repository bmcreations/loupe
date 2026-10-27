package target

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidSetting wraps every error ApplySettings returns before it has run
// a single command: an unknown id, a value that fails its field's validate,
// or (talkBack only) a device that does not support the setting at all. The
// handler uses errors.Is against this to answer 400 rather than 409 --
// nothing on the device was touched, so there is nothing to report as failed
// and nothing to roll back.
var ErrInvalidSetting = errors.New("invalid setting")

// ErrDeviceUnreachable wraps an error ApplySettings' pre-flight talkBack
// check hit trying to reach the device itself, as opposed to
// ErrInvalidSetting's "the device answered and TalkBack is genuinely
// absent". Nothing has run by the time this can be returned, so there is
// nothing to roll back either; the handler maps it to 502 rather than the
// 400 an invalid setting gets or the 409 a partial failure gets.
var ErrDeviceUnreachable = errors.New("device unreachable")

// SettingType names the shape of a Setting's Value, so a client can build the
// right control without hardcoding per-id knowledge.
type SettingType string

const (
	SettingBool  SettingType = "bool"
	SettingRange SettingType = "range"
	// SettingEnum is a setting whose Value is one of Options: posture, on a
	// foldable emulator.
	SettingEnum SettingType = "enum"
)

// Setting is one device-wide accessibility or display toggle, reported the
// same way config.go reports a server setting: the value in effect, whether
// it can be written at all, and what applying a change means. Unlike the
// server's own settings there is no file/flag/default provenance here -- a
// device setting has exactly one source, the device -- but "supported" plays
// the same role "pinned" does there: a caller that cannot see it will claim
// success for a write that does nothing.
//
// darkMode and reduceMotion are reported as a bool over device state that
// carries more than two values. Night mode can be "auto" or "custom_schedule"
// as well as "yes"/"no", and the three animation-scale keys behind
// reduceMotion can independently hold any value, not just 0 or 1. Value
// collapses both to a bool for the client; ApplySettings' rollback keeps the
// raw strings it actually read, so a device found in one of those other
// states restores to exactly that state rather than to whichever side of the
// bool it rounded to.
type Setting struct {
	ID    string      `json:"id"`
	Type  SettingType `json:"type"`
	Value any         `json:"value"`
	// Min and Max apply only to SettingRange.
	Min float64 `json:"min,omitempty"`
	Max float64 `json:"max,omitempty"`
	// Options applies only to SettingEnum: the allowed values for Value.
	Options []string `json:"options,omitempty"`
	// Supported is false when the setting cannot be changed on this device
	// (TalkBack not installed, so far the only case), and Reason says why.
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
	// Applies is always "immediately": every setting here is a live device
	// toggle, not a stream parameter queued for the next session the way
	// config.go's maxFps and bitRate are.
	Applies string `json:"applies"`
}

// DeviceSettings is implemented by targets that can read and change
// device-wide settings. Synthetic cannot, so this is deliberately not part of
// Target, the same way Inspector, Screenshotter and Framer are optional.
type DeviceSettings interface {
	Settings(ctx context.Context) ([]Setting, error)
	// ApplySettings changes one or more settings, atomically from the
	// caller's point of view: on any failure, every setting this call had
	// already started changing is restored to the raw state it read before
	// the call began. failed names the setting that failed and every setting
	// after it in table order, which never ran at all. notRestored names any
	// already-applied setting whose rollback write itself failed -- best
	// effort, not guaranteed -- and is reported alongside failed rather than
	// folded into it, because a setting in notRestored did change and a
	// setting in failed did not.
	ApplySettings(ctx context.Context, changes map[string]any) (failed, notRestored []string, err error)
}

// talkBackComponent is the accessibility-service component id TalkBack
// registers under. It is a constant, not a caller-supplied string, because
// enabled_accessibility_services is a colon-separated list shared with every
// other accessibility service on the device and a setting write here must
// never let a caller name an arbitrary component to add or remove.
const talkBackComponent = "com.google.android.marvin.talkback/com.google.android.marvin.talkback.TalkBackService"

// talkBackShortComponent is the same service named the way
// ComponentName.unflattenFromString accepts and Android itself sometimes
// writes back: a class name starting with "." is shorthand for
// "<package>.<rest>". A raw list holding this form and a list holding the
// fully-qualified form name the same service, so comparing the two literally
// would add a duplicate on enable and fail to remove it on disable.
const talkBackShortComponent = "com.google.android.marvin.talkback/.TalkBackService"

// talkBackPackage is checked with `pm path` to decide Supported: a device
// without TalkBack installed can still be asked to read or write the
// accessibility-services list, and doing so would silently do nothing.
const talkBackPackage = "com.google.android.marvin.talkback"

// rollbackTimeout bounds a rollback's own context, independent of the
// request that triggered it. A rollback runs after a write has already
// failed, often because the connection to the device is in trouble, so it
// needs time of its own rather than whatever was left on the request's
// deadline -- which, for a request already close to its own timeout, could
// be nothing at all.
const rollbackTimeout = 10 * time.Second

// shellFunc runs one `adb shell` command and returns its output. Settings
// logic is written against this instead of *ADBTarget directly so it can be
// exercised without a device, the same shape adb.go's own tests use.
type shellFunc func(ctx context.Context, args ...string) (string, error)

// settingField is one row of the fixed table below. read, readRestore and
// write never see a caller-supplied setting name or a caller-supplied shell
// argument: id, the shell command shape, and (for talkBack) the component
// string are all compiled in, and the only caller-controlled data that
// reaches a shell command is a bool or a validated float formatted by this
// package itself.
type settingField struct {
	id       string
	typ      SettingType
	min, max float64
	// readRestore captures the exact raw state a field is in right now and
	// returns a closure that writes that same raw state back. Settings()
	// itself reads current values through the combined script and its own
	// parse functions (parseFontScale, parseReduceMotion, ...), not through
	// this -- readRestore exists purely for rollback, and normalizes
	// nothing: darkMode's "auto", a reduceMotion key sitting at 0.5, and an
	// unset font_scale all have no lossless bool/float form, so restoring
	// them means replaying the raw string, not a typed value.
	readRestore func(ctx context.Context, sh shellFunc) (restore func(ctx context.Context, sh shellFunc) error, err error)
	write       func(ctx context.Context, sh shellFunc, value any) error
	// validate is passed its own field so a range check has one source of
	// truth for min/max: the same numbers settingFields declares and Settings
	// reports in the GET response.
	validate func(f settingField, value any) error
}

// settingFields is iterated in this order everywhere: for the GET response,
// and for the order ApplySettings applies (and, in reverse, rolls back)
// changes in. That order is arbitrary but fixed, which is what lets a
// rollback test assert on which settings were restored and which never ran.
var settingFields = []settingField{
	{id: "darkMode", typ: SettingBool, readRestore: readRestoreDarkMode, write: writeDarkMode, validate: validateBool},
	{id: "fontScale", typ: SettingRange, min: 0.85, max: 2.0, readRestore: readRestoreFontScale, write: writeFontScale, validate: validateRange},
	{id: "reduceMotion", typ: SettingBool, readRestore: readRestoreReduceMotion, write: writeReduceMotion, validate: validateBool},
	{id: "layoutBounds", typ: SettingBool, readRestore: readRestoreLayoutBounds, write: writeLayoutBounds, validate: validateBool},
	{id: "talkBack", typ: SettingBool, readRestore: readRestoreTalkBack, write: writeTalkBack, validate: validateBool},
}

func (a *androidSettings) fieldByID(id string) (settingField, bool) {
	for _, f := range a.fields() {
		if f.id == id {
			return f, true
		}
	}
	return settingField{}, false
}

func validateBool(_ settingField, v any) error {
	if _, ok := v.(bool); !ok {
		return fmt.Errorf("want a bool, got %T", v)
	}
	return nil
}

// validateRange checks v against f's own Min/Max, so the bound enforced here
// can never drift from the bound Settings() reports to a caller building a
// slider from it.
func validateRange(f settingField, v any) error {
	fl, ok := v.(float64)
	if !ok {
		return fmt.Errorf("want a number, got %T", v)
	}
	if fl < f.min || fl > f.max {
		return fmt.Errorf("%s %v is outside %v-%v", f.id, fl, f.min, f.max)
	}
	return nil
}

// androidSettings is the pure logic behind ADBTarget's DeviceSettings, kept
// separate from ADBTarget so it can be tested with a fake shellFunc instead
// of a device.
//
// emu is set only on an emulator, and adds the posture field.
type androidSettings struct {
	sh  shellFunc
	emu emuFunc
}

// fields is settingFields, plus posture on an emulator.
func (a *androidSettings) fields() []settingField {
	if a.emu == nil {
		return settingFields
	}
	return append(append([]settingField(nil), settingFields...), postureField(a.emu))
}

// Settings reads current device state. It does not lock ADBTarget's
// settingsMu: a GET racing a PUT can see a value mid-change, which is no
// worse than any other unsynchronized read of live device state, and the PUT
// path's own re-read runs inside the lock it already holds.
func (t *ADBTarget) Settings(ctx context.Context) ([]Setting, error) {
	if !t.info.Ready() {
		return nil, fmt.Errorf("%w: device %s is in state %q", ErrDeviceUnreachable, t.info.Serial, t.info.State)
	}
	return newAndroidSettings(t).Settings(ctx)
}

// ApplySettings holds settingsMu for its entire run, including the talkBack
// read-modify-write inside writeTalkBack. Two concurrent PUTs that both touch
// talkBack would otherwise interleave their reads of
// enabled_accessibility_services and one caller's change would be silently
// dropped by the other's write.
func (t *ADBTarget) ApplySettings(ctx context.Context, changes map[string]any) ([]string, []string, error) {
	if !t.info.Ready() {
		return nil, nil, fmt.Errorf("%w: device %s is in state %q", ErrDeviceUnreachable, t.info.Serial, t.info.State)
	}
	t.settingsMu.Lock()
	defer t.settingsMu.Unlock()
	return newAndroidSettings(t).ApplySettings(ctx, changes)
}

func newAndroidSettings(t *ADBTarget) *androidSettings {
	a := &androidSettings{sh: func(ctx context.Context, args ...string) (string, error) {
		return t.adb.Shell(ctx, t.info.Serial, args...)
	}}
	if strings.HasPrefix(t.info.Serial, "emulator-") {
		a.emu = func(ctx context.Context, args ...string) (string, error) {
			return t.adb.Emu(ctx, t.info.Serial, args...)
		}
	}
	return a
}

// settingsSectionMarker delimits one field's raw shell output from the next
// inside the single combined command Settings issues. Trap 33: a separate
// `adb shell` per field opens a separate loopback TCP connection per field,
// and under load the ephemeral port range can run out from invocation count
// alone with nothing wrong on the device end. One shell command, whatever it
// asks for internally, is one connection.
const settingsSectionMarker = "@@LOUPE:"

// newNonce returns a random per-call token that a device-read value cannot
// have been crafted to contain in advance. Without it, a section marker is a
// fixed, predictable string, and device state a caller does not control --
// an app's versionName, an entry in enabled_accessibility_services -- can
// forge a line that looks like the next section boundary: a versionName of
// "@@LOUPE:0@@ topResumedActivity=ActivityRecord{... com.spoof/.Fake t1}"
// made ForegroundApp report the spoofed package, and a services list holding
// "@@LOUPE:1@@2.0" made Settings report fontScale 2. Sixteen hex characters
// from crypto/rand is not guessable by anything that only sees prior output.
func newNonce() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate section nonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// sectionMarkerLine is the exact line echo prints for section i under nonce.
// It is matched as a whole line, never as a substring, which is what stops a
// device value that merely contains the marker text from being read as one:
// device output embedded between two real markers becomes a body line, and a
// body line is never equal to a marker line unless the device could predict
// the nonce.
func sectionMarkerLine(nonce string, i int) string {
	return fmt.Sprintf("%s%s:%d@@", settingsSectionMarker, nonce, i)
}

func sectionEcho(nonce string, i int) string {
	return "echo " + sectionMarkerLine(nonce, i)
}

// splitSections divides combined output from a script built with sectionEcho
// into n content sections (0..n-1) plus a required terminal marker at index
// n, and reports whether that terminal marker was reached. The terminal
// marker, not the shell command's exit status, is what says the script ran
// to completion: the last real command in the script can legitimately fail
// (pm path on a device without TalkBack exits 1), which makes the shell's
// own exit status indistinguishable from adb dropping the connection
// mid-script. Trailing a fixed `echo` after every real command fixes that --
// echo cannot fail -- so only a marker actually being present, not the
// process's exit code, is trusted.
//
// A marker is recognised only as an exact line, only for the next expected
// index in order, and only once: any other line -- including one holding the
// marker text at the wrong nonce, the wrong index, out of order, or a repeat
// of an index already consumed -- is treated as body content of whichever
// section is currently open, never as a boundary.
func splitSections(out, nonce string, n int) (sections []string, complete bool) {
	sections = make([]string, n)
	lines := strings.Split(out, "\n")
	expect := 0
	idx := -1
	var body []string
	flush := func() {
		if idx >= 0 && idx < n {
			sections[idx] = strings.Join(body, "\n")
		}
		body = nil
	}
	for _, line := range lines {
		// An older adb transport runs shell output through a pty, which
		// rewrites every line ending to CRLF. Trimming a trailing \r before
		// comparing is what keeps that path from hiding every marker line
		// and reading every GET as incomplete.
		trimmed := strings.TrimSuffix(line, "\r")
		if expect <= n && trimmed == sectionMarkerLine(nonce, expect) {
			flush()
			idx = expect
			expect++
			continue
		}
		if idx >= 0 {
			body = append(body, line)
		}
	}
	flush()
	return sections, expect > n
}

// settingsScript is the single command Settings sends to read every field.
// Each line's echo runs before the read it labels, so a read that itself
// prints nothing (getprop on an unset property) still leaves its section
// findable, and a step that fails leaves every section after it empty rather
// than shifting the ones after it out of place. The trailing echo carries no
// command of its own -- it exists only so the script's last command always
// succeeds, and reaching it is splitSections' signal that every step before
// it ran (see the comment there): the pm path step second-to-last routinely
// fails with a nonzero exit on a device without TalkBack installed, and that
// must not be mistaken for the script itself dying early.
//
// withPosture appends the two device_state reads, on an emulator only, so a
// device that cannot fold pays nothing for them.
func settingsScript(nonce string, withPosture bool) string {
	cmds := []string{
		"cmd uimode night",
		"settings get system font_scale",
		"settings get global window_animation_scale",
		"settings get global transition_animation_scale",
		"settings get global animator_duration_scale",
		"getprop debug.layout",
		"settings get secure enabled_accessibility_services",
		"pm path " + talkBackPackage,
	}
	if withPosture {
		cmds = append(cmds, "cmd device_state print-states", "cmd device_state print-state")
	}
	var b strings.Builder
	for i, c := range cmds {
		b.WriteString(sectionEcho(nonce, i))
		b.WriteString("; ")
		b.WriteString(c)
		b.WriteString("\n")
	}
	b.WriteString(sectionEcho(nonce, len(cmds)))
	b.WriteString("\n")
	return b.String()
}

func (a *androidSettings) Settings(ctx context.Context) ([]Setting, error) {
	nonce, err := newNonce()
	if err != nil {
		return nil, fmt.Errorf("settings: %w", err)
	}
	withPosture := a.emu != nil
	n := 8
	if withPosture {
		n = 10
	}
	out, shErr := a.sh(ctx, settingsScript(nonce, withPosture))
	sec, complete := splitSections(out, nonce, n)
	if !complete {
		if shErr != nil {
			return nil, fmt.Errorf("settings: %w", shErr)
		}
		return nil, fmt.Errorf("settings: script did not complete")
	}
	nightMode, fontScale, windowAnim, transitionAnim, animatorDur, layoutBounds, accessList, talkBackPath :=
		sec[0], sec[1], sec[2], sec[3], sec[4], sec[5], sec[6], sec[7]

	fs, err := parseFontScale(fontScale)
	if err != nil {
		return nil, fmt.Errorf("fontScale: %w", err)
	}
	reduceMotion, err := parseReduceMotion(windowAnim, transitionAnim, animatorDur)
	if err != nil {
		return nil, fmt.Errorf("reduceMotion: %w", err)
	}
	talkBackOK, talkBackReason := parseTalkBackSupported(talkBackPath)

	values := map[string]any{
		"darkMode":     strings.Contains(nightMode, "yes"),
		"fontScale":    fs,
		"reduceMotion": reduceMotion,
		"layoutBounds": strings.TrimSpace(layoutBounds) == "true",
		"talkBack":     hasService(accessList, talkBackComponent),
	}
	supported := map[string]bool{"darkMode": true, "fontScale": true, "reduceMotion": true, "layoutBounds": true, "talkBack": talkBackOK}
	reasons := map[string]string{"talkBack": talkBackReason}

	out2 := make([]Setting, 0, len(settingFields))
	for _, f := range settingFields {
		s := Setting{
			ID: f.id, Type: f.typ, Value: values[f.id],
			Supported: supported[f.id], Reason: reasons[f.id], Applies: "immediately",
		}
		if f.typ == SettingRange {
			s.Min, s.Max = f.min, f.max
		}
		out2 = append(out2, s)
	}
	// Posture is listed only where it can change: a phone that does not
	// fold has nothing to report, which is not the same as a TalkBack that
	// is not installed.
	if withPosture {
		byID, options := parsePostureStates(sec[8])
		if postureSupported(options) {
			out2 = append(out2, Setting{
				ID: "posture", Type: SettingEnum, Value: currentPosture(sec[9], byID),
				Options: options, Supported: true, Applies: "immediately",
			})
		}
	}
	return out2, nil
}

// appliedChange is the rollback record for one setting ApplySettings has
// already started changing: the field (for its id, when a rollback write
// itself fails) and a closure, captured before the write ran, that restores
// the exact raw state read then.
type appliedChange struct {
	field   settingField
	restore func(ctx context.Context, sh shellFunc) error
}

func (a *androidSettings) ApplySettings(ctx context.Context, changes map[string]any) (failed, notRestored []string, err error) {
	if len(changes) == 0 {
		return nil, nil, fmt.Errorf("%w: no changes given", ErrInvalidSetting)
	}

	// Validate every requested id and value before writing anything. A
	// caller-supplied id that is not in settingFields never reaches a shell
	// command at all: fieldByID is the only lookup, and nothing here formats
	// a command from the id string itself. The talkBack-supported check does
	// run a command (`pm path`), but only a read, and only when talkBack is
	// one of the requested ids -- asking to enable TalkBack on a device that
	// does not have it installed is rejected here, before any write, rather
	// than reported as a failure after a no-op write ran.
	for id, v := range changes {
		f, ok := a.fieldByID(id)
		if !ok {
			return nil, nil, fmt.Errorf("%w: unknown setting %q", ErrInvalidSetting, id)
		}
		if f.id == "talkBack" {
			ok, reason, terr := talkBackSupported(ctx, a.sh)
			if terr != nil {
				return nil, nil, fmt.Errorf("%w: talkBack: %w", ErrDeviceUnreachable, terr)
			}
			if !ok {
				return nil, nil, fmt.Errorf("%w: talkBack: %s", ErrInvalidSetting, reason)
			}
		}
		if err := f.validate(f, v); err != nil {
			return nil, nil, fmt.Errorf("%w: %s: %s", ErrInvalidSetting, id, err)
		}
		if f.id == "posture" {
			_, options, perr := readPosture(ctx, a.sh)
			if perr != nil {
				return nil, nil, fmt.Errorf("%w: posture: %w", ErrDeviceUnreachable, perr)
			}
			if !slices.Contains(options, v.(string)) {
				return nil, nil, fmt.Errorf("%w: posture: %q is not one of this device's %v", ErrInvalidSetting, v, options)
			}
		}
	}

	// Not collapsed into the single-script form GET uses (trap 33): each
	// field's readRestore has to run and be recorded into done before that
	// field's write is attempted, so a later field's write failure rolls
	// back only what already changed. A combined script would need to
	// either read every field up front -- losing that per-field
	// interleaving -- or become as many round trips as this already is.
	// A PUT changing every field is five reads and up to nine writes,
	// against a read-only GET's one; not free, but bounded and rare enough
	// (a settings PUT is a human action, not a per-frame one) that it has
	// not been worth the loss of the interleaving invariant item 3 depends
	// on.
	//
	// Table order, not map iteration order: this is what makes "the third of
	// four changes fails" a reproducible test rather than one that depends on
	// Go's randomised map order.
	var order []settingField
	for _, f := range a.fields() {
		if _, ok := changes[f.id]; ok {
			order = append(order, f)
		}
	}

	var done []appliedChange
	for _, f := range order {
		restore, rerr := f.readRestore(ctx, a.sh)
		if rerr != nil {
			nr := rollback(ctx, a.sh, done)
			return failedFrom(order, f.id), nr, fmt.Errorf("%s: read current value: %w", f.id, rerr)
		}
		// Recorded before write runs, not after it succeeds: a field that
		// fails partway through its own multi-command write (reduceMotion's
		// three scale keys, layoutBounds' setprop-then-nudge, talkBack's
		// list-then-enabled-flag) has already changed device state by the
		// time write returns an error, and that partial state has to be
		// rolled back too.
		done = append(done, appliedChange{field: f, restore: restore})
		if werr := f.write(ctx, a.sh, changes[f.id]); werr != nil {
			nr := rollback(ctx, a.sh, done)
			return failedFrom(order, f.id), nr, fmt.Errorf("%s: %w", f.id, werr)
		}
	}
	return nil, nil, nil
}

// rollback restores every already-applied change, most recent first, using
// the closure each one captured before it wrote. It runs under a context
// detached from the caller's: rollback follows a failure that is often the
// request's own deadline running out, or close to it, and reusing that
// context would mean a rollback triggered by a slow link never gets to run
// at all. notRestored names every field whose own restore also failed --
// best effort, and reported rather than silently absorbed, because a field
// in this list is left in a different state than either the value requested
// or the value read before the call.
func rollback(ctx context.Context, sh shellFunc, done []appliedChange) []string {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	var notRestored []string
	for i := len(done) - 1; i >= 0; i-- {
		d := done[i]
		if err := d.restore(rctx, sh); err != nil {
			notRestored = append(notRestored, d.field.id)
		}
	}
	return notRestored
}

// failedFrom names failedID and every setting after it in order: the ones
// that were already applied and have now been rolled back are not in this
// list, and neither is anything before failedID, because rollback is what
// undoes those.
func failedFrom(order []settingField, failedID string) []string {
	var failed []string
	found := false
	for _, f := range order {
		if f.id == failedID {
			found = true
		}
		if found {
			failed = append(failed, f.id)
		}
	}
	return failed
}

// --- darkMode -----------------------------------------------------------

// nightModeRaw pulls the single word after "Night mode:" -- yes, no, auto,
// or a custom_schedule variant -- without collapsing it to a bool, so a
// rollback can write the exact word back.
func nightModeRaw(out string) string {
	const prefix = "Night mode:"
	i := strings.Index(out, prefix)
	if i < 0 {
		return "no"
	}
	rest := strings.TrimSpace(out[i+len(prefix):])
	if nl := strings.IndexAny(rest, "\r\n"); nl >= 0 {
		rest = rest[:nl]
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "no"
	}
	return rest
}

func readRestoreDarkMode(ctx context.Context, sh shellFunc) (func(ctx context.Context, sh shellFunc) error, error) {
	out, err := sh(ctx, "cmd", "uimode", "night")
	if err != nil {
		return nil, fmt.Errorf("cmd uimode night: %w", err)
	}
	raw := nightModeRaw(out)
	return func(ctx context.Context, sh shellFunc) error {
		_, err := sh(ctx, "cmd", "uimode", "night", shellQuote(raw))
		return err
	}, nil
}

func writeDarkMode(ctx context.Context, sh shellFunc, value any) error {
	on := value.(bool)
	arg := "no"
	if on {
		arg = "yes"
	}
	_, err := sh(ctx, "cmd", "uimode", "night", arg)
	return err
}

// --- fontScale ------------------------------------------------------------

// parseFontScale treats "null" -- what `settings get` prints for a key that
// has never been set -- as the platform default, 1.0, rather than a parse
// failure. A freshly booted device reads null here.
func parseFontScale(out string) (float64, error) {
	out = strings.TrimSpace(out)
	if out == "" || out == "null" {
		return 1.0, nil
	}
	f, err := strconv.ParseFloat(out, 64)
	if err != nil {
		return 0, fmt.Errorf("unparseable value %q", out)
	}
	return f, nil
}

// readRestoreFontScale restores via `settings delete` rather than writing
// "1.0" back when the key had never been set: a device that had never had
// font_scale touched reads "null" from `settings get`, and deleting is what
// makes a later read see "null" again instead of an explicit default that
// happens to match it.
func readRestoreFontScale(ctx context.Context, sh shellFunc) (func(ctx context.Context, sh shellFunc) error, error) {
	out, err := sh(ctx, "settings", "get", "system", "font_scale")
	if err != nil {
		return nil, fmt.Errorf("settings get system font_scale: %w", err)
	}
	raw := strings.TrimSpace(out)
	return func(ctx context.Context, sh shellFunc) error {
		if raw == "" || raw == "null" {
			_, err := sh(ctx, "settings", "delete", "system", "font_scale")
			return err
		}
		_, err := sh(ctx, "settings", "put", "system", "font_scale", shellQuote(raw))
		return err
	}, nil
}

func writeFontScale(ctx context.Context, sh shellFunc, value any) error {
	f := value.(float64)
	_, err := sh(ctx, "settings", "put", "system", "font_scale", strconv.FormatFloat(f, 'f', -1, 64))
	return err
}

// --- reduceMotion -----------------------------------------------------------

// animationScaleKeys are the three `global` settings that together gate every
// system animation. All three have to move together: leaving one at 1 still
// shows the animation it controls.
var animationScaleKeys = []string{
	"window_animation_scale",
	"transition_animation_scale",
	"animator_duration_scale",
}

func parseReduceMotion(vals ...string) (bool, error) {
	for _, out := range vals {
		out = strings.TrimSpace(out)
		f := 1.0
		if out != "" && out != "null" {
			parsed, err := strconv.ParseFloat(out, 64)
			if err != nil {
				return false, fmt.Errorf("unparseable value %q", out)
			}
			f = parsed
		}
		if f != 0 {
			return false, nil
		}
	}
	return true, nil
}

// readRestoreReduceMotion keeps each key's raw string separately, because the
// three can independently hold any value -- 0.5 is a real, if unusual,
// device state -- and collapsing to the reported bool before restoring would
// round it to 0 or 1 and lose it.
func readRestoreReduceMotion(ctx context.Context, sh shellFunc) (func(ctx context.Context, sh shellFunc) error, error) {
	raws := make([]string, len(animationScaleKeys))
	for i, key := range animationScaleKeys {
		out, err := sh(ctx, "settings", "get", "global", key)
		if err != nil {
			return nil, fmt.Errorf("settings get global %s: %w", key, err)
		}
		raws[i] = strings.TrimSpace(out)
	}
	return func(ctx context.Context, sh shellFunc) error {
		for i, key := range animationScaleKeys {
			raw := raws[i]
			if raw == "" || raw == "null" {
				if _, err := sh(ctx, "settings", "delete", "global", key); err != nil {
					return fmt.Errorf("settings delete global %s: %w", key, err)
				}
				continue
			}
			if _, err := sh(ctx, "settings", "put", "global", key, shellQuote(raw)); err != nil {
				return fmt.Errorf("settings put global %s: %w", key, err)
			}
		}
		return nil
	}, nil
}

// writeReduceMotion, off, sets 1 rather than restoring whatever the three
// keys read before this setting was turned on. Remembering the prior values
// for that purpose would mean keeping state somewhere that survives a server
// restart, and it would not: the server holds nothing across a restart, so
// "restore the prior values" would silently become "restore 1" anyway the
// first time the process restarted between an on and an off. 1 is also the
// Android default for all three keys, so writing it unconditionally is what
// a device that had never had this setting touched already reads as. (This
// is a property of the write, not of rollback: readRestoreReduceMotion above
// keeps the real prior values for ApplySettings' own rollback, which runs
// within one request and has no restart to lose them to.)
func writeReduceMotion(ctx context.Context, sh shellFunc, value any) error {
	on := value.(bool)
	scale := "1"
	if on {
		scale = "0"
	}
	for _, key := range animationScaleKeys {
		if _, err := sh(ctx, "settings", "put", "global", key, scale); err != nil {
			return fmt.Errorf("settings put global %s: %w", key, err)
		}
	}
	return nil
}

// --- layoutBounds -----------------------------------------------------------

func readRestoreLayoutBounds(ctx context.Context, sh shellFunc) (func(ctx context.Context, sh shellFunc) error, error) {
	out, err := sh(ctx, "getprop", "debug.layout")
	if err != nil {
		return nil, fmt.Errorf("getprop debug.layout: %w", err)
	}
	raw := strings.TrimSpace(out)
	return func(ctx context.Context, sh shellFunc) error {
		// shellQuote is unconditional, not just for metacharacters: an empty
		// raw (the property was never set) needs to reach the device as an
		// explicit empty argument, and adb shell drops an empty unquoted
		// argument entirely when it concatenates argv into one command line,
		// which would turn "setprop debug.layout ''" into "setprop
		// debug.layout" -- a query, not the restore this closure exists for.
		if _, err := sh(ctx, "setprop", "debug.layout", shellQuote(raw)); err != nil {
			return fmt.Errorf("setprop debug.layout: %w", err)
		}
		// SYSPROPS_TRANSACTION (0x5f535052, the ASCII bytes '_SPR' read as an
		// int, which is how binder transaction codes for well-known IPCs are
		// chosen) sent to the activity service, the same transaction the
		// "Show layout bounds" developer option itself triggers, so a
		// property change on an idle screen becomes visible without the
		// caller having to also cause some other UI change. Its failure does
		// not fail the restore: the property itself, which is the actual
		// persisted state, is already back to what it was, and the nudge is
		// a cosmetic redraw rather than state that can be "not restored".
		_, _ = sh(ctx, "service", "call", "activity", "1599295570")
		return nil
	}, nil
}

// writeLayoutBounds sets the property and then nudges every window to
// redraw. The property alone only takes effect on the next layout pass,
// which on an idle screen can be never; broadcasting service call
// 1599295570 is what makes it visible without the caller having to also
// cause some other UI change.
func writeLayoutBounds(ctx context.Context, sh shellFunc, value any) error {
	on := value.(bool)
	arg := "false"
	if on {
		arg = "true"
	}
	if _, err := sh(ctx, "setprop", "debug.layout", arg); err != nil {
		return fmt.Errorf("setprop debug.layout: %w", err)
	}
	_, err := sh(ctx, "service", "call", "activity", "1599295570")
	return err
}

// --- talkBack -----------------------------------------------------------

// talkBackSupported reports whether TalkBack is installed on the device.
// `pm path` on a package that is not installed can still exit 0 on some
// devices while printing nothing useful, so both the exit status and the
// "package:" prefix on its output are required -- either alone has been seen
// to read as "installed" for a package that is not. The pm path command runs
// wrapped in the same sentinel-terminated single-command shape settingsScript
// uses (see splitSections), for the same reason: `pm path` on an absent
// package legitimately exits nonzero, and that must not be confused with adb
// itself failing to reach the device. err is non-nil only for the latter --
// a genuine inability to ask the device, which the caller must not report as
// "TalkBack is not installed" (that is what supported=false, reason="..." is
// for) but as its own failure.
func talkBackSupported(ctx context.Context, sh shellFunc) (supported bool, reason string, err error) {
	nonce, nerr := newNonce()
	if nerr != nil {
		return false, "", nerr
	}
	script := sectionEcho(nonce, 0) + "; pm path " + talkBackPackage + "\n" + sectionEcho(nonce, 1) + "\n"
	out, shErr := sh(ctx, script)
	sec, complete := splitSections(out, nonce, 1)
	if !complete {
		if shErr != nil {
			return false, "", fmt.Errorf("pm path %s: %w", talkBackPackage, shErr)
		}
		return false, "", fmt.Errorf("pm path %s: script did not complete", talkBackPackage)
	}
	supported, reason = parseTalkBackSupported(sec[0])
	return supported, reason, nil
}

func parseTalkBackSupported(pmPathOut string) (bool, string) {
	if !strings.HasPrefix(strings.TrimSpace(pmPathOut), "package:") {
		return false, "TalkBack is not installed on this device"
	}
	return true, ""
}

func readRestoreTalkBack(ctx context.Context, sh shellFunc) (func(ctx context.Context, sh shellFunc) error, error) {
	list, err := sh(ctx, "settings", "get", "secure", "enabled_accessibility_services")
	if err != nil {
		return nil, fmt.Errorf("settings get secure enabled_accessibility_services: %w", err)
	}
	rawList := strings.TrimSpace(list)

	enabled, err := sh(ctx, "settings", "get", "secure", "accessibility_enabled")
	if err != nil {
		return nil, fmt.Errorf("settings get secure accessibility_enabled: %w", err)
	}
	rawEnabled := strings.TrimSpace(enabled)

	return func(ctx context.Context, sh shellFunc) error {
		if rawList == "" || rawList == "null" {
			if _, err := sh(ctx, "settings", "delete", "secure", "enabled_accessibility_services"); err != nil {
				return fmt.Errorf("settings delete secure enabled_accessibility_services: %w", err)
			}
		} else {
			if _, err := sh(ctx, "settings", "put", "secure", "enabled_accessibility_services", shellQuote(rawList)); err != nil {
				return fmt.Errorf("settings put secure enabled_accessibility_services: %w", err)
			}
		}
		if rawEnabled == "" || rawEnabled == "null" {
			_, err := sh(ctx, "settings", "delete", "secure", "accessibility_enabled")
			if err != nil {
				return fmt.Errorf("settings delete secure accessibility_enabled: %w", err)
			}
			return nil
		}
		_, err := sh(ctx, "settings", "put", "secure", "accessibility_enabled", shellQuote(rawEnabled))
		if err != nil {
			return fmt.Errorf("settings put secure accessibility_enabled: %w", err)
		}
		return nil
	}, nil
}

func writeTalkBack(ctx context.Context, sh shellFunc, value any) error {
	on := value.(bool)

	current, err := sh(ctx, "settings", "get", "secure", "enabled_accessibility_services")
	if err != nil {
		return fmt.Errorf("settings get secure enabled_accessibility_services: %w", err)
	}

	var next string
	if on {
		next = withService(current, talkBackComponent)
	} else {
		next = withoutService(current, talkBackComponent)
	}

	if next == "" {
		_, err = sh(ctx, "settings", "delete", "secure", "enabled_accessibility_services")
	} else {
		// next is built from a device-read list plus a compiled-in component
		// string, never from caller input, but the list can already hold
		// another accessibility service's component name -- itself
		// arbitrary, since some other agent or the user could have put
		// anything there -- and that string is about to reach adb shell.
		// shellQuote is unconditional so a component containing a shell
		// metacharacter cannot turn a settings write into a second command.
		_, err = sh(ctx, "settings", "put", "secure", "enabled_accessibility_services", shellQuote(next))
	}
	if err != nil {
		return fmt.Errorf("settings put secure enabled_accessibility_services: %w", err)
	}

	if on {
		_, err = sh(ctx, "settings", "put", "secure", "accessibility_enabled", "1")
	}
	return err
}

// expandComponent expands a short class name the way
// ComponentName.unflattenFromString does: a class beginning with "." is
// shorthand for "<package>.<rest>". Android itself writes this short form
// back into enabled_accessibility_services in some versions, so a list
// entry in that form names the same service as the fully-qualified one and
// has to compare equal to it.
func expandComponent(component string) string {
	pkg, cls, ok := strings.Cut(component, "/")
	if !ok || !strings.HasPrefix(cls, ".") {
		return component
	}
	return pkg + "/" + pkg + cls
}

// hasService reports whether component appears in list, a colon-separated
// enabled_accessibility_services value ("null" when the setting has never
// been written). Comparison is against each entry's expanded form, so a
// list holding the short "pkg/.Cls" form still matches a lookup for the
// fully-qualified component and vice versa.
func hasService(list, component string) bool {
	want := expandComponent(component)
	for _, p := range splitServices(list) {
		if expandComponent(p) == want {
			return true
		}
	}
	return false
}

// withService adds component to list if it is not already present under
// either its short or fully-qualified form, keeping every other entry
// untouched. Never rewrites the whole list to just component: another
// accessibility service already enabled on the device would be silently
// turned off.
func withService(list, component string) string {
	parts := splitServices(list)
	want := expandComponent(component)
	for _, p := range parts {
		if expandComponent(p) == want {
			return joinServices(parts)
		}
	}
	return joinServices(append(parts, component))
}

// withoutService removes component from list, matching either its short or
// fully-qualified form, keeping every other entry.
func withoutService(list, component string) string {
	parts := splitServices(list)
	want := expandComponent(component)
	out := parts[:0]
	for _, p := range parts {
		if expandComponent(p) != want {
			out = append(out, p)
		}
	}
	return joinServices(out)
}

func splitServices(list string) []string {
	list = strings.TrimSpace(list)
	if list == "" || list == "null" {
		return nil
	}
	return strings.Split(list, ":")
}

func joinServices(parts []string) string {
	return strings.Join(parts, ":")
}
