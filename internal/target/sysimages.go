package target

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// SystemImage is one installed `system-images;android-<n>;<tag>;<abi>`.
type SystemImage struct {
	// ID is the package path avdmanager's -k takes, built from the directory
	// names: the 37.0 image lives under google_apis_playstore_ps16k although
	// its TagId says google_apis_playstore.
	ID string
	// API is AndroidVersion.ApiLevel as written: "29", "36.1", "37.0".
	API string
	// Family is the first of SystemImage.TagId, the kind of image
	// (google_apis_playstore, google_apis, default).
	Family string
	// Tags is every entry of SystemImage.TagId.
	Tags []string
	// TagDisplay is SystemImage.TagDisplay's first entry.
	TagDisplay string
	Abi        string
}

// APIFloor is the integer part of API: 36 for "36.1".
func (s SystemImage) APIFloor() int {
	n, _, _ := strings.Cut(s.API, ".")
	v, _ := strconv.Atoi(n)
	return v
}

// APILabel is API without a trailing ".0": "37.0" reads "37".
func (s SystemImage) APILabel() string { return strings.TrimSuffix(s.API, ".0") }

// phoneFamily reports whether the image is one a phone, foldable or tablet
// runs. android-tv, android-wear, android-automotive* and android-desktop
// images are not.
func (s SystemImage) phoneFamily() bool {
	return s.Family != "" && !strings.HasPrefix(s.Family, "android-")
}

// androidVersionNames maps an API level to the release it shipped as. 36.1 is
// the Android 16 QPR2 platform release and 37.0 is Android 17. An API not
// listed is labelled "API <n>" rather than guessed.
var androidVersionNames = map[string]string{
	"29": "10", "30": "11", "31": "12", "32": "12L", "33": "13",
	"34": "14", "35": "15", "36": "16", "36.1": "16", "37": "17",
}

// Label is "Android 16", or "API 41" when the release is not in the table.
func (s SystemImage) Label() string {
	if v, ok := androidVersionNames[s.APILabel()]; ok {
		return "Android " + v
	}
	return "API " + s.APILabel()
}

// Detail is `API 36.1 · Google Play`, with ` · 16 KB pages` when the image is
// the page-size variant.
func (s SystemImage) Detail() string {
	d := "API " + s.APILabel()
	if tag := s.tagName(); tag != "" {
		d += " · " + tag
	}
	if slices.Contains(s.Tags, "page_size_16kb") {
		d += " · 16 KB pages"
	}
	return d
}

func (s SystemImage) tagName() string {
	switch s.Family {
	case "google_apis_playstore":
		return "Google Play"
	case "google_apis":
		return "Google APIs"
	case "default":
		return "AOSP"
	}
	return s.TagDisplay
}

// ParseSystemImage reads a source.properties. dirAPI, dirTag and dirAbi are
// the directory names it was found under.
func ParseSystemImage(props map[string]string, dirAPI, dirTag, dirAbi string) (SystemImage, bool) {
	api := props["AndroidVersion.ApiLevel"]
	if api == "" || props["AndroidVersion.CodeName"] != "" {
		return SystemImage{}, false
	}
	if _, err := strconv.ParseFloat(api, 64); err != nil {
		return SystemImage{}, false
	}
	tags := strings.Split(props["SystemImage.TagId"], ",")
	for i := range tags {
		tags[i] = strings.TrimSpace(tags[i])
	}
	if tags[0] == "" {
		tags = []string{dirTag}
	}
	abi := props["SystemImage.Abi"]
	if abi == "" {
		abi = dirAbi
	}
	display, _, _ := strings.Cut(props["SystemImage.TagDisplay"], ",")
	return SystemImage{
		ID:         fmt.Sprintf("system-images;%s;%s;%s", dirAPI, dirTag, dirAbi),
		API:        api,
		Family:     tags[0],
		Tags:       tags,
		TagDisplay: strings.TrimSpace(display),
		Abi:        abi,
	}, true
}

// hostAbi is the ABI an emulator on this machine can run.
func hostAbi() string {
	if runtime.GOARCH == "arm64" {
		return "arm64-v8a"
	}
	return "x86_64"
}

// InstalledSystemImages lists the phone-family images under the SDK for this
// host's ABI, newest API first. Nothing is downloaded.
func InstalledSystemImages(sdk string) []SystemImage {
	return installedSystemImages(sdk, hostAbi())
}

func installedSystemImages(sdk, abi string) []SystemImage {
	if sdk == "" {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(sdk, "system-images", "*", "*", abi, "source.properties"))
	var out []SystemImage
	for _, f := range files {
		abiDir := filepath.Dir(f)
		tagDir := filepath.Dir(abiDir)
		apiDir := filepath.Dir(tagDir)
		props, err := readINI(f)
		if err != nil {
			continue
		}
		img, ok := ParseSystemImage(props, filepath.Base(apiDir), filepath.Base(tagDir), filepath.Base(abiDir))
		if !ok || !img.phoneFamily() {
			continue
		}
		out = append(out, img)
	}
	slices.SortFunc(out, func(a, b SystemImage) int {
		x, _ := strconv.ParseFloat(a.API, 64)
		y, _ := strconv.ParseFloat(b.API, 64)
		switch {
		case x != y:
			if x > y {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// SkinExists reports whether the SDK ships a skin for a device id, which
// avdmanager's --skin takes and loupe's frame route reads.
func SkinExists(sdk, device string) bool {
	if sdk == "" || device == "" {
		return false
	}
	fi, err := os.Stat(filepath.Join(sdk, "skins", device))
	return err == nil && fi.IsDir()
}

// PatchAVDConfig sets keys in an AVD's config.ini, replacing a line that
// already has the key (either spacing) and appending the rest. avdmanager
// leaves out what Android Studio writes: the keyboard, the Play Store flag,
// the GPU and the display name.
func PatchAVDConfig(dir string, set [][2]string) error {
	path := filepath.Join(dir, "config.ini")
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	for _, kv := range set {
		found := false
		for i, l := range lines {
			if k, _, ok := strings.Cut(l, "="); ok && strings.TrimSpace(k) == kv[0] {
				lines[i], found = kv[0]+"="+kv[1], true
			}
		}
		if !found {
			lines = append(lines, kv[0]+"="+kv[1])
		}
	}
	return writeFileAtomic(path, []byte(strings.Join(lines, "\n")+"\n"))
}

// writeFileAtomic writes a temp file beside path, syncs it and renames it over
// path, so a reader (or a crash) sees the old file or the new one, never half.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op after a successful rename
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
