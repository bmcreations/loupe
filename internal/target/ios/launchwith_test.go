package ios

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

// dylibs makes n files to name in DYLD_INSERT_LIBRARIES.
func dylibs(t *testing.T, n int) []string {
	t.Helper()
	dir := t.TempDir()
	var out []string
	for i := range n {
		p := filepath.Join(dir, string(rune('a'+i))+".dylib")
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// The camera shim's insert and file stay in the launch, with the
// extensions' libraries after the shim's rather than in its place.
func TestSimLaunchEnvJoinsTheCameraShim(t *testing.T) {
	const shim, file = "/c/loupecam-0123456789ab.dylib", "/c/sim-UDID.png"
	for _, tc := range []struct {
		name         string
		shim, camera string
		exts         []target.LaunchExtension
		want         []string
	}{
		{
			name: "camera staged",
			shim: shim, camera: file,
			exts: []target.LaunchExtension{{Name: "one", Env: map[string]string{"DYLD_INSERT_LIBRARIES": "/x/loader.dylib", "ONE_SERVER": "http://127.0.0.1:1"}}},
			want: []string{
				"SIMCTL_CHILD_DYLD_INSERT_LIBRARIES=" + shim + ":/x/loader.dylib",
				"SIMCTL_CHILD_LOUPE_CAMERA_FILE=" + file,
				"SIMCTL_CHILD_ONE_SERVER=http://127.0.0.1:1",
			},
		},
		{
			name: "no camera",
			exts: []target.LaunchExtension{{Name: "one", Env: map[string]string{"DYLD_INSERT_LIBRARIES": "/x/loader.dylib", "ONE_SERVER": "s"}}},
			want: []string{
				"SIMCTL_CHILD_DYLD_INSERT_LIBRARIES=/x/loader.dylib",
				"SIMCTL_CHILD_ONE_SERVER=s",
			},
		},
		{
			name: "camera, extension inserts nothing",
			shim: shim, camera: file,
			exts: []target.LaunchExtension{{Name: "one", Env: map[string]string{"ONE_SERVER": "s"}}},
			want: []string{
				"SIMCTL_CHILD_DYLD_INSERT_LIBRARIES=" + shim,
				"SIMCTL_CHILD_LOUPE_CAMERA_FILE=" + file,
				"SIMCTL_CHILD_ONE_SERVER=s",
			},
		},
		{
			name: "two extensions, in with order, a shared library once",
			shim: shim, camera: file,
			exts: []target.LaunchExtension{
				{Name: "one", Env: map[string]string{"DYLD_INSERT_LIBRARIES": "/x/a.dylib:/x/shared.dylib"}},
				{Name: "two", Env: map[string]string{"DYLD_INSERT_LIBRARIES": "/x/shared.dylib:/x/b.dylib", "TWO": "2"}},
			},
			want: []string{
				"SIMCTL_CHILD_DYLD_INSERT_LIBRARIES=" + shim + ":/x/a.dylib:/x/shared.dylib:/x/b.dylib",
				"SIMCTL_CHILD_LOUPE_CAMERA_FILE=" + file,
				"SIMCTL_CHILD_TWO=2",
			},
		},
		{
			name: "an extension that names the shim does not list it twice",
			shim: shim, camera: file,
			exts: []target.LaunchExtension{{Name: "one", Env: map[string]string{"DYLD_INSERT_LIBRARIES": shim + ":/x/a.dylib"}}},
			want: []string{
				"SIMCTL_CHILD_DYLD_INSERT_LIBRARIES=" + shim + ":/x/a.dylib",
				"SIMCTL_CHILD_LOUPE_CAMERA_FILE=" + file,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := simLaunchEnv(tc.shim, tc.camera, tc.exts)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("simLaunchEnv =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// A key two parties both set would have one of them lose without a word, so
// it is a refusal: two extensions, or an extension and loupe's camera.
func TestSimLaunchEnvRefusesACollision(t *testing.T) {
	for _, tc := range []struct {
		name   string
		camera string
		exts   []target.LaunchExtension
	}{
		{"two extensions", "", []target.LaunchExtension{
			{Name: "one", Env: map[string]string{"SERVER": "a"}},
			{Name: "two", Env: map[string]string{"SERVER": "b"}},
		}},
		{"the camera's file", "/c/sim.png", []target.LaunchExtension{
			{Name: "one", Env: map[string]string{target.ReservedLaunchEnv: "/x.png"}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shim := ""
			if tc.camera != "" {
				shim = "/c/loupecam.dylib"
			}
			_, err := simLaunchEnv(shim, tc.camera, tc.exts)
			if !errors.Is(err, target.ErrBadLaunchSpec) {
				t.Errorf("err = %v, want ErrBadLaunchSpec", err)
			}
		})
	}
}

// stubSimLaunch stands in for simctl and launchctl. list is launchctl's
// answer before the launch.
func stubSimLaunch(t *testing.T, list string) *[][]string {
	t.Helper()
	oldLaunch, oldList := simctlLaunch, simAppList
	t.Cleanup(func() { simctlLaunch, simAppList = oldLaunch, oldList })
	var calls [][]string
	simAppList = func(context.Context, *Simulator) (string, error) { return list, nil }
	simctlLaunch = func(_ context.Context, env []string, args ...string) ([]byte, error) {
		calls = append(calls, append(append([]string{}, args...), env...))
		return []byte(args[len(args)-1] + ": 4242\n"), nil
	}
	return &calls
}

func TestSimLaunchWithAlwaysTerminatesAndSaysWhenItRelaunched(t *testing.T) {
	lib := dylibs(t, 1)[0]
	exts := []target.LaunchExtension{{Name: "one", Env: map[string]string{"DYLD_INSERT_LIBRARIES": lib, "ONE_SERVER": "s"}}}
	for _, tc := range []struct {
		name       string
		list       string
		relaunched bool
	}{
		{"running", "4100\t0\tUIKitApplication:com.example.app[a1b2][rb-legacy]\n", true},
		{"listed, not running", "-\t0\tUIKitApplication:com.example.app[a1b2][rb-legacy]\n", false},
		{"not listed", "4100\t0\tUIKitApplication:com.example.other[a1b2][rb-legacy]\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := stubSimLaunch(t, tc.list)
			s := &Simulator{info: SimInfo{UDID: "LAUNCHWITH-TEST-UDID", OSVersion: "27.0"}}
			res, err := s.LaunchWith(context.Background(), "com.example.app", "", exts)
			if err != nil {
				t.Fatal(err)
			}
			if len(*calls) != 1 {
				t.Fatalf("simctl calls = %q", *calls)
			}
			got := (*calls)[0]
			want := []string{"launch", "--terminate-running-process", "LAUNCHWITH-TEST-UDID", "com.example.app",
				"SIMCTL_CHILD_DYLD_INSERT_LIBRARIES=" + lib, "SIMCTL_CHILD_ONE_SERVER=s"}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("simctl = %q, want %q", got, want)
			}
			if res.Relaunched != tc.relaunched {
				t.Errorf("relaunched = %v, want %v", res.Relaunched, tc.relaunched)
			}
			if wantWith := []target.LaunchedExtension{{Name: "one", PID: 4242}}; !reflect.DeepEqual(res.With, wantWith) {
				t.Errorf("with = %+v, want %+v", res.With, wantWith)
			}
		})
	}
}

// A spec that fails a check is refused here too, before simctl runs.
func TestSimLaunchWithChecksEachSpec(t *testing.T) {
	calls := stubSimLaunch(t, "")
	s := &Simulator{info: SimInfo{UDID: "LAUNCHWITH-TEST-UDID"}}
	for _, exts := range [][]target.LaunchExtension{
		{{Name: "one", Env: map[string]string{"DYLD_INSERT_LIBRARIES": "relative.dylib"}}},
		{{Name: "one"}},
		{{Name: "one", Env: map[string]string{"A": "b"}}, {Name: "two", Env: map[string]string{"A": "c"}}},
	} {
		if _, err := s.LaunchWith(context.Background(), "com.example.app", "", exts); !errors.Is(err, target.ErrBadLaunchSpec) {
			t.Errorf("%+v: err = %v, want ErrBadLaunchSpec", exts, err)
		}
	}
	if _, err := s.LaunchWith(context.Background(), "not a bundle", "", nil); !errors.Is(err, target.ErrBadLaunchName) {
		t.Errorf("bad bundle: err = %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("simctl ran: %q", *calls)
	}
}

func TestSimLaunchFacts(t *testing.T) {
	s := &Simulator{info: SimInfo{UDID: "U", OSVersion: "27.2"}}
	f, err := s.LaunchFacts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := (target.LaunchFacts{Platform: target.LaunchIOSSimulator, UDID: "U", IOSVersion: "27.2"}); f != want {
		t.Errorf("facts = %+v, want %+v", f, want)
	}
}
