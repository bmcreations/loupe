package target

import (
	"context"
	"os"
	"testing"
)

func TestSyntheticDescribeIsAFixedPattern(t *testing.T) {
	s := &Synthetic{}
	d, err := s.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if d.Form != "pattern" {
		t.Errorf("Form = %q, want pattern", d.Form)
	}
	if d.OS != "synthetic" {
		t.Errorf("OS = %q, want synthetic", d.OS)
	}
	if d.OSVersion != "" || d.APILevel != 0 {
		t.Errorf("Description = %+v, want no version or api level", d)
	}
}

func TestParseGetpropReadsKeyValueLines(t *testing.T) {
	raw := "[ro.build.version.release]: [17]\n[ro.build.version.sdk]: [37]\n"
	props := parseGetprop([]byte(raw))
	if props["ro.build.version.release"] != "17" {
		t.Fatalf("release = %q, want 17", props["ro.build.version.release"])
	}
	if props["ro.build.version.sdk"] != "37" {
		t.Fatalf("sdk = %q, want 37", props["ro.build.version.sdk"])
	}
}

func TestParseGetpropHandlesValuesContainingABracket(t *testing.T) {
	raw := "[ro.build.fingerprint]: [google/sdk/emu:17/CP1[test]/1:user/dev-keys]\n"
	props := parseGetprop([]byte(raw))
	want := "google/sdk/emu:17/CP1[test]/1:user/dev-keys"
	if props["ro.build.fingerprint"] != want {
		t.Fatalf("fingerprint = %q, want %q", props["ro.build.fingerprint"], want)
	}
}

func TestParseGetpropHandlesAnEmptyValue(t *testing.T) {
	raw := "[dalvik.vm.dex2oat-Xms]: []\n"
	props := parseGetprop([]byte(raw))
	v, ok := props["dalvik.vm.dex2oat-Xms"]
	if !ok || v != "" {
		t.Fatalf("dex2oat-Xms = %q, %v; want empty string, present", v, ok)
	}
}

func TestParseGetpropSkipsCommentLines(t *testing.T) {
	raw := "# this is a comment, not a property\n\n[ro.build.version.sdk]: [37]\n"
	props := parseGetprop([]byte(raw))
	if len(props) != 1 || props["ro.build.version.sdk"] != "37" {
		t.Fatalf("props = %+v, want just sdk=37", props)
	}
}

func TestDescribeFromTheEmulatorFixtureIsAPhone(t *testing.T) {
	raw, err := os.ReadFile("testdata/getprop-emulator.txt")
	if err != nil {
		t.Fatal(err)
	}
	d, ok := describeFrom(parseGetprop(raw))
	if !ok {
		t.Fatal("describeFrom on the emulator fixture: want ok, got not-ok")
	}
	if d.Form != "phone" {
		t.Errorf("Form = %q, want phone", d.Form)
	}
	if d.OS != "android" {
		t.Errorf("OS = %q, want android", d.OS)
	}
	if d.OSVersion != "17" {
		t.Errorf("OSVersion = %q, want 17", d.OSVersion)
	}
	if d.APILevel != 37 {
		t.Errorf("APILevel = %d, want 37", d.APILevel)
	}
}

func TestDescribeFromTheTabletFixtureIsATablet(t *testing.T) {
	raw, err := os.ReadFile("testdata/getprop-tablet.txt")
	if err != nil {
		t.Fatal(err)
	}
	d, ok := describeFrom(parseGetprop(raw))
	if !ok {
		t.Fatal("describeFrom on the tablet fixture: want ok, got not-ok")
	}
	if d.Form != "tablet" {
		t.Errorf("Form = %q, want tablet", d.Form)
	}
}

func TestCharacteristicsMatchesTheElementNotASubstring(t *testing.T) {
	// "nosdcard,notablet" contains the substring "tablet" but is not the
	// tablet form; matching must be element-wise.
	props := map[string]string{
		"ro.build.characteristics": "nosdcard,notablet",
		"ro.build.version.release": "14",
		"ro.build.version.sdk":     "34",
	}
	d, ok := describeFrom(props)
	if !ok {
		t.Fatal("describeFrom: want ok, got not-ok")
	}
	if d.Form != "phone" {
		t.Fatalf("Form = %q, want phone", d.Form)
	}
}

func TestDescribeFromAnUnparsableAPILevelOmitsIt(t *testing.T) {
	props := map[string]string{
		"ro.build.version.release": "14",
		"ro.build.version.sdk":     "not-a-number",
	}
	d, ok := describeFrom(props)
	if !ok {
		t.Fatal("describeFrom: want ok, got not-ok (sdk key is present, just unparsable)")
	}
	if d.APILevel != 0 {
		t.Fatalf("APILevel = %d, want 0 (omitted)", d.APILevel)
	}
}

func TestDescribeFromMissingReleaseIsNotOk(t *testing.T) {
	props := map[string]string{
		"ro.build.version.sdk": "34",
	}
	if _, ok := describeFrom(props); ok {
		t.Fatal("describeFrom with no release key: want not-ok, got ok")
	}
}

func TestDescribeFromMissingSDKIsNotOk(t *testing.T) {
	props := map[string]string{
		"ro.build.version.release": "14",
	}
	if _, ok := describeFrom(props); ok {
		t.Fatal("describeFrom with no sdk key: want not-ok, got ok")
	}
}

func TestDescribeFromEmptyOutputIsNotOk(t *testing.T) {
	// A shell that exits 0 with nothing on stdout -- a plausible partial
	// failure -- must not be reported as a Description with every field
	// blank; it must be treated exactly like a dump missing both keys.
	if _, ok := describeFrom(parseGetprop(nil)); ok {
		t.Fatal("describeFrom on empty getprop output: want not-ok, got ok")
	}
}

func TestFormFromTVIsOmitted(t *testing.T) {
	if got := formFrom("tv"); got != "" {
		t.Fatalf("formFrom(tv) = %q, want empty (omitted)", got)
	}
}

func TestFormFromWatchIsOmitted(t *testing.T) {
	if got := formFrom("watch"); got != "" {
		t.Fatalf("formFrom(watch) = %q, want empty (omitted)", got)
	}
}

func TestFormFromAutomotiveIsOmitted(t *testing.T) {
	if got := formFrom("nosdcard,automotive"); got != "" {
		t.Fatalf("formFrom(nosdcard,automotive) = %q, want empty (omitted)", got)
	}
}

func TestFormFromPhoneLikeValues(t *testing.T) {
	for _, characteristics := range []string{"default", "nosdcard", "emulator", ""} {
		if got := formFrom(characteristics); got != "phone" {
			t.Errorf("formFrom(%q) = %q, want phone", characteristics, got)
		}
	}
}
