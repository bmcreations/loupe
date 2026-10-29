package apischema

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/httpapi"
)

// TestGenerateIsDeterministic pins the property this package exists to
// give a caller: the same Go types produce byte-identical schema bytes on
// every run, not just an equivalent JSON structure. Two Generate() calls in
// the same process is the strongest version of that claim this test can
// make without shelling out to a second process.
func TestGenerateIsDeterministic(t *testing.T) {
	a, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	b, err := Generate()
	if err != nil {
		t.Fatalf("Generate (second call): %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("Generate is not deterministic: two calls in the same process produced different bytes")
	}
}

// TestGenerateProducesValidJSON is a cheap sanity check that Generate's
// output parses, before the more targeted structural assertions below.
func TestGenerateProducesValidJSON(t *testing.T) {
	b, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("Generate produced invalid JSON: %v", err)
	}
	if doc["$schema"] != schemaDialect {
		t.Errorf("$schema = %v, want %s", doc["$schema"], schemaDialect)
	}
	defs, ok := doc["$defs"].(map[string]any)
	if !ok {
		t.Fatalf("$defs is not an object: %T", doc["$defs"])
	}
	for _, ep := range endpoints {
		if _, ok := defs[ep.key]; !ok {
			t.Errorf("$defs is missing endpoint key %q", ep.key)
		}
	}
}

// TestSchemaFileMatchesGenerated is the drift test: docs/api.schema.json is
// checked in, and this fails the moment a response type changes without
// `go run ./cmd/apischema` being rerun to catch up. The failure message
// says exactly that, since "schema mismatch" alone does not tell a reader
// what to do about it.
func TestSchemaFileMatchesGenerated(t *testing.T) {
	generated, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	checkedIn, err := os.ReadFile(filepath.Join("..", "..", "docs", "api.schema.json"))
	if err != nil {
		t.Fatalf("reading docs/api.schema.json: %v", err)
	}
	if !bytes.Equal(generated, checkedIn) {
		t.Fatalf("docs/api.schema.json is out of date with the Go types it describes; " +
			"run `go run ./cmd/apischema` from the repo root and commit the result")
	}
}

// --- unit coverage for the individual kinds schemaFor handles ---

func newGen() *generator {
	return &generator{typeDefs: map[string]any{}, seen: map[reflect.Type]string{}}
}

func TestSchemaForBasicKinds(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want map[string]any
	}{
		{"string", "", map[string]any{"type": "string"}},
		{"bool", false, map[string]any{"type": "boolean"}},
		{"int", int(0), map[string]any{"type": "integer"}},
		{"uint64", uint64(0), map[string]any{"type": "integer"}},
		{"float64", float64(0), map[string]any{"type": "number"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newGen()
			got, err := g.schemaFor(reflect.TypeOf(c.v))
			if err != nil {
				t.Fatalf("schemaFor: %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("schemaFor(%s) = %#v, want %#v", c.name, got, c.want)
			}
		})
	}
}

func TestSchemaForAnyIsUnconstrained(t *testing.T) {
	g := newGen()
	var iface any
	got, err := g.schemaFor(reflect.TypeOf(&iface).Elem())
	if err != nil {
		t.Fatalf("schemaFor: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("schemaFor(any) = %#v, want an empty (unconstrained) schema", got)
	}
}

func TestSchemaForSlice(t *testing.T) {
	g := newGen()
	got, err := g.schemaFor(reflect.TypeOf([]string{}))
	if err != nil {
		t.Fatalf("schemaFor: %v", err)
	}
	want := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("schemaFor([]string) = %#v, want %#v", got, want)
	}
}

func TestSchemaForMap(t *testing.T) {
	g := newGen()
	got, err := g.schemaFor(reflect.TypeOf(map[string]int{}))
	if err != nil {
		t.Fatalf("schemaFor: %v", err)
	}
	want := map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "integer"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("schemaFor(map[string]int) = %#v, want %#v", got, want)
	}
}

func TestSchemaForMapRejectsNonStringKeys(t *testing.T) {
	g := newGen()
	if _, err := g.schemaFor(reflect.TypeOf(map[int]string{})); err == nil {
		t.Fatal("schemaFor(map[int]string) succeeded, want an error: JSON object keys are always strings")
	}
}

func TestSchemaForPointerIsNullable(t *testing.T) {
	g := newGen()
	got, err := g.schemaFor(reflect.TypeOf((*int)(nil)))
	if err != nil {
		t.Fatalf("schemaFor: %v", err)
	}
	want := map[string]any{
		"anyOf": []any{
			map[string]any{"type": "integer"},
			map[string]any{"type": "null"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("schemaFor(*int) = %#v, want %#v", got, want)
	}
}

// embedFlattenA/B mirror the shape TargetInfo uses: an outer struct embeds
// an inner one with no json tag of its own, so the inner struct's fields
// are promoted to the outer struct's properties instead of nesting under a
// key named after the inner type.
type embedFlattenInner struct {
	Inner string `json:"inner"`
}
type embedFlattenOuter struct {
	Outer string `json:"outer"`
	embedFlattenInner
}

func TestEmbeddedStructIsFlattened(t *testing.T) {
	g := newGen()
	ref, err := g.schemaFor(reflect.TypeOf(embedFlattenOuter{}))
	if err != nil {
		t.Fatalf("schemaFor: %v", err)
	}
	name := defName(reflect.TypeOf(embedFlattenOuter{}))
	if ref["$ref"] != "#/$defs/"+name {
		t.Fatalf("schemaFor(embedFlattenOuter) = %#v, want a $ref to %s", ref, name)
	}
	def := g.typeDefs[name].(map[string]any)
	props := def["properties"].(map[string]any)
	if _, ok := props["outer"]; !ok {
		t.Errorf("properties missing outer's own field %q: %#v", "outer", props)
	}
	if _, ok := props["inner"]; !ok {
		t.Errorf("properties missing the embedded struct's field %q -- embedding was not flattened: %#v", "inner", props)
	}
	if _, ok := props["embedFlattenInner"]; ok {
		t.Errorf("properties has a nested key for the embedded type itself; it should have been flattened, not nested: %#v", props)
	}
}

// requiredFixture and its omitempty counterpart pin that a field is
// required exactly when its tag lacks ",omitempty".
type requiredFixture struct {
	Always    string `json:"always"`
	Sometimes string `json:"sometimes,omitempty"`
}

func TestOmitemptyControlsRequired(t *testing.T) {
	g := newGen()
	if _, err := g.schemaFor(reflect.TypeOf(requiredFixture{})); err != nil {
		t.Fatalf("schemaFor: %v", err)
	}
	name := defName(reflect.TypeOf(requiredFixture{}))
	def := g.typeDefs[name].(map[string]any)
	required, _ := def["required"].([]string)
	if len(required) != 1 || required[0] != "always" {
		t.Errorf("required = %#v, want exactly [\"always\"]", required)
	}
}

// selfRef mirrors target.Node: a struct reachable from one of its own
// fields through a pointer slice. schemaFor must terminate rather than
// recurse forever, and the cycle must close with a $ref rather than an
// infinitely nested object.
type selfRef struct {
	Name     string     `json:"name"`
	Children []*selfRef `json:"children,omitempty"`
}

func TestSelfReferencingStructTerminates(t *testing.T) {
	g := newGen()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := g.schemaFor(reflect.TypeOf(selfRef{})); err != nil {
			t.Errorf("schemaFor: %v", err)
		}
	}()
	select {
	case <-done:
	case <-timeoutC(t):
		t.Fatal("schemaFor did not terminate on a self-referencing struct")
	}

	name := defName(reflect.TypeOf(selfRef{}))
	def := g.typeDefs[name].(map[string]any)
	props := def["properties"].(map[string]any)
	children := props["children"].(map[string]any)
	items := children["items"].(map[string]any)
	anyOf := items["anyOf"].([]any)
	ref := anyOf[0].(map[string]any)
	if ref["$ref"] != "#/$defs/"+name {
		t.Errorf("children item does not close the cycle with a $ref back to itself: %#v", ref)
	}
}

// reusedFixtureA/B both reference the same struct type; it must be defined
// once and referenced the second time, not duplicated.
type reusedLeaf struct {
	V string `json:"v"`
}
type reusedFixtureA struct {
	One reusedLeaf `json:"one"`
}
type reusedFixtureB struct {
	Two reusedLeaf `json:"two"`
}

func TestReusedStructTypeIsDefinedOnce(t *testing.T) {
	g := newGen()
	if _, err := g.schemaFor(reflect.TypeOf(reusedFixtureA{})); err != nil {
		t.Fatalf("schemaFor A: %v", err)
	}
	before := len(g.typeDefs)
	if _, err := g.schemaFor(reflect.TypeOf(reusedFixtureB{})); err != nil {
		t.Fatalf("schemaFor B: %v", err)
	}
	// A, B and reusedLeaf: exactly one new def (B) should have been added,
	// not two, since reusedLeaf was already defined while walking A.
	if got, want := len(g.typeDefs)-before, 1; got != want {
		t.Errorf("typeDefs grew by %d defining B, want %d (reusedLeaf should already be defined): %#v", got, want, g.typeDefs)
	}
}

// --- json.Marshaler handling ---

type overriddenMarshaler struct{ X int }

func (overriddenMarshaler) MarshalJSON() ([]byte, error) { return []byte("0"), nil }

type unknownMarshaler struct{ X int }

func (unknownMarshaler) MarshalJSON() ([]byte, error) { return []byte("0"), nil }

func TestMarshalerWithoutOverrideIsAnError(t *testing.T) {
	g := newGen()
	if _, err := g.schemaFor(reflect.TypeOf(unknownMarshaler{})); err == nil {
		t.Fatal("schemaFor succeeded for a json.Marshaler with no registered override, want a generation error")
	}
}

func TestMarshalerWithOverrideUsesIt(t *testing.T) {
	saved := marshalerSchemas[reflect.TypeOf(overriddenMarshaler{})]
	want := map[string]any{"type": "integer", "const": 0}
	marshalerSchemas[reflect.TypeOf(overriddenMarshaler{})] = want
	defer func() {
		if saved == nil {
			delete(marshalerSchemas, reflect.TypeOf(overriddenMarshaler{}))
		} else {
			marshalerSchemas[reflect.TypeOf(overriddenMarshaler{})] = saved
		}
	}()

	g := newGen()
	got, err := g.schemaFor(reflect.TypeOf(overriddenMarshaler{}))
	if err != nil {
		t.Fatalf("schemaFor: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("schemaFor(overriddenMarshaler) = %#v, want the registered override %#v", got, want)
	}
}

// TestPointerToMarshalerStaysNullable guards the ordering schemaFor relies
// on: a *T field must still produce a nullable wrapper even when T (and so
// *T, by promotion) implements json.Marshaler, rather than short-circuiting
// straight to T's override schema and silently dropping nullability.
func TestPointerToMarshalerStaysNullable(t *testing.T) {
	saved := marshalerSchemas[reflect.TypeOf(overriddenMarshaler{})]
	marshalerSchemas[reflect.TypeOf(overriddenMarshaler{})] = map[string]any{"type": "integer"}
	defer func() {
		if saved == nil {
			delete(marshalerSchemas, reflect.TypeOf(overriddenMarshaler{}))
		} else {
			marshalerSchemas[reflect.TypeOf(overriddenMarshaler{})] = saved
		}
	}()

	g := newGen()
	got, err := g.schemaFor(reflect.TypeOf((*overriddenMarshaler)(nil)))
	if err != nil {
		t.Fatalf("schemaFor: %v", err)
	}
	anyOf, ok := got["anyOf"].([]any)
	if !ok || len(anyOf) != 2 {
		t.Fatalf("schemaFor(*overriddenMarshaler) = %#v, want an anyOf[schema, null] wrapper", got)
	}
	null := anyOf[1].(map[string]any)
	if null["type"] != "null" {
		t.Errorf("second anyOf branch = %#v, want {\"type\":\"null\"}", null)
	}
}

func TestUnsupportedKindIsAnError(t *testing.T) {
	g := newGen()
	if _, err := g.schemaFor(reflect.TypeOf(make(chan int))); err == nil {
		t.Fatal("schemaFor succeeded for a chan type, want an error")
	}
}

func TestParseJSONTag(t *testing.T) {
	cases := []struct {
		tag             string
		wantName        string
		wantOmitempty   bool
		wantUnsupported string
	}{
		{"", "", false, ""},
		{"name", "name", false, ""},
		{"name,omitempty", "name", true, ""},
		{",omitempty", "", true, ""},
		{"name,string", "name", false, "string"},
		{"name,omitzero", "name", false, "omitzero"},
		{"name,omitempty,string", "name", true, "string"},
	}
	for _, c := range cases {
		name, omitempty, unsupported := parseJSONTag(c.tag)
		if name != c.wantName || omitempty != c.wantOmitempty || unsupported != c.wantUnsupported {
			t.Errorf("parseJSONTag(%q) = (%q, %v, %q), want (%q, %v, %q)",
				c.tag, name, omitempty, unsupported, c.wantName, c.wantOmitempty, c.wantUnsupported)
		}
	}
}

// TestStringTagOptionIsAnError pins item 5 of the CHANGES-REQUIRED review: a
// ",string" field is not reflected correctly (it changes the field's wire
// type from what its Go kind would otherwise describe), so Generate must
// refuse it rather than silently emit a schema for the wrong shape.
func TestStringTagOptionIsAnError(t *testing.T) {
	type hasStringTag struct {
		Count int `json:"count,string"`
	}
	g := newGen()
	if _, err := g.schemaFor(reflect.TypeOf(hasStringTag{})); err == nil {
		t.Fatal("schemaFor succeeded for a `,string` field, want an error")
	}
}

// TestOmitzeroTagOptionIsAnError is the "omitzero" half of the same pin.
func TestOmitzeroTagOptionIsAnError(t *testing.T) {
	type hasOmitzero struct {
		Count int `json:"count,omitzero"`
	}
	g := newGen()
	if _, err := g.schemaFor(reflect.TypeOf(hasOmitzero{})); err == nil {
		t.Fatal("schemaFor succeeded for a `,omitzero` field, want an error")
	}
}

// textMarshalerFixture implements encoding.TextMarshaler and nothing else,
// so encoding/json would encode it as a quoted string via MarshalText.
type textMarshalerFixture struct{}

func (textMarshalerFixture) MarshalText() ([]byte, error) { return []byte("x"), nil }

// TestUnregisteredTextMarshalerIsAnError pins the MarshalText half of item
// 5: a type reflected past this check would describe an object with no
// fields (textMarshalerFixture has none), not the quoted string the wire
// actually carries.
func TestUnregisteredTextMarshalerIsAnError(t *testing.T) {
	g := newGen()
	if _, err := g.schemaFor(reflect.TypeOf(textMarshalerFixture{})); err == nil {
		t.Fatal("schemaFor succeeded for an encoding.TextMarshaler, want an error")
	}
}

// TestJSONMarshalerTakesPriorityOverTextMarshaler pins the fallback order
// marshalerSchemaFor mirrors from encoding/json itself: a type implementing
// both is encoded (and so must be schematized) via MarshalJSON, never
// MarshalText. session.Point does not implement TextMarshaler, so this uses
// a fixture that implements both to prove the order rather than assume it.
type bothMarshalersFixture struct{}

func (bothMarshalersFixture) MarshalJSON() ([]byte, error) { return []byte(`"j"`), nil }
func (bothMarshalersFixture) MarshalText() ([]byte, error) { return []byte("t"), nil }

func TestJSONMarshalerTakesPriorityOverTextMarshaler(t *testing.T) {
	rt := reflect.TypeOf(bothMarshalersFixture{})
	restore := marshalerSchemas[rt]
	marshalerSchemas[rt] = map[string]any{"type": "string"}
	defer func() {
		if restore == nil {
			delete(marshalerSchemas, rt)
		} else {
			marshalerSchemas[rt] = restore
		}
	}()

	g := newGen()
	got, err := g.schemaFor(rt)
	if err != nil {
		t.Fatalf("schemaFor: %v", err)
	}
	if got["type"] != "string" {
		t.Errorf("schemaFor(bothMarshalersFixture) = %#v, want the registered json.Marshaler schema, not a TextMarshaler error or field reflection", got)
	}
}

func TestDefNameIncludesPackage(t *testing.T) {
	got := defName(reflect.TypeOf(requiredFixture{}))
	if !strings.HasSuffix(got, ".requiredFixture") {
		t.Errorf("defName = %q, want it to end with %q", got, ".requiredFixture")
	}
	if !strings.Contains(got, "apischema") {
		t.Errorf("defName = %q, want it to include the package name", got)
	}
}

// routeExemptions are routes server.go registers that deliberately have no
// entry in endpoints, each with the one-line reason it is not a JSON
// response schema can describe.
var routeExemptions = map[string]string{
	"/api/screenshot":     "returns a PNG, not JSON",
	"/api/frame/asset":    "returns skin art (webp/png), not JSON",
	"/api/overlays/image": "returns an overlay's picture as PNG, not JSON",
	"/api/flags/shot":     "returns a flag's screenshot as PNG, not JSON",
	"/api/ext/":           "passes through to a registered extension, whose responses loupe does not read",
	"/api/logcat":         "returns logcat's threadtime text, not JSON",
	"/api/crashes/":       "returns one .ips report as it was written, two JSON documents read as text",
	"/api/record/":        "an id-addressed subtree: an MP4, a JPEG, metadata or 204",
	"/api/log":            "accepts client telemetry; writes no body worth pinning",
	"/api/stats":          "internal polling endpoint, not a documented client integration point",
	"/healthz":            "a supervisor poll target (OKResponse's shape is still covered as a shared body), not a documented client integration point",
	"/ws":                 "a WebSocket upgrade, not a JSON response",
	"/wt":                 "a WebTransport upgrade, not a JSON response",
}

// TestEveryJSONRouteHasASchemaEntry walks httpapi.Routes() -- the exact
// table server.go's Handler registers from, one entry per method a route
// actually answers -- and fails if a method+path pair neither has a
// *response* row in endpoints nor is named in routeExemptions. This is what
// stops a new handler's response type from silently missing the schema:
// Routes and Handler share one table (see server.go's route type), so this
// test cannot pass by checking a list nothing actually serves from.
//
// Coverage is built only from dirResponse rows, and keyed by method+path,
// not path alone. Two things that keyed-on-path coverage missed motivated
// this: a request-only row (PUT /api/settings request) would otherwise mark
// the path covered with no response schema in sight, and GET and PUT on the
// same path (/api/config, /api/settings) would satisfy each other -- a
// deleted PUT /api/settings 200 row still left GET /api/settings's row
// making the path look covered.
func TestEveryJSONRouteHasASchemaEntry(t *testing.T) {
	covered := map[string]bool{}
	for _, ep := range endpoints {
		if ep.dir != dirResponse {
			continue
		}
		if m, p, ok := endpointRoute(ep.key); ok {
			covered[m+" "+p] = true
		}
	}

	for _, rt := range httpapi.Routes() {
		if _, exempt := routeExemptions[rt.Path]; exempt {
			continue
		}
		if !covered[rt.Method+" "+rt.Path] {
			t.Errorf("route %s %q is registered in server.go but has no response entry in endpoints and no entry in routeExemptions", rt.Method, rt.Path)
		}
	}
}

// TestRouteExemptionsAreActuallyRegistered guards the other direction: an
// exemption naming a route server.go no longer registers would sit unread
// forever and stop meaning anything.
func TestRouteExemptionsAreActuallyRegistered(t *testing.T) {
	registered := map[string]bool{}
	for _, rt := range httpapi.Routes() {
		registered[rt.Path] = true
	}
	for p := range routeExemptions {
		if !registered[p] {
			t.Errorf("routeExemptions names %q, which server.go no longer registers", p)
		}
	}
}

// TestEveryEndpointRouteIsActuallyRegistered guards the direction
// TestEveryJSONRouteHasASchemaEntry cannot: an endpoints row naming a
// method+path server.go does not register would sit here describing an
// endpoint that answers 404, with nothing to say so, since that test only
// walks httpapi.Routes() outward and never checks a row's path exists at
// all.
func TestEveryEndpointRouteIsActuallyRegistered(t *testing.T) {
	registered := map[string]bool{}
	for _, rt := range httpapi.Routes() {
		registered[rt.Method+" "+rt.Path] = true
	}
	for _, ep := range endpoints {
		m, p, ok := endpointRoute(ep.key)
		if !ok {
			continue // a shared body (ErrorResponse, OKResponse), not tied to one route.
		}
		if !registered[m+" "+p] {
			t.Errorf("endpoints entry %q names %s %q, which server.go does not register", ep.key, m, p)
		}
	}
}

// TestARemovedResponseRowIsCaughtEvenWhenAnotherRowSharesItsPath is the
// regression this test's redesign exists for: /api/settings answers both
// GET and PUT, so deleting the GET row must fail coverage even though PUT's
// rows for the same path are untouched. Keying coverage on path alone let
// this mutation survive -- GET /api/settings would still read as "covered"
// because PUT /api/settings 200 and 409 cover the same path string.
func TestARemovedResponseRowIsCaughtEvenWhenAnotherRowSharesItsPath(t *testing.T) {
	saved := endpoints
	defer func() { endpoints = saved }()

	filtered := make([]endpoint, 0, len(endpoints))
	for _, ep := range endpoints {
		if ep.key == "GET /api/settings" {
			continue
		}
		filtered = append(filtered, ep)
	}
	endpoints = filtered

	covered := map[string]bool{}
	for _, ep := range endpoints {
		if ep.dir != dirResponse {
			continue
		}
		if m, p, ok := endpointRoute(ep.key); ok {
			covered[m+" "+p] = true
		}
	}
	if covered["GET /api/settings"] {
		t.Fatalf("GET /api/settings still reads as covered after removing its only response row, because PUT /api/settings shares the path")
	}
	if !covered["PUT /api/settings"] {
		t.Fatalf("PUT /api/settings lost coverage as a side effect of removing the unrelated GET row -- method+path keying should not do that")
	}
}

// TestFieldOverridesNamingNoRealFieldFailsGeneration is round-4 item 4's
// first half: a fieldOverrides entry naming a field neither InputRequest nor
// any other overridden type actually has -- a typo, or a field renamed out
// from under the override -- must fail generation rather than silently
// applying to nothing. validateFieldOverrides is what Generate calls before
// it reflects anything else, so this drives it directly with a planted typo
// and restores the real table afterwards.
func TestFieldOverridesNamingNoRealFieldFailsGeneration(t *testing.T) {
	saved := fieldOverrides
	defer func() { fieldOverrides = saved }()

	fieldOverrides = map[reflect.Type]map[string]map[string]any{
		reflect.TypeOf(httpapi.InputRequest{}): {
			"buttn": {"enum": []any{"back"}}, // typo: InputRequest has "button", not "buttn"
		},
	}
	if err := validateFieldOverrides(); err == nil {
		t.Fatalf("validateFieldOverrides: want an error for a field name InputRequest does not have, got nil")
	}

	if _, err := Generate(); err == nil {
		t.Fatalf("Generate: want an error for the same reason, got nil")
	}
}

// TestMergeFieldOverridePutsConstraintOnTheIntegerBranch is round-4 item 4's
// second half. A nullable field's schema is the anyOf shape schemaFor's
// pointer case produces -- {"anyOf": [inner, {"type": "null"}]} -- and a
// range or enum belongs on inner, the non-null branch: merging it at the
// schema's own top level would attach it beside "anyOf" instead, which no
// validator applies to the null case and so constrains nothing extra. This
// checks the constraint lands where it has to, not merely that mergeSchema
// returns without error.
func TestMergeFieldOverridePutsConstraintOnTheIntegerBranch(t *testing.T) {
	nullable := map[string]any{
		"anyOf": []any{
			map[string]any{"type": "integer"},
			map[string]any{"type": "null"},
		},
	}
	mergeFieldOverride(nullable, map[string]any{"minimum": 0, "maximum": 3})

	anyOf, ok := nullable["anyOf"].([]any)
	if !ok || len(anyOf) != 2 {
		t.Fatalf("nullable = %#v; anyOf shape was not preserved", nullable)
	}
	inner, ok := anyOf[0].(map[string]any)
	if !ok {
		t.Fatalf("anyOf[0] = %#v, want the integer branch as a map", anyOf[0])
	}
	if inner["type"] != "integer" {
		t.Fatalf("anyOf[0] = %#v; the integer branch itself must be untouched", inner)
	}
	if inner["minimum"] != 0 || inner["maximum"] != 3 {
		t.Fatalf("anyOf[0] = %#v; want minimum/maximum merged onto the integer branch", inner)
	}
	if _, onTop := nullable["minimum"]; onTop {
		t.Fatalf("nullable = %#v; minimum leaked onto the top-level anyOf schema, where no validator applies it to the null case", nullable)
	}
	nullBranch, ok := anyOf[1].(map[string]any)
	if !ok || nullBranch["type"] != "null" {
		t.Fatalf("anyOf[1] = %#v; the null branch must be untouched", anyOf[1])
	}
	if _, onNull := nullBranch["minimum"]; onNull {
		t.Fatalf("anyOf[1] = %#v; minimum leaked onto the null branch", nullBranch)
	}
}

// timeoutC bounds how long a test waits for schemaFor to terminate. A
// self-referencing struct that failed to break its own cycle would hang the
// test process; this turns that hang into an ordinary failure instead.
// Generous on purpose -- this is a correctness guard, not a performance test.
func timeoutC(t *testing.T) <-chan time.Time {
	t.Helper()
	return time.After(5 * time.Second)
}
