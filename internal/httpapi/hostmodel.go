package httpapi

import (
	"context"
	"encoding/json"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// DetectHostModel names the kind of machine this server runs on, for GET
// /api/config's hostModel: the model name macOS reports ("Mac mini",
// "MacBook Pro"), "Linux" on Linux, and "" anywhere else or when macOS will
// not say. Handset picks a server's icon from it.
//
// It runs system_profiler, about 0.1s, so call it once and keep the answer.
func DetectHostModel(ctx context.Context) string {
	return hostModelFor(runtime.GOOS, func() ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		// -detailLevel mini leaves out the serial number and hardware UUID,
		// which this has no use for.
		return exec.CommandContext(ctx, "/usr/sbin/system_profiler",
			"-json", "-detailLevel", "mini", "SPHardwareDataType").Output()
	})
}

// hostModelFor is DetectHostModel with the OS and the system_profiler run
// passed in, so the mapping tests on any machine.
func hostModelFor(goos string, profile func() ([]byte, error)) string {
	switch goos {
	case "darwin":
		out, err := profile()
		if err != nil {
			return ""
		}
		return parseMachineName(out)
	case "linux":
		return "Linux"
	}
	return ""
}

// parseMachineName reads the model name from `system_profiler -json
// SPHardwareDataType`. It reads machine_name, the family name, and not
// machine_model, which is an identifier like "Mac16,10". The JSON keys stay
// the same in every language, where the text output's "Model Name:" label
// does not.
func parseMachineName(out []byte) string {
	var doc struct {
		Hardware []struct {
			MachineName string `json:"machine_name"`
		} `json:"SPHardwareDataType"`
	}
	if err := json.Unmarshal(out, &doc); err != nil || len(doc.Hardware) == 0 {
		return ""
	}
	return strings.TrimSpace(doc.Hardware[0].MachineName)
}

// SetHostModel gives GET /api/config its hostModel. model is called on every
// GET, so it must return a cached answer; main passes a sync.OnceValue over
// DetectHostModel. Nil leaves hostModel out.
func (s *Server) SetHostModel(model func() string) { s.hostModel = model }
