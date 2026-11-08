package target

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// emuFunc runs one `adb emu` console command against an emulator and returns
// its reply. It is nil on anything that is not an emulator.
type emuFunc func(ctx context.Context, args ...string) (string, error)

// postureEmuIDs maps a posture value to the emulator console's
// `posture <id>`. The console also has flipped (4) and tent (5); neither is a
// device state the Pixel Fold AVD declares, so neither is offered.
//
// The console moves the hinge sensor, and the device's base state follows it,
// the same as folding a real device. `cmd device_state state N` was the other
// candidate and was not used: it is an override, which holds until something
// resets it, so a fold from Android Studio's extended controls would do
// nothing while it stood.
var postureEmuIDs = map[string]int{"closed": 1, "halfOpened": 2, "opened": 3}

// postureEmuDegrees is the hinge angle each console posture moves the
// sensor to, the angle a posture write is queued under.
var postureEmuDegrees = map[string]int{"closed": 0, "halfOpened": 90, "opened": 180}

// postureOrder is the order Options lists them in, closed to open.
var postureOrder = []string{"closed", "halfOpened", "opened"}

// postureByStateName maps a DeviceState name from `cmd device_state
// print-states` to a posture value.
var postureByStateName = map[string]string{"CLOSED": "closed", "HALF_OPENED": "halfOpened", "OPENED": "opened"}

// postureStateRe reads one DeviceState line of print-states:
//
//	DeviceState{identifier=1, name='HALF_OPENED', app_accessible=true, ...}
var postureStateRe = regexp.MustCompile(`identifier=(\d+), name='([A-Z_]+)'`)

// parsePostureStates returns the posture of each device state id, and the
// postures on offer in postureOrder. A device that is not foldable lists one
// state, DEFAULT, and gets no options.
func parsePostureStates(printStates string) (byID map[int]string, options []string) {
	byID = map[int]string{}
	for _, m := range postureStateRe.FindAllStringSubmatch(printStates, -1) {
		id, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		if p, ok := postureByStateName[m[2]]; ok {
			byID[id] = p
		}
	}
	have := map[string]bool{}
	for _, p := range byID {
		have[p] = true
	}
	for _, p := range postureOrder {
		if have[p] {
			options = append(options, p)
		}
	}
	return byID, options
}

// postureSupported is the rule Settings and ApplySettings share: two postures
// or more, so there is something to change between.
func postureSupported(options []string) bool { return len(options) >= 2 }

// currentPosture reads the committed device state and names its posture, or
// "" when it is a state with no posture here (REAR_DISPLAY_MODE).
func currentPosture(printState string, byID map[int]string) string {
	id, err := strconv.Atoi(strings.TrimSpace(printState))
	if err != nil {
		return ""
	}
	return byID[id]
}

func readPosture(ctx context.Context, sh shellFunc) (current string, options []string, err error) {
	states, err := sh(ctx, "cmd", "device_state", "print-states")
	if err != nil {
		return "", nil, fmt.Errorf("print-states: %w", err)
	}
	state, err := sh(ctx, "cmd", "device_state", "print-state")
	if err != nil {
		return "", nil, fmt.Errorf("print-state: %w", err)
	}
	byID, options := parsePostureStates(states)
	return currentPosture(state, byID), options, nil
}

// setPosture sends the console command and waits for the committed state to
// follow, so the re-read a PUT answers with shows the new posture rather than
// the one it replaced. The console answers "OK" or "KO: <why>" and adb exits
// 0 either way, so the reply is the verdict.
func setPosture(ctx context.Context, sh shellFunc, emu emuFunc, posture string) error {
	id, ok := postureEmuIDs[posture]
	if !ok {
		return fmt.Errorf("unknown posture %q", posture)
	}
	out, err := emu(ctx, "posture", strconv.Itoa(id))
	if err != nil {
		return fmt.Errorf("emu posture: %w", err)
	}
	if reply := strings.TrimSpace(out); !strings.HasPrefix(reply, "OK") {
		return fmt.Errorf("emu posture: %s", reply)
	}
	deadline := time.Now().Add(postureSettleTimeout)
	for {
		cur, _, err := readPosture(ctx, sh)
		if err == nil && cur == posture {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("posture still %q after %v", cur, postureSettleTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(posturePollInterval):
		}
	}
}

// postureSettleTimeout bounds the wait for the committed state. On
// Hinge_Pixel_Fold the state followed the console within one 100ms poll.
var (
	postureSettleTimeout = 3 * time.Second
	posturePollInterval  = 100 * time.Millisecond
)

// postureField is the posture row for an emulator. It is built per target
// rather than listed in settingFields because its write goes through the
// emulator console, which the shared table's fields have no way to reach.
// The write, settle wait included, goes through the target's hinge
// coalescer, so a hingeAngle write cannot land in the middle of it.
func postureField(emu emuFunc, hinge *hingeCoalescer) settingField {
	set := func(ctx context.Context, sh shellFunc, posture string) error {
		return moveHinge(ctx, hinge, postureEmuDegrees[posture], func(ctx context.Context, _ int) error {
			ctx, cancel := context.WithTimeout(ctx, emuHingeWriteTimeout+postureSettleTimeout)
			defer cancel()
			return setPosture(ctx, sh, emu, posture)
		})
	}
	return settingField{
		id:  "posture",
		typ: SettingEnum,
		readRestore: func(ctx context.Context, sh shellFunc) (func(ctx context.Context, sh shellFunc) error, error) {
			cur, _, err := readPosture(ctx, sh)
			if err != nil {
				return nil, err
			}
			if cur == "" {
				return nil, fmt.Errorf("the current device state has no posture to restore")
			}
			return func(ctx context.Context, sh shellFunc) error {
				return set(ctx, sh, cur)
			}, nil
		},
		write: func(ctx context.Context, sh shellFunc, v any) error {
			return set(ctx, sh, v.(string))
		},
		validate: func(_ settingField, v any) error {
			s, ok := v.(string)
			if !ok {
				return fmt.Errorf("want a string, got %T", v)
			}
			if _, ok := postureEmuIDs[s]; !ok {
				return fmt.Errorf("posture %q is not one of %v", s, postureOrder)
			}
			return nil
		},
	}
}

// emuHingeSensor is the emulator's first hinge sensor. A Pixel Fold AVD has
// one hinge (hw.sensor.hinge.count=1) and lists hinge-angle1 and 2 as
// disabled in `sensor status`.
const emuHingeSensor = "hinge-angle0"

// hingeSensorRe reads `sensor get hinge-angle0`, which answers
//
//	hinge-angle0 = 120
//	OK
//
// and a fraction (33.5) when one was set from elsewhere.
var hingeSensorRe = regexp.MustCompile(`(?m)^` + emuHingeSensor + ` = (-?[0-9.]+)\s*$`)

// parseHingeSensor reads the angle out of a `sensor get` reply. The console
// does not clamp, so a value outside 0-180 set from elsewhere is passed
// through as it is rather than failing the read.
func parseHingeSensor(out string) (float64, error) {
	m := hingeSensorRe.FindStringSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("sensor get %s: %s", emuHingeSensor, strings.TrimSpace(out))
	}
	return strconv.ParseFloat(m[1], 64)
}

func readHingeAngle(ctx context.Context, emu emuFunc) (float64, error) {
	out, err := emu(ctx, "sensor", "get", emuHingeSensor)
	if err != nil {
		return 0, fmt.Errorf("emu sensor get: %w", err)
	}
	return parseHingeSensor(out)
}

// emuHingeWriteTimeout bounds one console write that moves the hinge. A
// write takes 13-65 ms; hingeWriteTimeout is sized for the Duo. A coalesced
// write runs after its request has returned, and with it the Emulator's
// in-use hold, so the pause timer can stop the VM under it; this keeps such
// a write from holding up the writes queued behind it for long.
const emuHingeWriteTimeout = 2 * time.Second

// writeHingeAngle sets the hinge sensor and returns without waiting for the
// device state to follow: on pixel_fold_hinge the console answered in about
// 65ms and print-state had moved by the next read, except at the edge of a
// band (179 read HALF_OPENED at once and OPENED half a second later). An
// emulator with no hinge answers "KO: unknown sensor name", which is the
// caller's mistake, not the device's.
func writeHingeAngle(ctx context.Context, emu emuFunc, deg float64) error {
	out, err := emu(ctx, "sensor", "set", emuHingeSensor, strconv.FormatFloat(deg, 'f', -1, 64))
	if err != nil {
		return fmt.Errorf("emu sensor set: %w", err)
	}
	reply := strings.TrimSpace(out)
	if strings.HasPrefix(reply, "OK") {
		return nil
	}
	if strings.Contains(reply, "unknown sensor name") {
		return fmt.Errorf("%w: %s: this emulator has no hinge", ErrInvalidSetting, hingeAngleID)
	}
	return fmt.Errorf("emu sensor set: %s", reply)
}

// hingeAngleField is the hingeAngle row for an emulator, beside postureField.
// Its rollback replays the angle read before the write, fraction and all.
// Both go through the target's hinge coalescer, as the fast path does.
func hingeAngleField(emu emuFunc, hinge *hingeCoalescer) settingField {
	set := func(ctx context.Context, deg float64) error {
		return moveHinge(ctx, hinge, int(deg), func(ctx context.Context, _ int) error {
			ctx, cancel := context.WithTimeout(ctx, emuHingeWriteTimeout)
			defer cancel()
			return writeHingeAngle(ctx, emu, deg)
		})
	}
	return settingField{
		id:  hingeAngleID,
		typ: SettingRange,
		min: hingeAngleMin, max: hingeAngleMax, step: hingeAngleStep,
		readRestore: func(ctx context.Context, _ shellFunc) (func(ctx context.Context, sh shellFunc) error, error) {
			cur, err := readHingeAngle(ctx, emu)
			if err != nil {
				return nil, err
			}
			return func(ctx context.Context, _ shellFunc) error {
				return set(ctx, cur)
			}, nil
		},
		write: func(ctx context.Context, _ shellFunc, v any) error {
			return set(ctx, v.(float64))
		},
		validate: func(_ settingField, v any) error {
			_, err := parseHingeAngle(v)
			return err
		},
	}
}
