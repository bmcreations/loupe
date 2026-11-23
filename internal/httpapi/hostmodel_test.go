package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// profilerOutput is the shape `system_profiler -json -detailLevel mini
// SPHardwareDataType` prints, with made-up values.
const profilerOutput = `{
  "SPHardwareDataType" : [
    {
      "_name" : "hardware_overview",
      "chip_type" : "Apple M4",
      "machine_model" : "Mac16,10",
      "machine_name" : "Mac mini",
      "physical_memory" : "16 GB"
    }
  ]
}`

func TestParseMachineName(t *testing.T) {
	for _, tc := range []struct {
		name, out, want string
	}{
		{"mac mini", profilerOutput, "Mac mini"},
		{"macbook pro", `{"SPHardwareDataType":[{"machine_model":"Mac15,7","machine_name":"MacBook Pro"}]}`, "MacBook Pro"},
		{"padded", `{"SPHardwareDataType":[{"machine_name":"  Mac Studio\n"}]}`, "Mac Studio"},
		// The identifier alone is no model name; the field stays empty.
		{"no machine_name", `{"SPHardwareDataType":[{"machine_model":"Mac16,10"}]}`, ""},
		{"empty array", `{"SPHardwareDataType":[]}`, ""},
		{"other data type", `{"SPSoftwareDataType":[{"machine_name":"Mac mini"}]}`, ""},
		{"malformed", `Hardware:\n  Model Name: Mac mini`, ""},
		{"empty", ``, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseMachineName([]byte(tc.out)); got != tc.want {
				t.Errorf("parseMachineName = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHostModelFor(t *testing.T) {
	ran := false
	profile := func() ([]byte, error) { ran = true; return []byte(profilerOutput), nil }
	failing := func() ([]byte, error) { ran = true; return nil, errors.New("exit status 1") }

	for _, tc := range []struct {
		goos    string
		profile func() ([]byte, error)
		want    string
		runs    bool
	}{
		{"darwin", profile, "Mac mini", true},
		{"darwin", failing, "", true},
		// Only macOS has system_profiler; nothing else runs it.
		{"linux", profile, "Linux", false},
		{"windows", profile, "", false},
		{"freebsd", profile, "", false},
	} {
		ran = false
		if got := hostModelFor(tc.goos, tc.profile); got != tc.want {
			t.Errorf("hostModelFor(%q) = %q, want %q", tc.goos, got, tc.want)
		}
		if ran != tc.runs {
			t.Errorf("hostModelFor(%q) ran system_profiler = %v, want %v", tc.goos, ran, tc.runs)
		}
	}
}

func getConfigBody(t *testing.T, s *Server) string {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleConfig(w, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/config = %d %s", w.Code, w.Body)
	}
	return w.Body.String()
}

func TestConfigGetReportsHostModel(t *testing.T) {
	st, _ := newTestStore(t)
	calls := 0
	s := &Server{cfg: st}
	s.SetHostModel(func() string { calls++; return "MacBook Pro" })

	if body := getConfigBody(t, s); !strings.Contains(body, `"hostModel":"MacBook Pro"`) {
		t.Errorf("body %s has no hostModel", body)
	}
	if calls != 1 {
		t.Errorf("hostModel called %d times for one GET, want 1", calls)
	}
}

// The field is omitempty: an empty answer and no detector at all both leave
// the key out of the body, not "hostModel":"".
func TestConfigGetOmitsAnUnknownHostModel(t *testing.T) {
	st, _ := newTestStore(t)
	for name, model := range map[string]func() string{
		"unset": nil,
		"empty": func() string { return "" },
	} {
		s := &Server{cfg: st, hostModel: model}
		if body := getConfigBody(t, s); strings.Contains(body, "hostModel") {
			t.Errorf("%s: body %s carries hostModel", name, body)
		}
	}
}

// Handset reads hostModel with the session it paired, not the master token.
func TestAPairedSessionReadsHostModel(t *testing.T) {
	s, _ := pairingServer(t)
	st, _ := newTestStore(t)
	s.SetConfig(st)
	s.SetHostModel(func() string { return "Mac mini" })
	h := s.Handler()

	sess := pairBearer(t, h, mintCode(t, h, master, `{"label":"handset"}`).Code)
	rec := pdo(h, call{method: http.MethodGet, path: "/api/config", auth: "Bearer " + sess.Token})
	if rec.Code != http.StatusOK {
		t.Fatalf("a session's GET /api/config = %d %s", rec.Code, rec.Body)
	}
	if got := decode[ConfigResponse](t, rec).HostModel; got != "Mac mini" {
		t.Errorf("hostModel = %q, want Mac mini", got)
	}
}
