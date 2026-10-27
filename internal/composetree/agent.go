// Package composetree reads the composable tree out of a debuggable Compose
// app and maps it onto the uiautomator tree loupe already serves.
//
// The device side is a JVMTI agent (tools/composeagent) that hosts Google's
// Compose inspector, the one Android Studio's Layout Inspector uses, from the
// app's own ui-android version. Design and spike results:
// docs/superpowers/specs/2026-10-24-compose-tree-design.md.
package composetree

import (
	"embed"
	"fmt"
)

// DexName is the agent dex's file name, on the host and on the device.
const DexName = "loupe-compose.dex"

// SHA256 pins the committed `make composeagent` output, by file name. A
// rebuild that changes a file fails TestAgentFilesArePinned until repinned.
var SHA256 = map[string]string{
	"libloupecompose-arm64-v8a.so": "03f322fad15ad394e3daa2f972bead68fa6a81d52c858f512c16ef40cee7ac0b",
	"libloupecompose-x86_64.so":    "e115087ba8788e2a0c8cec109905f18185c9a1c43e631b629f7e121e2b248a9d",
	DexName:                        "ec793151b05014fa40c6e5f46033aa0f80b2b38de65293091044798158df4f38",
}

//go:embed agent/*
var agent embed.FS

// SOName is the agent library's file name for an ABI.
func SOName(abi string) string { return "libloupecompose-" + abi + ".so" }

// AgentFiles returns the agent library for abi and the dex it loads.
func AgentFiles(abi string) (so, dex []byte, err error) {
	if _, ok := SHA256[SOName(abi)]; !ok {
		return nil, nil, fmt.Errorf("compose agent: unsupported ABI %q", abi)
	}
	if so, err = agent.ReadFile("agent/" + SOName(abi)); err != nil {
		return nil, nil, err
	}
	if dex, err = agent.ReadFile("agent/" + DexName); err != nil {
		return nil, nil, err
	}
	return so, dex, nil
}
