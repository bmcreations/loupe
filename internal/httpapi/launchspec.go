package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bmcreations/loupe/internal/target"
	"github.com/bmcreations/loupe/internal/target/android"
)

// A launch with extensions: {"action":"launch","package":...,"with":[names]}
// asks each named extension, which registered with "launch": true, what the
// launch should carry, by POSTing <its url>/launch-spec, and hands every
// answer that passes its checks to the target's LaunchWith. Nothing is
// launched until every extension has answered and every answer has passed.
// See docs/extensions.md, "Launch-time injection".

const (
	// launchSpecTimeout bounds one extension's answer. It may compile what
	// it hands back on its first call.
	launchSpecTimeout = 60 * time.Second
	// maxLaunchSpecBody bounds an extension's answer.
	maxLaunchSpecBody = 64 << 10
	// maxLaunchSpecError bounds how much of a refusal's body the error
	// repeats.
	maxLaunchSpecError = 512
	// launchWithTimeout is the device's budget for a launch with
	// extensions: the launch, the wait for the process, the pushes and
	// the attach. The extensions' answers have their own.
	launchWithTimeout = launchTimeout + time.Minute
)

// launchSpecClient calls extensions. It follows no redirect: the extension
// answers at the URL it registered, and a redirect is a failed answer.
var launchSpecClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// launchSpecRequest is what loupe POSTs to <extension url>/launch-spec.
type launchSpecRequest struct {
	Target     string `json:"target"`
	Platform   string `json:"platform"`
	Package    string `json:"package"`
	UDID       string `json:"udid,omitempty"`
	IOSVersion string `json:"iosVersion,omitempty"`
	ABI        string `json:"abi,omitempty"`
	SDK        int    `json:"sdk,omitempty"`
}

// launchSpecResponse is an extension's answer: env for an iOS simulator,
// agent and reverse for Android.
type launchSpecResponse struct {
	Env   map[string]string `json:"env"`
	Agent *struct {
		SO      string            `json:"so"`
		Dex     string            `json:"dex"`
		Options map[string]string `json:"options"`
	} `json:"agent"`
	Reverse string `json:"reverse"`
}

// launchWithError is a launch with extensions refused with status.
type launchWithError struct {
	status int
	err    error
}

func (e *launchWithError) Error() string { return e.err.Error() }
func (e *launchWithError) Unwrap() error { return e.err }

func launchWithErrorf(status int, format string, a ...any) *launchWithError {
	return &launchWithError{status: status, err: fmt.Errorf(format, a...)}
}

// launchExtensions checks a launch's with list before arbitration: that t
// can launch with extensions and that each name is registered for launch,
// once. It returns the registrations it found, which the launch then
// uses, so one that is renewed or dropped meanwhile does not change it.
func (s *Server) launchExtensions(t target.Target, names []string) ([]*registeredExtension, *launchWithError) {
	if t.Kind() == target.KindIOSDevice {
		return nil, launchWithErrorf(http.StatusBadRequest, "launch injection is simulator-only")
	}
	if _, ok := t.(target.LaunchInjector); !ok {
		return nil, launchWithErrorf(http.StatusNotImplemented, "target %q cannot launch with extensions", t.ID())
	}
	seen := map[string]bool{}
	exts := make([]*registeredExtension, 0, len(names))
	for _, name := range names {
		if seen[name] {
			return nil, launchWithErrorf(http.StatusBadRequest, "extension %q is named twice in with", name)
		}
		seen[name] = true
		e := s.exts.get(name)
		if e == nil {
			return nil, launchWithErrorf(http.StatusBadRequest, "extension %q is not registered", name)
		}
		if !e.Launch {
			return nil, launchWithErrorf(http.StatusBadRequest, `extension %q did not register with "launch": true`, name)
		}
		exts = append(exts, e)
	}
	return exts, nil
}

// launchSpecs asks each extension in turn what a launch of pkg on t should
// carry and checks every answer. Any failure is an error, never a launch
// without that extension.
func launchSpecs(ctx context.Context, t target.LaunchInjector, id, pkg string, exts []*registeredExtension) ([]target.LaunchExtension, *launchWithError) {
	fctx, cancel := context.WithTimeout(ctx, agentTimeout)
	facts, err := t.LaunchFacts(fctx)
	cancel()
	if err != nil {
		return nil, &launchWithError{status: http.StatusBadGateway, err: err}
	}
	body, err := json.Marshal(launchSpecRequest{
		Target: id, Platform: facts.Platform, Package: pkg,
		UDID: facts.UDID, IOSVersion: facts.IOSVersion, ABI: facts.ABI, SDK: facts.SDK,
	})
	if err != nil {
		return nil, &launchWithError{status: http.StatusInternalServerError, err: err}
	}
	specs := make([]target.LaunchExtension, 0, len(exts))
	for _, e := range exts {
		spec, err := fetchLaunchSpec(ctx, e, body)
		if err != nil {
			return nil, launchWithErrorf(http.StatusBadGateway, "extension %s: %v", e.Name, err)
		}
		le := target.LaunchExtension{Name: e.Name, Env: spec.Env, Reverse: spec.Reverse}
		if spec.Agent != nil {
			le.Agent = &target.LaunchAgent{SO: spec.Agent.SO, Dex: spec.Agent.Dex, Options: spec.Agent.Options}
		}
		if spec.Reverse != "" {
			// The extension's own registered port, never one it answered
			// with: a reverse to any other port would let it point the app
			// at something else on this Mac.
			le.ReversePort = extensionPort(e)
		}
		if err := le.Validate(facts.Platform); err != nil {
			return nil, launchWithErrorf(http.StatusBadGateway, "extension %s: %v", e.Name, err)
		}
		specs = append(specs, le)
	}
	return specs, nil
}

// fetchLaunchSpec POSTs body to e's /launch-spec, with none of loupe's
// credentials, and decodes a 2xx answer. Anything else is an error naming
// what the extension said.
func fetchLaunchSpec(ctx context.Context, e *registeredExtension, body []byte) (*launchSpecResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, launchSpecTimeout)
	defer cancel()
	u := *e.target
	u.Path = strings.TrimSuffix(u.Path, "/") + "/launch-spec"
	u.RawPath = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := launchSpecClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxLaunchSpecBody+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		msg := oneLine(b, maxLaunchSpecError)
		if msg == "" {
			msg = resp.Status
		}
		return nil, errors.New(msg)
	}
	if len(b) > maxLaunchSpecBody {
		return nil, fmt.Errorf("launch spec is over %d KiB", maxLaunchSpecBody>>10)
	}
	var spec launchSpecResponse
	if err := json.Unmarshal(b, &spec); err != nil {
		return nil, fmt.Errorf("launch spec: %v", err)
	}
	return &spec, nil
}

// extensionPort is the port of e's registered URL, its scheme's default
// when the URL names none.
func extensionPort(e *registeredExtension) int {
	if p, err := strconv.Atoi(e.target.Port()); err == nil {
		return p
	}
	if e.target.Scheme == "https" {
		return 443
	}
	return 80
}

// oneLine is b with control characters as spaces, trimmed, and cut to at
// most max bytes, for an error message.
func oneLine(b []byte, max int) string {
	if len(b) > max {
		b = b[:max]
	}
	s := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, string(b))
	return strings.TrimSpace(strings.ToValidUTF8(s, ""))
}

// launchWith asks exts what a launch of req.Package on t should carry and,
// when every answer passes, launches it within budget. The answers are
// fetched on ctx, outside the device's budget, since each may take up to
// launchSpecTimeout.
func (s *Server) launchWith(ctx context.Context, t target.LaunchInjector, id string, req InputRequest, exts []*registeredExtension, budget time.Duration) (*target.LaunchWithResult, *launchWithError) {
	specs, lerr := launchSpecs(ctx, t, id, req.Package, exts)
	if lerr != nil {
		return nil, lerr
	}
	dctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	res, err := t.LaunchWith(dctx, req.Package, req.Activity, specs)
	switch {
	case err == nil:
		return res, nil
	case errors.Is(err, target.ErrBadLaunchName):
		return nil, &launchWithError{status: http.StatusBadRequest, err: err}
	case errors.Is(err, android.ErrNotDebuggable):
		// The app was launched by now, without the agents.
		return nil, &launchWithError{status: http.StatusBadRequest, err: notDebuggable(req.Package)}
	}
	return nil, &launchWithError{status: http.StatusBadGateway, err: err}
}

// notDebuggable is a launch's refusal of an app run-as will not enter, in
// the words docs/extensions.md gives it.
type notDebuggable string

func (p notDebuggable) Error() string { return string(p) + " is not debuggable" }
func (notDebuggable) Unwrap() error   { return android.ErrNotDebuggable }

// launchWithTrace adds a launch with extensions to its trace record.
func launchWithTrace(rec map[string]any, names []string, res *target.LaunchWithResult) map[string]any {
	rec["with"] = names
	if res != nil && res.Relaunched {
		rec["relaunched"] = true
	}
	return rec
}
