package android

import (
	"context"
	"errors"
	"testing"

	"github.com/bmcreations/loupe/internal/target/ios"

	"github.com/bmcreations/loupe/internal/target"
)

func TestADBLogcatRefusesAFormat(t *testing.T) {
	tg := &Device{info: DeviceInfo{Serial: "s", State: "device"}}
	_, err := tg.Logcat(context.Background(), target.LogcatQuery{Format: ios.LogFormatNDJSON})
	if !errors.Is(err, ios.ErrLogQueryUnsupported) {
		t.Errorf("err = %v; want ErrLogQueryUnsupported", err)
	}
}
