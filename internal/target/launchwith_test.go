package target

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// launchFiles makes a .so and a .dex to point a spec at, and a directory.
func launchFiles(t *testing.T) (so, dex, dir string) {
	t.Helper()
	dir = t.TempDir()
	so, dex = filepath.Join(dir, "agent.so"), filepath.Join(dir, "agent.dex")
	for _, p := range []string{so, dex} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return so, dex, dir
}

func TestLaunchExtensionValidateAccepts(t *testing.T) {
	so, dex, _ := launchFiles(t)
	for name, c := range map[string]struct {
		platform string
		ext      LaunchExtension
	}{
		"ios env": {LaunchIOSSimulator, LaunchExtension{Name: "ext", Env: map[string]string{
			InsertLibrariesKey:            so + ":" + dex,
			"EXT_SERVER":                  "http://127.0.0.1:18492",
			"A":                           "",
			"K" + strings.Repeat("X", 63): strings.Repeat("v", maxLaunchEnvValueLen),
		}}},
		"android agent and reverse": {LaunchAndroid, LaunchExtension{
			Name:    "ext",
			Agent:   &LaunchAgent{SO: so, Dex: dex, Options: map[string]string{"runtime": "@dex", "server": "local:ext-sock", "a_1": "A-z.0_9:/@"}},
			Reverse: "ext-sock", ReversePort: 18492,
		}},
		"android agent alone":   {LaunchAndroid, LaunchExtension{Name: "ext", Agent: &LaunchAgent{SO: so, Dex: dex}}},
		"android reverse alone": {LaunchAndroid, LaunchExtension{Name: "ext", Reverse: "s", ReversePort: 1}},
	} {
		if err := c.ext.Validate(c.platform); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Every check in docs/extensions.md, "Launch-time injection", refuses with
// ErrBadLaunchSpec, so no spec that fails one becomes a plain launch.
func TestLaunchExtensionValidateRefuses(t *testing.T) {
	so, dex, dir := launchFiles(t)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	many := map[string]string{}
	for i := range maxLaunchEnvKeys + 1 {
		many["K"+strings.Repeat("A", i)] = "v"
	}
	agent := func(mod func(*LaunchAgent)) *LaunchAgent {
		a := &LaunchAgent{SO: so, Dex: dex, Options: map[string]string{"runtime": "@dex"}}
		mod(a)
		return a
	}
	env := func(k, v string) LaunchExtension {
		return LaunchExtension{Name: "ext", Env: map[string]string{k: v}}
	}
	for name, c := range map[string]struct {
		platform string
		ext      LaunchExtension
	}{
		// What the extension asked for has to fit the platform.
		"ios nothing":          {LaunchIOSSimulator, LaunchExtension{Name: "ext"}},
		"ios agent":            {LaunchIOSSimulator, LaunchExtension{Name: "ext", Env: map[string]string{"A": "b"}, Agent: &LaunchAgent{SO: so, Dex: dex}}},
		"ios reverse":          {LaunchIOSSimulator, LaunchExtension{Name: "ext", Env: map[string]string{"A": "b"}, Reverse: "s", ReversePort: 1}},
		"android nothing":      {LaunchAndroid, LaunchExtension{Name: "ext"}},
		"android env":          {LaunchAndroid, LaunchExtension{Name: "ext", Env: map[string]string{"A": "b"}, Reverse: "s", ReversePort: 1}},
		"unknown platform":     {"windows", LaunchExtension{Name: "ext", Env: map[string]string{"A": "b"}}},
		"bad extension name":   {LaunchIOSSimulator, LaunchExtension{Name: "../x", Env: map[string]string{"A": "b"}}},
		"empty extension name": {LaunchIOSSimulator, LaunchExtension{Env: map[string]string{"A": "b"}}},
		// env keys ^[A-Z][A-Z0-9_]{0,63}$
		"env key lowercase":      {LaunchIOSSimulator, env("lower", "v")},
		"env key digit first":    {LaunchIOSSimulator, env("1A", "v")},
		"env key underscore 1st": {LaunchIOSSimulator, env("_A", "v")},
		"env key dash":           {LaunchIOSSimulator, env("A-B", "v")},
		"env key equals":         {LaunchIOSSimulator, env("A=B", "v")},
		"env key 65 chars":       {LaunchIOSSimulator, env("K"+strings.Repeat("X", 64), "v")},
		"env key loupe's":        {LaunchIOSSimulator, env(ReservedLaunchEnv, "/tmp/x.png")},
		// values: no NUL or newline, at most 1024 bytes
		"env value NUL":        {LaunchIOSSimulator, env("A", "a\x00b")},
		"env value newline":    {LaunchIOSSimulator, env("A", "a\nb")},
		"env value CR":         {LaunchIOSSimulator, env("A", "a\rb")},
		"env value 1025 bytes": {LaunchIOSSimulator, env("A", strings.Repeat("v", maxLaunchEnvValueLen+1))},
		// at most 16 keys
		"17 env keys": {LaunchIOSSimulator, LaunchExtension{Name: "ext", Env: many}},
		// DYLD_INSERT_LIBRARIES: each part an absolute path to a regular file
		"insert relative":     {LaunchIOSSimulator, env(InsertLibrariesKey, "loader.dylib")},
		"insert missing":      {LaunchIOSSimulator, env(InsertLibrariesKey, filepath.Join(dir, "missing.dylib"))},
		"insert directory":    {LaunchIOSSimulator, env(InsertLibrariesKey, dir)},
		"insert dir via link": {LaunchIOSSimulator, env(InsertLibrariesKey, link)},
		"insert unclean":      {LaunchIOSSimulator, env(InsertLibrariesKey, dir+"/../"+filepath.Base(dir)+"/agent.so")},
		"insert one bad part": {LaunchIOSSimulator, env(InsertLibrariesKey, so+":relative.dylib")},
		"insert empty part":   {LaunchIOSSimulator, env(InsertLibrariesKey, so+"::"+dex)},
		"insert empty":        {LaunchIOSSimulator, env(InsertLibrariesKey, "")},
		// agent so and dex: absolute, existing regular files
		"so relative":   {LaunchAndroid, LaunchExtension{Name: "ext", Agent: agent(func(a *LaunchAgent) { a.SO = "agent.so" })}},
		"so missing":    {LaunchAndroid, LaunchExtension{Name: "ext", Agent: agent(func(a *LaunchAgent) { a.SO = filepath.Join(dir, "none.so") })}},
		"so directory":  {LaunchAndroid, LaunchExtension{Name: "ext", Agent: agent(func(a *LaunchAgent) { a.SO = dir })}},
		"so empty":      {LaunchAndroid, LaunchExtension{Name: "ext", Agent: agent(func(a *LaunchAgent) { a.SO = "" })}},
		"dex relative":  {LaunchAndroid, LaunchExtension{Name: "ext", Agent: agent(func(a *LaunchAgent) { a.Dex = "agent.dex" })}},
		"dex missing":   {LaunchAndroid, LaunchExtension{Name: "ext", Agent: agent(func(a *LaunchAgent) { a.Dex = filepath.Join(dir, "none.dex") })}},
		"dex directory": {LaunchAndroid, LaunchExtension{Name: "ext", Agent: agent(func(a *LaunchAgent) { a.Dex = dir })}},
		"dex empty":     {LaunchAndroid, LaunchExtension{Name: "ext", Agent: agent(func(a *LaunchAgent) { a.Dex = "" })}},
		// options: keys ^[a-z][a-z0-9_]{0,31}$
		"option key upper":     {LaunchAndroid, LaunchExtension{Name: "ext", Agent: agent(func(a *LaunchAgent) { a.Options = map[string]string{"Runtime": "x"} })}},
		"option key digit 1st": {LaunchAndroid, LaunchExtension{Name: "ext", Agent: agent(func(a *LaunchAgent) { a.Options = map[string]string{"1a": "x"} })}},
		"option key dash":      {LaunchAndroid, LaunchExtension{Name: "ext", Agent: agent(func(a *LaunchAgent) { a.Options = map[string]string{"a-b": "x"} })}},
		"option key 33 chars":  {LaunchAndroid, LaunchExtension{Name: "ext", Agent: agent(func(a *LaunchAgent) { a.Options = map[string]string{"a" + strings.Repeat("b", 32): "x"} })}},
		// option values ^[A-Za-z0-9._:/@-]{1,200}$: no comma, =, quote, space or ;
		"option value comma":  {LaunchAndroid, LaunchExtension{Name: "ext", Agent: agent(func(a *LaunchAgent) { a.Options = map[string]string{"a": "x,y"} })}},
		"option value equals": {LaunchAndroid, LaunchExtension{Name: "ext", Agent: agent(func(a *LaunchAgent) { a.Options = map[string]string{"a": "x=y"} })}},
		"option value quote":  {LaunchAndroid, LaunchExtension{Name: "ext", Agent: agent(func(a *LaunchAgent) { a.Options = map[string]string{"a": "x'y"} })}},
		"option value dquote": {LaunchAndroid, LaunchExtension{Name: "ext", Agent: agent(func(a *LaunchAgent) { a.Options = map[string]string{"a": `x"y`} })}},
		"option value space":  {LaunchAndroid, LaunchExtension{Name: "ext", Agent: agent(func(a *LaunchAgent) { a.Options = map[string]string{"a": "x y"} })}},
		"option value semi":   {LaunchAndroid, LaunchExtension{Name: "ext", Agent: agent(func(a *LaunchAgent) { a.Options = map[string]string{"a": "x;y"} })}},
		"option value dollar": {LaunchAndroid, LaunchExtension{Name: "ext", Agent: agent(func(a *LaunchAgent) { a.Options = map[string]string{"a": "$(id)"} })}},
		"option value empty":  {LaunchAndroid, LaunchExtension{Name: "ext", Agent: agent(func(a *LaunchAgent) { a.Options = map[string]string{"a": ""} })}},
		"option value 201":    {LaunchAndroid, LaunchExtension{Name: "ext", Agent: agent(func(a *LaunchAgent) { a.Options = map[string]string{"a": strings.Repeat("v", 201)} })}},
		// reverse ^[a-z][a-z0-9-]{0,31}$, and a port loupe found
		"reverse upper":        {LaunchAndroid, LaunchExtension{Name: "ext", Reverse: "Sock", ReversePort: 1}},
		"reverse digit first":  {LaunchAndroid, LaunchExtension{Name: "ext", Reverse: "1sock", ReversePort: 1}},
		"reverse colon":        {LaunchAndroid, LaunchExtension{Name: "ext", Reverse: "tcp:80", ReversePort: 1}},
		"reverse 33 chars":     {LaunchAndroid, LaunchExtension{Name: "ext", Reverse: "s" + strings.Repeat("o", 32), ReversePort: 1}},
		"reverse no port":      {LaunchAndroid, LaunchExtension{Name: "ext", Reverse: "sock"}},
		"reverse port too big": {LaunchAndroid, LaunchExtension{Name: "ext", Reverse: "sock", ReversePort: 65536}},
	} {
		err := c.ext.Validate(c.platform)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !errors.Is(err, ErrBadLaunchSpec) {
			t.Errorf("%s: %v is not ErrBadLaunchSpec", name, err)
		}
	}
}
