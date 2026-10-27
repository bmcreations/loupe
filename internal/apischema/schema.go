// Package apischema reflects over the server's HTTP response types and
// emits one JSON Schema document (draft 2020-12) describing them, keyed by
// endpoint. It exists so the wire contract in docs/api.schema.json is
// generated from the Go types a handler actually writes, rather than
// hand-maintained and left to drift the moment a field is renamed.
//
// Generate walks a fixed list (endpoints, below) rather than discovering
// response types on its own: a new JSON field silently reaching a client
// with no corresponding change here is exactly what the drift test in
// schema_test.go exists to catch, and that needs a fixed inventory to check
// against.
package apischema

import (
	"encoding"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"reflect"
	"strings"
	"time"

	"github.com/bmcreations/loupe/internal/config"
	"github.com/bmcreations/loupe/internal/httpapi"
	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
)

// schemaDialect is the JSON Schema dialect every generated document declares.
const schemaDialect = "https://json-schema.org/draft/2020-12/schema"

// direction says which way an endpoint entry's bytes travel. A request and a
// response can legitimately share a key's method-and-path prefix (PUT
// /api/config both reads a patch and writes back what changed), so the two
// are kept as separate rows rather than one row trying to describe both.
type direction string

const (
	dirRequest  direction = "request"
	dirResponse direction = "response"
)

// endpoint is one entry in the fixed list Generate walks.
type endpoint struct {
	// key names the entry under $defs: method and path, or path plus a
	// status code when one endpoint has more than one response shape (a
	// settings PUT answers 200 with SettingsResponse and 409 with
	// SettingsConflictResponse). A shared body used by more than one
	// endpoint (ErrorResponse, OKResponse) is keyed by its own name rather
	// than any one path, since it does not belong to a single endpoint.
	key   string
	dir   direction
	value any
}

// endpoints is the fixed list of request and response shapes Generate
// describes. Adding or changing a handler's decoded or written type means
// adding or changing the entry here -- that edit, and the regenerated
// docs/api.schema.json that follows it, is the whole point of the drift
// test. TestEveryJSONRouteHasASchemaEntry checks this list's response rows
// against every route server.go actually registers, less the exemptions
// named there.
//
// Left out deliberately: the plain-text (non-JSON) bodies http.Error writes
// on paths ErrorResponse does not reach. Two 405s -- "POST only" in
// agent.go's handleInput and "GET only" in events.go's handleEvents -- and
// every protected endpoint's 401, written by auth.go's middleware before a
// handler (and its JSON error responses) ever runs. All three predate
// ErrorResponse, are not JSON, and are not changed by this package; a
// client parsing any of their error paths as JSON was already wrong before
// this file existed.
var endpoints = []endpoint{
	{"GET /api/targets", dirResponse, []httpapi.TargetInfo{}},
	{"GET /api/servers", dirResponse, []httpapi.ServerInfo{}},
	{"POST /api/servers request", dirRequest, httpapi.ServerAddRequest{}},
	{"POST /api/servers 201", dirResponse, httpapi.ServerInfo{}},
	{"POST /api/servers 409", dirResponse, httpapi.ErrorResponse{}},
	{"POST /api/servers 502", dirResponse, httpapi.ErrorResponse{}},
	{"DELETE /api/servers 404", dirResponse, httpapi.ErrorResponse{}},
	{"GET /api/ui", dirResponse, httpapi.UIResponse{}},
	{"GET /api/app", dirResponse, target.App{}},
	{"GET /api/settings", dirResponse, httpapi.SettingsResponse{}},
	{"PUT /api/settings request", dirRequest, httpapi.SettingsPutRequest{}},
	{"PUT /api/settings 200", dirResponse, httpapi.SettingsResponse{}},
	{"PUT /api/settings 409", dirResponse, httpapi.SettingsConflictResponse{}},
	{"GET /api/config", dirResponse, httpapi.ConfigResponse{}},
	// config.Merge decodes a PUT /api/config body into exactly this type --
	// see internal/config.Merge -- before re-marshaling the current config
	// and overlaying the patch key by key. That decode step is genuinely
	// untyped per field (a raw JSON value, checked only after decoding
	// against the known field names and the base config's own types), so
	// map[string]json.RawMessage is what the wire contract actually is: an
	// object whose keys are a subset of config.Config's field names and
	// whose values are left unconstrained here rather than described as
	// config.Config's (non-optional) field types, which would wrongly
	// require every field on every patch.
	{"PUT /api/config request", dirRequest, map[string]json.RawMessage{}},
	{"PUT /api/config", dirResponse, httpapi.PutResponse{}},
	{"GET /api/frame", dirResponse, target.DeviceFrame{}},
	{"GET /api/events", dirResponse, session.EventPage{}},
	{"POST /api/input request", dirRequest, httpapi.InputRequest{}},
	{"POST /api/input 200", dirResponse, httpapi.OKResponse{}},
	{"POST /api/install 200", dirResponse, httpapi.InstallResponse{}},
	{"POST /api/install 422", dirResponse, httpapi.InstallResponse{}},
	{"POST /api/media 200", dirResponse, httpapi.MediaResponse{}},
	{"GET /api/clipboard", dirResponse, httpapi.ClipboardResponse{}},
	{"POST /api/clipboard request", dirRequest, httpapi.ClipboardRequest{}},
	{"POST /api/clipboard 200", dirResponse, httpapi.OKResponse{}},
	{"GET /api/avds/profiles", dirResponse, httpapi.AVDProfilesResponse{}},
	{"POST /api/avds request", dirRequest, httpapi.CreateAVDRequest{}},
	{"POST /api/avds 202", dirResponse, httpapi.CreateAVDResponse{}},
	{"GET /api/devices/options", dirResponse, httpapi.DeviceOptionsResponse{}},
	{"POST /api/devices request", dirRequest, httpapi.CreateDeviceRequest{}},
	{"POST /api/devices 202", dirResponse, httpapi.CreateDeviceResponse{}},
	{"POST /api/devices/shutdown 200", dirResponse, httpapi.TargetInfo{}},
	// 409 with code "recording" (a recording is in progress, id names it), "busy" (another
	// shut down or delete of the target is running) or "humanHeld".
	{"POST /api/devices/shutdown 409", dirResponse, httpapi.ErrorResponse{}},
	{"DELETE /api/devices 409", dirResponse, httpapi.ErrorResponse{}},
	{"POST /api/devices/reset 200", dirResponse, httpapi.TargetInfo{}},
	// 409 with code "recording", "busy" or "humanHeld", as for shutdown.
	{"POST /api/devices/reset 409", dirResponse, httpapi.ErrorResponse{}},
	// 200 also when the target was already running.
	{"POST /api/devices/boot 200", dirResponse, httpapi.TargetInfo{}},
	// 409 with code "busy" (a shut down or delete of the target is running) or "humanHeld".
	{"POST /api/devices/boot 409", dirResponse, httpapi.ErrorResponse{}},
	{"POST /api/devices/handsoff 200", dirResponse, httpapi.HandsOffResponse{}},
	{"GET /api/claims", dirResponse, []httpapi.ClaimInfo{}},
	{"POST /api/claims request", dirRequest, httpapi.ClaimRequest{}},
	{"POST /api/claims 200", dirResponse, httpapi.ClaimInfo{}},
	// 409 with code "claimedBy": another agent holds it, `claim` says who.
	{"POST /api/claims 409", dirResponse, httpapi.ErrorResponse{}},
	{"DELETE /api/claims 409", dirResponse, httpapi.ErrorResponse{}},
	{"GET /api/crashes", dirResponse, httpapi.CrashList{}},
	{"GET /api/extensions", dirResponse, httpapi.ExtensionsResponse{}},
	{"POST /api/extensions request", dirRequest, httpapi.ExtensionRequest{}},
	{"POST /api/extensions 200", dirResponse, httpapi.Extension{}},
	{"DELETE /api/extensions", dirResponse, httpapi.OKResponse{}},
	{"GET /api/handoff", dirResponse, httpapi.HandoffResponse{}},
	{"GET /api/overlays", dirResponse, httpapi.OverlaysResponse{}},
	{"POST /api/overlays request", dirRequest, httpapi.OverlayRequest{}},
	{"POST /api/overlays 200", dirResponse, httpapi.Overlay{}},
	// 409: the target already has the most overlays it may hold.
	{"POST /api/overlays 409", dirResponse, httpapi.ErrorResponse{}},
	{"DELETE /api/overlays", dirResponse, httpapi.OverlayDeleteResponse{}},
	{"GET /api/record", dirResponse, httpapi.RecordListResponse{}},
	{"POST /api/record 201", dirResponse, httpapi.RecordStartResponse{}},
	// 409 with code "recording": the target is already recording.
	{"POST /api/record 409", dirResponse, httpapi.ErrorResponse{}},

	// Shared bodies, not tied to one path. ErrorResponse is what every JSON
	// endpoint above writes on its error paths; OKResponse is also what
	// /healthz writes, exempted from route coverage below because it is a
	// supervisor poll target rather than a documented client integration
	// point, not because its body is undocumented.
	{"ErrorResponse", dirResponse, httpapi.ErrorResponse{}},
	{"OKResponse", dirResponse, httpapi.OKResponse{}},
}

// endpointRoute extracts the method and path an endpoint key names, for keys
// shaped "METHOD /path" or "METHOD /path <suffix>" (a status code or the
// word "request"). A key with no leading HTTP method -- the shared
// ErrorResponse and OKResponse rows -- reports ok=false: it does not belong
// to one route, so it has nothing for TestEveryJSONRouteHasASchemaEntry to
// match against.
func endpointRoute(key string) (method, path string, ok bool) {
	parts := strings.Fields(key)
	if len(parts) < 2 {
		return "", "", false
	}
	switch parts[0] {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
	default:
		return "", "", false
	}
	if !strings.HasPrefix(parts[1], "/") {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// marshalerSchemas gives the schema for a type that implements
// json.Marshaler by hand, because reflecting over its Go fields would
// describe its Go shape, not its wire shape. session.Point is the one type
// reachable from endpoints that does this: MarshalJSON encodes it as a
// two-element [x, y] array, not an object with X/Y fields.
//
// json.RawMessage also implements it (value receiver, so it matches here
// rather than falling through to the Slice case below), and is reachable
// through the PUT /api/config request row: its MarshalJSON passes the bytes
// through unchanged, so it can be any JSON value, and the unconstrained
// schema below says exactly that -- the same one the Interface case in
// schemaFor gives "any" for.
//
// A type that implements json.Marshaler and has no entry here is a
// generation error (see marshalerSchemaFor) rather than a schema reflected
// off fields the marshaler will never actually emit.
var marshalerSchemas = map[reflect.Type]map[string]any{
	reflect.TypeOf(session.Point{}): {
		"type":     "array",
		"items":    map[string]any{"type": "number"},
		"minItems": 2,
		"maxItems": 2,
	},
	reflect.TypeOf(json.RawMessage{}): {},
	// A recording's started and ended times: MarshalJSON writes RFC 3339.
	reflect.TypeOf(time.Time{}): {"type": "string", "format": "date-time"},
}

// fieldOverrides adds constraints a handler enforces that reflection over a
// Go field's type cannot derive on its own -- the exact set of strings a
// handler accepts, or a numeric range it checks before ever calling into a
// target -- keyed by the request struct's Go type and the field's wire
// (JSON) name. Only meaningful for a dirRequest entry: a response field's
// possible values are already exactly what the Go type allows to be
// written, so there is nothing an override could narrow.
//
// Every entry here is read off the handler that decodes the type, not
// guessed independently of it, so it cannot drift into claiming a stricter
// or looser contract than the server actually enforces:
//   - "button" is target.ButtonNames(), the same table handleInput checks
//     req.Button against before resolving it to a keycode.
//   - "to" is httpapi.InputToMin/InputToMax, the range handleInput itself
//     rejects a rotate request for falling outside, before the Rotator is
//     ever called.
//   - "scale" is above 0, which handleInput checks before calling the
//     Pincher.
//
// InputRequest's "action" field carries no entry: since inputActionSchemas
// (below) gives POST /api/input's request body one oneOf variant per
// httpapi.InputActions entry, each variant's own "action" property is
// already a const naming that one action, which is narrower than any enum
// override could express here.
//
// Generate validates every key against the type's actual fields
// (validateFieldOverrides) before it reflects anything, so a typo here --
// naming a field neither InputRequest nor any other overridden type has --
// fails generation rather than silently applying to nothing.
var fieldOverrides = map[reflect.Type]map[string]map[string]any{
	reflect.TypeOf(httpapi.InputRequest{}): {
		"button": {"enum": stringsToAny(target.ButtonNames())},
		"to":     {"minimum": httpapi.InputToMin, "maximum": httpapi.InputToMax},
		"scale":  {"exclusiveMinimum": 0},
	},
}

// validateFieldOverrides checks that every field name in fieldOverrides
// actually appears on the struct type it is keyed under, so a typo'd or
// stale entry -- one that names no real field -- fails generation instead of
// silently applying to nothing. Generate calls this before it reflects
// anything else.
func validateFieldOverrides() error {
	for t, overrides := range fieldOverrides {
		names, err := structFieldNames(t)
		if err != nil {
			return err
		}
		for field := range overrides {
			if !names[field] {
				return fmt.Errorf("apischema: fieldOverrides names %s.%s, which has no field with that JSON name", t, field)
			}
		}
	}
	return nil
}

// structFieldNames returns every JSON field name t's fields marshal under,
// with an embedded struct's own fields flattened in the same way
// collectFields promotes them -- close enough to that function's logic to
// answer "does this struct have a field named X", which is all
// validateFieldOverrides needs.
func structFieldNames(t reflect.Type) (map[string]bool, error) {
	names := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" && !f.Anonymous {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, unsupported := parseJSONTag(tag)
		if unsupported != "" {
			return nil, fmt.Errorf("apischema: %s.%s: %q is not reflected correctly by this package", t.Name(), f.Name, unsupported)
		}
		if f.Anonymous && name == "" {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				inner, err := structFieldNames(ft)
				if err != nil {
					return nil, err
				}
				for k := range inner {
					names[k] = true
				}
				continue
			}
		}
		if name == "" {
			name = f.Name
		}
		names[name] = true
	}
	return names, nil
}

func stringsToAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// mergeFieldOverride adds override's keys into s, the schema schemaFor
// already produced for a field. A nullable field's schema is
// {"anyOf": [inner, {"type": "null"}]} (see schemaFor's pointer case), and a
// range or enum belongs on the non-null branch -- merging it at the top
// level would attach "minimum" to a schema note that also allows null,
// which draft 2020-12 permits but no validator would apply to the null
// case, so it would silently constrain nothing extra. Every override table
// entry today (InputRequest's "to") is exactly this shape, so the anyOf
// case is not speculative.
func mergeFieldOverride(s map[string]any, override map[string]any) {
	if anyOf, ok := s["anyOf"].([]any); ok {
		if inner, ok := anyOf[0].(map[string]any); ok {
			for k, v := range override {
				inner[k] = v
			}
			return
		}
	}
	for k, v := range override {
		s[k] = v
	}
}

// configFieldTypes maps each config.Fields() name to the reflect.Type of the
// config.Config struct field carrying that JSON tag, so
// endpointSchemaPatches can give PUT /api/config's per-field schema the same
// type config.Merge will eventually decode it as -- an int field rejects
// {"maxSize":"x"} at the schema rather than only at config.Merge's own
// json.Unmarshal, which fails the same way but only once the request is
// already on the wire. Built once by reflecting over config.Config itself,
// so a field renamed there cannot silently leave this table stale the way a
// hand-copied one could.
func configFieldTypes() (map[string]reflect.Type, error) {
	t := reflect.TypeOf(config.Config{})
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, unsupported := parseJSONTag(f.Tag.Get("json"))
		if unsupported != "" {
			return nil, fmt.Errorf("apischema: config.Config.%s: %q is not reflected correctly by this package", f.Name, unsupported)
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out, nil
}

// endpointSchemaPatches adjusts a whole endpoint's generated schema by hand,
// for cases reflection over the Go type alone cannot describe. Each entry
// takes the generator, so a patch can call schemaFor to describe a field's
// real type rather than writing an inline schema by hand.
var endpointSchemaPatches = map[string]func(*generator, map[string]any) error{
	// PUT /api/config's request body decodes into map[string]json.RawMessage
	// (see that endpoints entry's comment), which reflects to an object with
	// unconstrained values and no named properties. config.Merge accepts
	// only the keys config.Fields() names and rejects any other key with a
	// 400 (see internal/config.Merge's unknown-settings check), so the
	// schema can say exactly that: every config field name as an optional
	// property -- a PUT patches whatever subset it names, so none is
	// required -- typed from the matching config.Config field
	// (configFieldTypes) so a value of the wrong JSON type fails validation
	// before it ever reaches config.Merge, and additionalProperties false
	// for every other key, matching the 400.
	"PUT /api/config request": func(g *generator, s map[string]any) error {
		types, err := configFieldTypes()
		if err != nil {
			return err
		}
		props := map[string]any{}
		for _, f := range config.Fields() {
			ft, ok := types[f.Name]
			if !ok {
				return fmt.Errorf("apischema: config.Fields() names %q, which config.Config has no matching JSON field for", f.Name)
			}
			fs, err := g.schemaFor(ft)
			if err != nil {
				return fmt.Errorf("apischema: config field %q: %w", f.Name, err)
			}
			props[f.Name] = fs
		}
		s["properties"] = props
		s["additionalProperties"] = false
		return nil
	},
	// PUT /api/settings's request body is {"changes": {...}}, and
	// putSettings 400s outright on an empty Changes map (see settings.go),
	// so an empty object is not a valid request even though every one of
	// SettingsPutRequest's own fields is otherwise unconstrained.
	//
	// SettingsPutRequest is a named struct, so schemaFor gave the endpoint's
	// own schema (s, the argument here) only a $ref -- the object with
	// "changes" as a property lives in g.typeDefs under the type's own def
	// name (structRef), which is what has to be patched.
	"PUT /api/settings request": func(g *generator, s map[string]any) error {
		def, ok := g.typeDefs[defName(reflect.TypeOf(httpapi.SettingsPutRequest{}))].(map[string]any)
		if !ok {
			return fmt.Errorf("apischema: PUT /api/settings request: SettingsPutRequest's own def is missing")
		}
		props, ok := def["properties"].(map[string]any)
		if !ok {
			return fmt.Errorf("apischema: PUT /api/settings request: no properties to patch")
		}
		changes, ok := props["changes"].(map[string]any)
		if !ok {
			return fmt.Errorf("apischema: PUT /api/settings request: no changes property to patch")
		}
		changes["minProperties"] = 1
		return nil
	},
	// POST /api/input's request body is a discriminated union on "action":
	// see inputActionSchemas. Same indirection as the settings patch above --
	// InputRequest is a named struct, so the object schema to replace with a
	// oneOf lives in g.typeDefs, not in s (the $ref the endpoint itself
	// resolves to).
	"POST /api/input request": func(g *generator, s map[string]any) error {
		variants, err := inputActionSchemas(g)
		if err != nil {
			return err
		}
		name := defName(reflect.TypeOf(httpapi.InputRequest{}))
		if _, ok := g.typeDefs[name]; !ok {
			return fmt.Errorf("apischema: POST /api/input request: InputRequest's own def is missing")
		}
		g.typeDefs[name] = map[string]any{"oneOf": variants}
		return nil
	},
}

// inputActionSchemas builds one object schema per httpapi.InputActions
// entry, each with "action" pinned to a const naming that one action and
// "required" set from httpapi.InputRequiredFields[action] -- the same table
// validateInputRequest checks the raw request body against, so a generated
// client's sealed type and the handler's own presence check cannot drift
// apart. Properties come from reflecting over httpapi.InputRequest itself
// (including any fieldOverrides), so a field's type or constraint is
// described once regardless of how many variants use it.
func inputActionSchemas(g *generator) ([]any, error) {
	base, err := g.objectSchema(reflect.TypeOf(httpapi.InputRequest{}))
	if err != nil {
		return nil, err
	}
	props, ok := base["properties"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("apischema: InputRequest: no properties to build oneOf variants from")
	}

	variants := make([]any, 0, len(httpapi.InputActions))
	for _, action := range httpapi.InputActions {
		required, ok := httpapi.InputRequiredFields[action]
		if !ok {
			return nil, fmt.Errorf("apischema: InputActions names %q, which InputRequiredFields has no entry for", action)
		}
		// Every InputRequest property is described in every variant, typed
		// exactly as the base schema gives it, so a field that is optional
		// for this action (ms on a swipe, to on a rotate) is still checked
		// against its real type when present -- only "required" differs
		// between variants, and "action" is narrowed to a const naming this
		// one action rather than the full enum every variant would
		// otherwise share.
		variantProps := make(map[string]any, len(props))
		for field, fs := range props {
			variantProps[field] = fs
		}
		variantProps["action"] = map[string]any{"const": action}
		for _, field := range required {
			if _, ok := props[field]; !ok {
				return nil, fmt.Errorf("apischema: InputRequiredFields[%q] names %q, which InputRequest has no field for", action, field)
			}
		}
		variants = append(variants, map[string]any{
			"type":                 "object",
			"properties":           variantProps,
			"required":             append([]string{"action"}, required...),
			"additionalProperties": true,
		})
	}
	return variants, nil
}

var jsonMarshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()

// textMarshalerType is checked the same way jsonMarshalerType is, but with
// no override table and always an error: encoding/json falls back to
// encoding.TextMarshaler for a type that does not implement json.Marshaler,
// quoting whatever MarshalText returns as a JSON string. Nothing reachable
// from endpoints does this today, and reflecting such a type's Go fields
// would describe an object where the wire carries a quoted string, which is
// as wrong as skipping an unregistered json.Marshaler override silently.
// There is no override map to match marshalerSchemas because every type
// that would need one can instead implement json.Marshaler and be added
// there, which is one mechanism instead of two.
var textMarshalerType = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()

// Generate reflects over endpoints and returns a formatted JSON Schema
// document. Output is deterministic: property and $defs ordering comes from
// Go's own sorted-map-key marshaling, and every other list (struct fields,
// the endpoint table) is walked in a fixed, source-order sequence, so two
// calls with the same binary produce byte-identical output. See
// TestGenerateIsDeterministic.
func Generate() ([]byte, error) {
	if err := validateFieldOverrides(); err != nil {
		return nil, err
	}

	g := &generator{
		typeDefs: map[string]any{},
		seen:     map[reflect.Type]string{},
	}

	defs := map[string]any{}
	for _, ep := range endpoints {
		s, err := g.schemaFor(reflect.TypeOf(ep.value))
		if err != nil {
			return nil, fmt.Errorf("apischema: %s: %w", ep.key, err)
		}
		if patch, ok := endpointSchemaPatches[ep.key]; ok {
			if err := patch(g, s); err != nil {
				return nil, fmt.Errorf("apischema: %s: %w", ep.key, err)
			}
		}
		if _, exists := defs[ep.key]; exists {
			return nil, fmt.Errorf("apischema: duplicate endpoint key %q", ep.key)
		}
		defs[ep.key] = s
	}
	for name, s := range g.typeDefs {
		if _, exists := defs[name]; exists {
			return nil, fmt.Errorf("apischema: type def %q collides with an endpoint key", name)
		}
		defs[name] = s
	}

	doc := map[string]any{
		"$schema": schemaDialect,
		"$defs":   defs,
	}

	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("apischema: marshal schema: %w", err)
	}
	return append(b, '\n'), nil
}

// generator carries the state one Generate call accumulates: the named
// struct types it has turned into $defs entries, and which reflect.Type maps
// to which def name, so a type reached twice (target.Setting, from both
// SettingsResponse and SettingsConflictResponse) is defined once and
// referenced the second time, and a self-referencing type (target.Node,
// through Children) terminates instead of recursing forever.
type generator struct {
	typeDefs map[string]any
	seen     map[reflect.Type]string
}

// schemaFor returns the schema for t: a $ref into typeDefs for a named
// struct type, or an inline schema for everything else (a pointer's
// nullable wrapper, a slice or array's items, a map's additionalProperties,
// or a base kind).
func (g *generator) schemaFor(t reflect.Type) (map[string]any, error) {
	// Pointer is handled before the json.Marshaler check below, not after:
	// *session.Point also implements json.Marshaler (a value-receiver method
	// promotes to the pointer's method set), and checking that first would
	// hand back Point's array schema directly for a *Point field, losing the
	// fact that the pointer itself is nullable. Every other kind is checked
	// for a marshaler override on its own, non-pointer type.
	if t.Kind() == reflect.Pointer {
		inner, err := g.schemaFor(t.Elem())
		if err != nil {
			return nil, err
		}
		return map[string]any{"anyOf": []any{inner, map[string]any{"type": "null"}}}, nil
	}

	if s, err, matched := marshalerSchemaFor(t); matched {
		return s, err
	}

	switch t.Kind() {
	case reflect.Struct:
		return g.structRef(t)

	case reflect.Slice:
		items, err := g.schemaFor(t.Elem())
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "array", "items": items}, nil

	case reflect.Array:
		items, err := g.schemaFor(t.Elem())
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"type": "array", "items": items,
			"minItems": t.Len(), "maxItems": t.Len(),
		}, nil

	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("apischema: map %s has a non-string key, which JSON cannot represent", t)
		}
		val, err := g.schemaFor(t.Elem())
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "object", "additionalProperties": val}, nil

	case reflect.String:
		return map[string]any{"type": "string"}, nil

	case reflect.Bool:
		return map[string]any{"type": "boolean"}, nil

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return map[string]any{"type": "integer"}, nil

	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}, nil

	case reflect.Interface:
		// any: no constraint. Setting.Value and configField.Value/FileValue
		// are genuinely polymorphic on the wire (bool, number or string
		// depending on the setting), so there is no narrower schema to give.
		return map[string]any{}, nil

	default:
		return nil, fmt.Errorf("apischema: unsupported kind %s for type %s", t.Kind(), t)
	}
}

// marshalerSchemaFor reports whether t (or *t, since a value-receiver method
// promotes) implements json.Marshaler, or failing that encoding.TextMarshaler
// -- encoding/json's own fallback order, so the same order is checked here.
// When either matches, matched is true and the caller must use the returned
// schema or error -- reflecting over t's fields in that case would describe
// a shape the marshaler never produces. When neither matches, matched is
// false and the caller proceeds with the ordinary kind-based schema.
func marshalerSchemaFor(t reflect.Type) (schema map[string]any, err error, matched bool) {
	if t.Implements(jsonMarshalerType) || reflect.PointerTo(t).Implements(jsonMarshalerType) {
		if s, ok := marshalerSchemas[t]; ok {
			return s, nil, true
		}
		return nil, fmt.Errorf("apischema: %s implements json.Marshaler with no schema registered in marshalerSchemas", t), true
	}
	if t.Implements(textMarshalerType) || reflect.PointerTo(t).Implements(textMarshalerType) {
		return nil, fmt.Errorf("apischema: %s implements encoding.TextMarshaler, which encodes as a quoted string this package does not reflect -- give it a schema by hand or extend marshalerSchemaFor", t), true
	}
	return nil, nil, false
}

// structRef returns a $ref to t's entry in typeDefs, creating the entry (and
// recursing into t's fields) the first time t is reached, and only a $ref on
// every later reach -- both for a type reused across endpoints and for a
// type that reaches itself, like target.Node through Children []*Node.
func (g *generator) structRef(t reflect.Type) (map[string]any, error) {
	if name, ok := g.seen[t]; ok {
		return map[string]any{"$ref": "#/$defs/" + name}, nil
	}
	name := defName(t)
	g.seen[t] = name
	// Reserved before recursing into fields, so a cycle back to t resolves
	// to this name via the g.seen check above rather than recursing forever.
	g.typeDefs[name] = map[string]any{}

	obj, err := g.objectSchema(t)
	if err != nil {
		delete(g.typeDefs, name)
		delete(g.seen, t)
		return nil, err
	}
	g.typeDefs[name] = obj
	return map[string]any{"$ref": "#/$defs/" + name}, nil
}

// defName is the key a struct type gets under $defs: its package's base
// name plus its own name, so two same-named types in different packages
// (none exist among the endpoints today) do not collide.
func defName(t reflect.Type) string {
	if pkg := t.PkgPath(); pkg != "" && t.Name() != "" {
		return path.Base(pkg) + "." + t.Name()
	}
	return t.Name()
}

// objectSchema builds the "object" schema for a struct type: a "properties"
// entry per field (with an embedded struct's own fields flattened in,
// matching how encoding/json promotes them) and a "required" list of every
// field without ",omitempty".
func (g *generator) objectSchema(t reflect.Type) (map[string]any, error) {
	props := map[string]any{}
	var required []string
	if err := g.collectFields(t, props, &required); err != nil {
		return nil, err
	}
	obj := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		obj["required"] = required
	}
	return obj, nil
}

func (g *generator) collectFields(t reflect.Type, props map[string]any, required *[]string) error {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" && !f.Anonymous {
			continue // unexported, not embedded: encoding/json skips it too.
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, omitempty, unsupported := parseJSONTag(tag)
		if unsupported != "" {
			return fmt.Errorf("apischema: %s.%s: %q is not reflected correctly by this package; give the field a schema by hand or extend parseJSONTag/schemaFor to cover it", t.Name(), f.Name, unsupported)
		}

		if f.Anonymous && name == "" {
			// An embedded field with no explicit tag name is flattened: its
			// own fields are promoted to this struct's properties, the way
			// TargetInfo embeds target.Description so form/os/osVersion/
			// apiLevel sit at the top level of the JSON rather than nested
			// under a "Description" key.
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				if err := g.collectFields(ft, props, required); err != nil {
					return err
				}
				continue
			}
		}

		if name == "" {
			name = f.Name
		}
		s, err := g.schemaFor(f.Type)
		if err != nil {
			return fmt.Errorf("%s.%s: %w", t.Name(), f.Name, err)
		}
		if override, ok := fieldOverrides[t][name]; ok {
			mergeFieldOverride(s, override)
		}
		props[name] = s
		if !omitempty {
			*required = append(*required, name)
		}
	}
	return nil
}

// parseJSONTag splits a `json:"..."` tag into its field name (empty when the
// tag itself is empty or names no field), whether it carries omitempty, and
// -- unsupported -- the name of a disallowed option found on it ("string" or
// "omitzero"), empty when neither is present.
//
// Both are refused rather than silently reflected wrong. "omitzero" (Go
// 1.24) omits the field when it is the type's zero value by a rule that
// differs from omitempty's for a struct -- IsZero() if the type defines it,
// reflect-based zero comparison otherwise -- so treating it as omitempty
// would both mis-describe when the field is required and risk being wrong
// about the omission rule itself. ",string" wraps a number, bool or string
// field's JSON value in a quoted string, which changes the field's schema
// type from what its Go type would otherwise reflect to (a string carrying
// digits is not `{"type": "integer"}`); handling it correctly means
// generating a different schema for the same Go kind depending on a tag
// option, which no code path here does today. Both are silent-wrong-schema
// risks if ever reflected past rather than caught, exactly like an
// unregistered json.Marshaler (see marshalerSchemaFor).
func parseJSONTag(tag string) (name string, omitempty bool, unsupported string) {
	if tag == "" {
		return "", false, ""
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	for _, opt := range parts[1:] {
		switch opt {
		case "omitempty":
			omitempty = true
		case "string", "omitzero":
			if unsupported == "" {
				unsupported = opt
			}
		}
	}
	return name, omitempty, unsupported
}
