package target

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// foldStates is print-states as Hinge_Pixel_Fold answered it.
const foldStates = `Supported states: [
  DeviceState{identifier=0, name='CLOSED', app_accessible=true, cancel_when_requester_not_on_top=false},
  DeviceState{identifier=1, name='HALF_OPENED', app_accessible=true, cancel_when_requester_not_on_top=false},
  DeviceState{identifier=2, name='OPENED', app_accessible=true, cancel_when_requester_not_on_top=false},
  DeviceState{identifier=3, name='REAR_DISPLAY_MODE', app_accessible=true, cancel_when_requester_not_on_top=false},
]`

// flatStates is print-states on emulator-5554 (Pixel_10) and on an Acme One.
const flatStates = `Supported states: [
  DeviceState{identifier=0, name='DEFAULT', app_accessible=true, cancel_when_requester_not_on_top=false},
]`

// fakeFold is an emulator whose committed state follows `emu posture` the
// way Hinge_Pixel_Fold's did, so a write can be read back.
type fakeFold struct {
	states  string
	state   int    // committed state id
	reply   string // console reply; "" means OK
	hinge   float64
	noHinge bool // the console has no hinge-angle0
	emus    []string
	failing map[string]error
}

func (f *fakeFold) sh(ctx context.Context, args ...string) (string, error) {
	joined := joinArgs(args)
	if err, ok := f.failing[joined]; ok {
		return "", err
	}
	if len(args) == 1 {
		if nonce, ok := extractNonce(args[0]); ok {
			sections := []string{"Night mode: no", "1.0", "1.0", "1.0", "1.0", "false", "null", "package:/x.apk"}
			for _, c := range scriptCommands(args[0])[len(sections):] {
				out, _ := f.sh(ctx, strings.Fields(c)...)
				sections = append(sections, out)
			}
			resp, _ := nonceFakeResponse(nonce, sections)
			return resp, nil
		}
	}
	switch joined {
	case "cmd device_state print-states":
		return f.states, nil
	case "cmd device_state print-state":
		return string(rune('0' + f.state)), nil
	}
	return "", nil
}

func (f *fakeFold) emu(_ context.Context, args ...string) (string, error) {
	joined := joinArgs(args)
	f.emus = append(f.emus, joined)
	if f.reply != "" {
		return f.reply, nil
	}
	if strings.HasPrefix(joined, "sensor ") && f.noHinge {
		return "KO: unknown sensor name: hinge-angle0, run 'sensor status' to get available sensors.\n", nil
	}
	if v, ok := strings.CutPrefix(joined, "sensor set hinge-angle0 "); ok {
		deg, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return "KO: bad value\n", nil
		}
		// The bands are pixel_fold's hw.sensor.hinge_angles_posture_definitions.
		f.hinge = deg
		switch {
		case deg < 30:
			f.state = 0
		case deg < 150:
			f.state = 1
		default:
			f.state = 2
		}
		return "OK\n", nil
	}
	if joined == "sensor get hinge-angle0" {
		return fmt.Sprintf("hinge-angle0 = %g\nOK\n", f.hinge), nil
	}
	// The console's posture ids are one more than the fold's state ids, and
	// each moves the hinge sensor to the middle of its band.
	switch joined {
	case "posture 1":
		f.state, f.hinge = 0, 0
	case "posture 2":
		f.state, f.hinge = 1, 90
	case "posture 3":
		f.state, f.hinge = 2, 180
	}
	return "OK\n", nil
}

func (f *fakeFold) settings() *androidSettings { return &androidSettings{sh: f.sh, emu: f.emu} }

func byID(settings []Setting) map[string]Setting {
	m := map[string]Setting{}
	for _, s := range settings {
		m[s.ID] = s
	}
	return m
}

func TestParsePostureStatesSkipsStatesWithNoPosture(t *testing.T) {
	ids, options := parsePostureStates(foldStates)
	if got := strings.Join(options, ","); got != "closed,halfOpened,opened" {
		t.Fatalf("options = %s", got)
	}
	if _, ok := ids[3]; ok {
		t.Fatalf("REAR_DISPLAY_MODE read as a posture: %v", ids)
	}
	if _, options := parsePostureStates(flatStates); postureSupported(options) {
		t.Fatalf("a DEFAULT-only device reads as foldable: %v", options)
	}
}

func TestSettingsListsPostureOnAFoldableEmulatorOnly(t *testing.T) {
	fold := &fakeFold{states: foldStates, state: 1}
	got, err := fold.settings().Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p, ok := byID(got)["posture"]
	if !ok || p.Value != "halfOpened" || p.Type != SettingEnum || !p.Supported || len(p.Options) != 3 {
		t.Fatalf("posture = %+v", p)
	}

	flat := &fakeFold{states: flatStates}
	got, err = flat.settings().Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := byID(got)["posture"]; ok {
		t.Fatal("a phone that does not fold lists posture")
	}

	// A device that is not an emulator never runs the device_state reads.
	f := &fakeShell{scriptAnswers: []string{"Night mode: no", "1.0", "1.0", "1.0", "1.0", "false", "null", "package:/x.apk"}}
	got, err = newFakeSettings(f).Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := byID(got)["posture"]; ok {
		t.Fatal("a device with no console lists posture")
	}
}

func TestApplyPostureSendsTheConsoleCommandAndWaitsForIt(t *testing.T) {
	fold := &fakeFold{states: foldStates, state: 0}
	if _, _, err := fold.settings().ApplySettings(context.Background(), map[string]any{"posture": "opened"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fold.emus, ";") != "posture 3" || fold.state != 2 {
		t.Fatalf("emus %v, state %d", fold.emus, fold.state)
	}
}

func TestApplyPostureRefusesBeforeWriting(t *testing.T) {
	cases := map[string]struct {
		states string
		value  any
	}{
		"not a posture":        {foldStates, "tent"},
		"not a string":         {foldStates, 2},
		"a phone that is flat": {flatStates, "closed"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			fold := &fakeFold{states: c.states}
			_, _, err := fold.settings().ApplySettings(context.Background(), map[string]any{"posture": c.value})
			if !errors.Is(err, ErrInvalidSetting) {
				t.Fatalf("err = %v, want ErrInvalidSetting", err)
			}
			if len(fold.emus) != 0 {
				t.Fatalf("console was sent %v", fold.emus)
			}
		})
	}
}

// The console answers KO in-band with adb exiting 0, so a KO must fail the
// write rather than wait out the settle timeout and call it slow.
func TestApplyPostureFailsOnAKOReply(t *testing.T) {
	fold := &fakeFold{states: foldStates, state: 0, reply: "KO: Posture 2 not supported\n"}
	_, _, err := fold.settings().ApplySettings(context.Background(), map[string]any{"posture": "halfOpened"})
	if err == nil || !strings.Contains(err.Error(), "KO: Posture 2 not supported") {
		t.Fatalf("err = %v", err)
	}
}

// A later field failing rolls posture back to where it was, through the
// console again.
func TestPostureRollsBackToThePostureItFound(t *testing.T) {
	oldTimeout := postureSettleTimeout
	postureSettleTimeout = 50 * time.Millisecond
	defer func() { postureSettleTimeout = oldTimeout }()

	fold := &fakeFold{states: foldStates, state: 1}
	a := fold.settings()
	// posture is last in table order, so make it the one that fails after
	// writing: the console takes the command and the state never follows.
	fold.reply = "OK\n"
	failed, notRestored, err := a.ApplySettings(context.Background(), map[string]any{"posture": "closed"})
	if err == nil || len(failed) != 1 || failed[0] != "posture" {
		t.Fatalf("failed %v, err %v", failed, err)
	}
	// The restore replays halfOpened, and the fold never left it, so the
	// read-back confirms the restore.
	if len(notRestored) != 0 {
		t.Fatalf("notRestored %v", notRestored)
	}
	if strings.Join(fold.emus, ";") != "posture 1;posture 2" {
		t.Fatalf("emus %v", fold.emus)
	}
}

// The replies are what pixel_fold_hinge (API 36) answered.
func TestParseHingeSensor(t *testing.T) {
	cases := map[string]struct {
		out  string
		want float64
		ok   bool
	}{
		"whole":        {"hinge-angle0 = 120\nOK\n", 120, true},
		"crlf":         {"hinge-angle0 = 0\r\nOK\r\n", 0, true},
		"fraction":     {"hinge-angle0 = 33.5\nOK\n", 33.5, true},
		"out of range": {"hinge-angle0 = 250\nOK\n", 250, true},
		"no sensor":    {"KO: unknown sensor name: hinge-angle0, run 'sensor status' to get available sensors.\n", 0, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := parseHingeSensor(c.out)
			if (err == nil) != c.ok || got != c.want {
				t.Fatalf("parseHingeSensor = %v, %v", got, err)
			}
		})
	}
}

func TestSettingsListsHingeAngleBesidePosture(t *testing.T) {
	fold := &fakeFold{states: foldStates, state: 1, hinge: 90}
	got, err := fold.settings().Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h, ok := byID(got)[hingeAngleID]
	if !ok || h.Type != SettingRange || h.Value != 90.0 || h.Min != 0 || h.Max != 180 || h.Step != 1 || !h.Supported {
		t.Fatalf("hingeAngle = %+v", h)
	}
	if fs := byID(got)["fontScale"]; fs.Step != 0.05 {
		t.Fatalf("fontScale step = %v", fs.Step)
	}

	flat := &fakeFold{states: flatStates}
	got, _ = flat.settings().Settings(context.Background())
	if _, ok := byID(got)[hingeAngleID]; ok {
		t.Fatal("a phone that does not fold lists hingeAngle")
	}
	if len(flat.emus) != 0 {
		t.Fatalf("a flat phone's console was sent %v", flat.emus)
	}

	// A console that cannot read the hinge drops the row, not the read.
	odd := &fakeFold{states: foldStates, state: 2, noHinge: true}
	got, err = odd.settings().Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := byID(got)[hingeAngleID]; ok {
		t.Fatal("hingeAngle listed with no sensor to read")
	}
	if _, ok := byID(got)["posture"]; !ok {
		t.Fatal("posture dropped with the hinge")
	}
}

// A hinge write moves the posture too, and posture reads it back: the two
// are one hinge.
func TestApplyHingeAngleMovesPosture(t *testing.T) {
	fold := &fakeFold{states: foldStates, state: 2, hinge: 180}
	a := fold.settings()
	if _, _, err := a.ApplySettings(context.Background(), map[string]any{hingeAngleID: 45.0}); err != nil {
		t.Fatal(err)
	}
	got, _ := a.Settings(context.Background())
	if p, h := byID(got)["posture"].Value, byID(got)[hingeAngleID].Value; p != "halfOpened" || h != 45.0 {
		t.Fatalf("posture %v, hingeAngle %v", p, h)
	}
	if _, _, err := a.ApplySettings(context.Background(), map[string]any{"posture": "closed"}); err != nil {
		t.Fatal(err)
	}
	got, _ = a.Settings(context.Background())
	if h := byID(got)[hingeAngleID].Value; h != 0.0 {
		t.Fatalf("after posture closed, hingeAngle %v", h)
	}
}

func TestApplyHingeAngleRefusesBeforeWriting(t *testing.T) {
	cases := map[string]map[string]any{
		"too far":        {hingeAngleID: 181.0},
		"negative":       {hingeAngleID: -1.0},
		"a fraction":     {hingeAngleID: 90.5},
		"not a number":   {hingeAngleID: "90"},
		"with a posture": {hingeAngleID: 90.0, "posture": "opened"},
	}
	for name, changes := range cases {
		t.Run(name, func(t *testing.T) {
			fold := &fakeFold{states: foldStates, hinge: 180}
			_, _, err := fold.settings().ApplySettings(context.Background(), changes)
			if !errors.Is(err, ErrInvalidSetting) {
				t.Fatalf("err = %v, want ErrInvalidSetting", err)
			}
			if len(fold.emus) != 0 {
				t.Fatalf("console was sent %v", fold.emus)
			}
		})
	}
}

// An emulator with no hinge says so in-band; that is the caller asking for
// something the device lacks, the same 400 a flat phone's posture gets.
func TestWriteHingeAngleOnAnEmulatorWithNoHinge(t *testing.T) {
	fold := &fakeFold{noHinge: true}
	if err := writeHingeAngle(context.Background(), fold.emu, 90); !errors.Is(err, ErrInvalidSetting) {
		t.Fatalf("err = %v, want ErrInvalidSetting", err)
	}
	fold = &fakeFold{reply: "KO: something else\n"}
	if err := writeHingeAngle(context.Background(), fold.emu, 90); err == nil || errors.Is(err, ErrInvalidSetting) {
		t.Fatalf("err = %v, want a device error", err)
	}
}
