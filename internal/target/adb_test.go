package target

import (
	"strings"
	"testing"
)

// The first block is captured byte for byte from `adb track-devices -l` on adb
// 35.0.2 with emulator-5554 up. The second is the empty list, which was not
// captured: it is what the length-prefixed format gives for zero devices, and
// it is the only notice there is that the last device left.
const trackCapture = "0074emulator-5554          device product:sdk_gphone16k_arm64 model:sdk_gphone16k_arm64 device:emu64a16k transport_id:1\n" +
	"0000"

func TestTrackDevicesReportsEveryListIncludingTheEmptyOne(t *testing.T) {
	var got [][]DeviceInfo
	err := readTrackBlocks(strings.NewReader(trackCapture), func(ds []DeviceInfo) {
		got = append(got, ds)
	})
	if err == nil {
		t.Fatal("readTrackBlocks returned nil at end of input; want the read error")
	}
	if len(got) != 2 {
		t.Fatalf("got %d updates, want 2: %+v", len(got), got)
	}
	want := DeviceInfo{Serial: "emulator-5554", State: "device", Model: "sdk_gphone16k_arm64", Emulator: true, TransportID: "1"}
	if len(got[0]) != 1 || got[0][0] != want {
		t.Errorf("first update = %+v, want [%+v]", got[0], want)
	}
	if len(got[1]) != 0 {
		t.Errorf("second update = %+v, want an empty list", got[1])
	}
}

func TestTrackDevicesReadsAnUnauthorizedDevice(t *testing.T) {
	body := "ACME00000000001\tunauthorized usb:1-1 transport_id:3\n"
	in := strings.ToLower(hex4(len(body))) + body
	var got []DeviceInfo
	_ = readTrackBlocks(strings.NewReader(in), func(ds []DeviceInfo) { got = ds })
	if len(got) != 1 || got[0].State != "unauthorized" || got[0].Serial != "ACME00000000001" {
		t.Fatalf("got %+v", got)
	}
}

func TestDeviceTransportFromTheListing(t *testing.T) {
	body := "ACME00000000001\tdevice usb:1-1 model:Acme One transport_id:3\n" +
		"192.168.1.20:5555\tdevice model:Pixel_9 transport_id:4\n" +
		"emulator-5554\tdevice model:sdk_gphone16k_arm64 transport_id:1\n"
	in := strings.ToLower(hex4(len(body))) + body
	var got []DeviceInfo
	_ = readTrackBlocks(strings.NewReader(in), func(ds []DeviceInfo) { got = ds })
	want := map[string]string{"ACME00000000001": "usb", "192.168.1.20:5555": "wifi", "emulator-5554": "emulator"}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for _, d := range got {
		if d.transport() != want[d.Serial] {
			t.Errorf("%s: transport %q, want %q", d.Serial, d.transport(), want[d.Serial])
		}
	}
}

func TestTrackDevicesRejectsAGarbledLength(t *testing.T) {
	err := readTrackBlocks(strings.NewReader("zz12whatever"), func([]DeviceInfo) {
		t.Error("fn called on a garbled stream")
	})
	if err == nil || !strings.Contains(err.Error(), "bad length") {
		t.Fatalf("err = %v, want a bad length error", err)
	}
}

func hex4(n int) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[n>>12&15], digits[n>>8&15], digits[n>>4&15], digits[n&15]})
}

// TestNaturalSizeReadsDisplayZerosInitSize pins the parser opus review item
// 3 asks for: display 0's natural size comes off the same "init=WxH" pair
// displayRotation already isolates, and trap 44's decoy display -- a 1dpi
// surface that prints first with its own init=1080x480 -- must not answer
// for it.
func TestNaturalSizeReadsDisplayZerosInitSize(t *testing.T) {
	dump := fakeDumpsysWindowDisplays("1080x2424", "2424x1080", "ROTATION_90")
	w, h, ok := naturalSize(dump)
	if !ok {
		t.Fatal("naturalSize: want ok, got false")
	}
	if w != 1080 || h != 2424 {
		t.Fatalf("naturalSize() = %d, %d; want the natural (unrotated) 1080, 2424 -- not the decoy's 1080x480 and not the rotated cur= 2424x1080", w, h)
	}
}

// TestNaturalSizeReportsNoDisplayZero covers a dump with no display 0 at
// all, mirroring displayRotation's own "no answer" case.
func TestNaturalSizeReportsNoDisplayZero(t *testing.T) {
	if _, _, ok := naturalSize("Display: mDisplayId=1084\n    init=1080x480 1dpi"); ok {
		t.Error("naturalSize answered for a dump with no display 0")
	}
}

// TestNaturalSizeReportsNoInitField covers display 0 being present but
// carrying no init= field, which a malformed or truncated dump could do.
func TestNaturalSizeReportsNoInitField(t *testing.T) {
	if _, _, ok := naturalSize("Display: mDisplayId=0\n    cur=1080x2424"); ok {
		t.Error("naturalSize answered for a display 0 block with no init=")
	}
}
