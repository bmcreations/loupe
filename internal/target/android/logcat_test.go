package android

import (
	"context"
	"errors"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

func TestADBLogcatRefusesAFormat(t *testing.T) {
	tg := &ADBTarget{info: DeviceInfo{Serial: "s", State: "device"}}
	_, err := tg.Logcat(context.Background(), target.LogcatQuery{Format: target.LogFormatNDJSON})
	if !errors.Is(err, target.ErrLogQueryUnsupported) {
		t.Errorf("err = %v; want ErrLogQueryUnsupported", err)
	}
}
