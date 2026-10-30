package target

import (
	"reflect"
	"testing"
)

func TestParseIdentities(t *testing.T) {
	// `security find-identity -v -p codesigning`, shortened.
	out := `  1) F91AF5852254D4F431DE0ED4147F3C2313E76855 "Apple Development: Jane Doe (ABCDE12345)"
  2) 1111111111111111111111111111111111111111 "Developer ID Application: Jane Doe (2X56L589EV)"
  3) A8FE56FC3881C00F8A78371E77E27E946A1B4CC4 "Apple Development: jane@example.com (FGHIJ67890)"
     3 valid identities found
`
	got := parseIdentities(out)
	want := []string{"F91AF5852254D4F431DE0ED4147F3C2313E76855", "A8FE56FC3881C00F8A78371E77E27E946A1B4CC4"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestHomeRE(t *testing.T) {
	// dyld's DYLD_PRINT_ENV output on the console, on an iPhone 16 Pro.
	l := "dyld[6204]: HOME=/private/var/mobile/Containers/Data/Application/241849D5-52D7-44AB-A9CF-F469C0A4FB65"
	m := homeRE.FindStringSubmatch(l)
	if m == nil || m[1] != "/private/var/mobile/Containers/Data/Application/241849D5-52D7-44AB-A9CF-F469C0A4FB65" {
		t.Fatalf("got %v", m)
	}
	if homeRE.MatchString("dyld[6204]: CFFIXED_USER_HOME=/private/var/x") {
		t.Fatal("matched CFFIXED_USER_HOME")
	}
}

func TestLaunchPid(t *testing.T) {
	// `devicectl device process launch -j`, devicectl 651.
	raw := []byte(`{"result":{"deviceIdentifier":"B28880B7","process":{"executable":"file:///private/var/containers/Bundle/Application/X/CamTest.app/CamTest","processIdentifier":6328}}}`)
	if got := launchPid(raw); got != 6328 {
		t.Fatalf("got %d", got)
	}
	if got := launchPid([]byte("not json")); got != 0 {
		t.Fatalf("got %d for junk", got)
	}
}

func TestParseRunningPids(t *testing.T) {
	raw := []byte(`{"result":{"deviceIdentifier":"B2","runningProcesses":[{"executable":"file:///sbin/launchd","processIdentifier":1},{"processIdentifier":6297}]}}`)
	got := parseRunningPids(raw)
	if !got[1] || !got[6297] || len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}
