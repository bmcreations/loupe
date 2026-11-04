// Package a11ydump carries the device half of loupe's Android tree dump: a
// dex (tools/a11ydump) that app_process runs to print the screen's
// accessibility tree as uiautomator dump's XML, in about 250ms where
// uiautomator takes 2s, with each scrollable node's scroll-backward and
// scroll-forward actions added.
package a11ydump

import (
	_ "embed"
)

// SHA256 pins the committed `make a11ydump` output. A rebuild that changes
// it fails TestDexIsPinned until repinned.
const SHA256 = "3eeba8edddc4397d214767adadec3e091d2e752a6c888b17a789ca2ed3196f27"

// Class is the entry point app_process runs.
const Class = "dev.loupe.a11y.Dump"

// ExitBusy is the dump's exit status when another UiAutomation client, such
// as uiautomator or Maestro, holds the device.
const ExitBusy = 3

// DeviceName is the dex's file name on the device. It carries the hash so a
// device keeps no stale copy across loupe upgrades.
var DeviceName = "loupe-a11y-" + SHA256[:12] + ".dex"

//go:embed loupe-a11y.dex
var dex []byte

// Dex returns the dex to push.
func Dex() []byte { return dex }
