package target

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseAVDDevicesGroupsAndDrops(t *testing.T) {
	f, err := os.Open("../../testdata/devices/devices.xml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	devs, err := ParseAVDDevices(f)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]AVDDevice{}
	for _, d := range devs {
		got[d.ID] = d
	}
	want := map[string]struct {
		group, size string
		minAPI      int
	}{
		"pixel_9":          {GroupPhone, `6.2" · 1080×2424`, 0},
		"pixel_9_pro_fold": {GroupFoldable, `8.0" · 2076×2152`, 35},
		"pixel_fold":       {GroupFoldable, `7.6" · 2208×1840`, 34},
		"pixel_tablet":     {GroupTablet, `10.9" · 2560×1600`, 0},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d devices %v, want %d (TV and the legacy id dropped)", len(got), got, len(want))
	}
	for id, w := range want {
		d := got[id]
		if d.Group != w.group || d.Size() != w.size || d.MinAPI != w.minAPI {
			t.Errorf("%s = %s %q min %d, want %s %q min %d", id, d.Group, d.Size(), d.MinAPI, w.group, w.size, w.minAPI)
		}
	}
}

func writeImage(t *testing.T, sdk, api, tag, abi, props string) {
	t.Helper()
	dir := filepath.Join(sdk, "system-images", api, tag, abi)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source.properties"), []byte(props), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInstalledSystemImages(t *testing.T) {
	sdk := t.TempDir()
	writeImage(t, sdk, "android-34", "google_apis", "arm64-v8a", "AndroidVersion.ApiLevel=34\nSystemImage.TagId=google_apis\nSystemImage.TagDisplay=Google APIs\nSystemImage.Abi=arm64-v8a\n")
	writeImage(t, sdk, "android-36.1", "google_apis_playstore", "arm64-v8a", "AndroidVersion.ApiLevel=36.1\nSystemImage.TagId=google_apis_playstore\nSystemImage.TagDisplay=Google Play\nSystemImage.Abi=arm64-v8a\n")
	writeImage(t, sdk, "android-37.0", "google_apis_playstore_ps16k", "arm64-v8a", "AndroidVersion.ApiLevel=37.0\nSystemImage.TagId=google_apis_playstore,page_size_16kb,ai_glasses_compatible\nSystemImage.TagDisplay=Google Play,16 KB Page Size\nSystemImage.Abi=arm64-v8a\n")
	writeImage(t, sdk, "android-34", "android-tv", "arm64-v8a", "AndroidVersion.ApiLevel=34\nSystemImage.TagId=android-tv\nSystemImage.Abi=arm64-v8a\n")
	writeImage(t, sdk, "android-34", "google_apis", "x86_64", "AndroidVersion.ApiLevel=34\nSystemImage.TagId=google_apis\nSystemImage.Abi=x86_64\n")

	imgs := installedSystemImages(sdk, "arm64-v8a")
	type row struct{ id, label, detail string }
	var got []row
	for _, i := range imgs {
		got = append(got, row{i.ID, i.Label(), i.Detail()})
	}
	want := []row{
		{"system-images;android-37.0;google_apis_playstore_ps16k;arm64-v8a", "Android 17", "API 37 · Google Play · 16 KB pages"},
		{"system-images;android-36.1;google_apis_playstore;arm64-v8a", "Android 16", "API 36.1 · Google Play"},
		{"system-images;android-34;google_apis;arm64-v8a", "Android 14", "API 34 · Google APIs"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("image %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestPatchAVDConfig(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.ini"), []byte("hw.keyboard = no\nhw.lcd.width=1080\n"), 0o644)
	if err := PatchAVDConfig(dir, [][2]string{{"hw.keyboard", "yes"}, {"avd.ini.displayname", "X"}}); err != nil {
		t.Fatal(err)
	}
	kv, _ := readINI(filepath.Join(dir, "config.ini"))
	if kv["hw.keyboard"] != "yes" || kv["avd.ini.displayname"] != "X" || kv["hw.lcd.width"] != "1080" {
		t.Errorf("config.ini = %v", kv)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "config.ini"))
	if string(raw) != "hw.keyboard=yes\nhw.lcd.width=1080\navd.ini.displayname=X\n" {
		t.Errorf("order or content changed:\n%s", raw)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Errorf("temp file left behind: %v", ents)
	}
}

// An AVD as avdmanager writes it for an API 36.1 image has to scan with the
// right level and form.
func TestScanAVDsReadsAvdmanagerOutput(t *testing.T) {
	home := t.TempDir()
	mk := func(name, device, sysdir, hinge string) {
		dir := filepath.Join(home, name+".avd")
		os.MkdirAll(dir, 0o755)
		cfg := "hw.device.name=" + device + "\nhw.lcd.width=1080\nhw.lcd.height=2424\ntag.id=google_apis_playstore\nimage.sysdir.1=" + sysdir + "\n" + hinge
		os.WriteFile(filepath.Join(dir, "config.ini"), []byte(cfg), 0o644)
		os.WriteFile(filepath.Join(home, name+".ini"), []byte("path="+dir+"\ntarget=android-36.1\n"), 0o644)
	}
	sd := "system-images/android-36.1/google_apis_playstore/arm64-v8a/"
	mk("P9", "pixel_9", sd, "")
	mk("Tab", "pixel_tablet", sd, "")
	mk("Fold", "pixel_9_pro_fold", sd, "hw.sensor.hinge=yes\n")
	avds, err := ScanAVDs(home)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"P9": "phone", "Tab": "tablet", "Fold": "foldable"}
	if len(avds) != 3 {
		t.Fatalf("got %d AVDs", len(avds))
	}
	for _, a := range avds {
		if a.APILevel != 36 || a.Description().Form != want[a.Name] {
			t.Errorf("%s = API %d form %q, want 36 %q", a.Name, a.APILevel, a.Description().Form, want[a.Name])
		}
	}
}

func TestParseSimRuntimesAndTypeInfo(t *testing.T) {
	raw, _ := os.ReadFile("../../testdata/devices/sim-runtimes.json")
	rts, err := ParseSimRuntimes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(rts) != 2 || rts[0].ID != "com.apple.CoreSimulator.SimRuntime.iOS-27-0" || len(rts[0].Types) != 2 {
		t.Fatalf("runtimes = %+v (unavailable, watchOS and Watch types dropped)", rts)
	}
	if len(rts[1].Types) != 1 || rts[1].Types[0].Name != "iPhone Duo" {
		t.Errorf("27.1 types = %+v, want only the Duo", rts[1].Types)
	}
	for file, want := range map[string]SimTypeInfo{
		"sim-caps-iphone17.json": {Group: GroupPhone, Size: `6.3" · 1206×2622`},
		"sim-caps-duo.json":      {Group: GroupFoldable, Size: `7.6" · 2853×2007`},
	} {
		caps, _ := os.ReadFile("../../testdata/devices/" + file)
		got, err := ParseSimTypeInfo(caps, "iPhone")
		if err != nil || got != want {
			t.Errorf("%s = %+v, %v; want %+v", file, got, err, want)
		}
	}
	if got, _ := ParseSimTypeInfo([]byte(`{"capabilities":{}}`), "iPad"); got.Group != GroupTablet {
		t.Errorf("iPad group = %q", got.Group)
	}
}
