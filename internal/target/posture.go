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
func postureField(emu emuFunc) settingField {
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
				return setPosture(ctx, sh, emu, cur)
			}, nil
		},
		write: func(ctx context.Context, sh shellFunc, v any) error {
			return setPosture(ctx, sh, emu, v.(string))
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
