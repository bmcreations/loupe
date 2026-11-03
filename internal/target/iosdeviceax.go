package target

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"howett.net/plist"
)

// Decoder for the accessibility dump a physical iPhone's remoteAXService
// sends over DTX (processDataFromRemoteDevice:). The payload is an
// NSKeyedArchiver archive whose root is an NSData wrapping a second archive;
// that inner root is an AXPTranslatorResponse carrying one type-5 entry
// (per-element attribute dict) per node.

const (
	axReqAttributes = 5

	axAttrTraits   = 78 // [role word, traits...]
	axAttrLabel    = 33
	axAttrFrame    = 48
	axAttrChildren = 8
)

var errPhoneAXEmpty = errors.New("phone accessibility dump has no element nodes")

// iOS role word -> the macOS AX constant axToNode keys clickability off.
// grow as more app screens are captured
var phoneAXRoles = map[string]string{
	"Button":       "AXButton",
	"Static Text":  "AXStaticText",
	"Image":        "AXImage",
	"Header":       "AXHeading",
	"Link":         "AXLink",
	"Text Field":   "AXTextField",
	"Search Field": "AXTextField",
	"Text Area":    "AXTextArea",
	"Cell":         "AXCell",
	"Tab":          "AXTab",
	"Adjustable":   "AXSlider",
}

func phoneAXRole(word string) string {
	if word == "" {
		return ""
	}
	if r, ok := phoneAXRoles[word]; ok {
		return r
	}
	return "AX" + strings.ReplaceAll(word, " ", "")
}

type phoneNode struct {
	role     string
	label    string
	frame    []float64
	childIDs []uint64
	hasRole  bool
	hasLabel bool
	hasFrame bool
	hasKids  bool
}

type keyedArchive struct {
	objects []interface{}
}

func openArchive(raw []byte) (*keyedArchive, interface{}, error) {
	var top map[string]interface{}
	if _, err := plist.Unmarshal(raw, &top); err != nil {
		return nil, nil, fmt.Errorf("parse archive plist: %w", err)
	}
	objs, _ := top["$objects"].([]interface{})
	t, _ := top["$top"].(map[string]interface{})
	if objs == nil || t == nil {
		return nil, nil, errors.New("not a keyed archive")
	}
	a := &keyedArchive{objects: objs}
	return a, a.deref(t["root"]), nil
}

// deref follows a CF$UID into $objects; other values pass through.
func (a *keyedArchive) deref(v interface{}) interface{} {
	if u, ok := v.(plist.UID); ok {
		if int(u) < len(a.objects) {
			return a.objects[u]
		}
		return nil
	}
	return v
}

func (a *keyedArchive) className(v interface{}) string {
	m, _ := v.(map[string]interface{})
	if m == nil {
		return ""
	}
	c, _ := a.deref(m["$class"]).(map[string]interface{})
	s, _ := c["$classname"].(string)
	return s
}

func (a *keyedArchive) field(v interface{}, key string) interface{} {
	m, _ := a.deref(v).(map[string]interface{})
	if m == nil {
		return nil
	}
	return a.deref(m[key])
}

// list returns the dereferenced NS.objects of an NSArray.
func (a *keyedArchive) list(v interface{}) []interface{} {
	m, _ := a.deref(v).(map[string]interface{})
	raw, _ := m["NS.objects"].([]interface{})
	out := make([]interface{}, len(raw))
	for i, e := range raw {
		out[i] = a.deref(e)
	}
	return out
}

// dict returns the dereferenced key/value pairs of an NSDictionary.
func (a *keyedArchive) dict(v interface{}) (keys, vals []interface{}) {
	m, _ := a.deref(v).(map[string]interface{})
	ks, _ := m["NS.keys"].([]interface{})
	vs, _ := m["NS.objects"].([]interface{})
	for i := range ks {
		if i >= len(vs) {
			break
		}
		keys = append(keys, a.deref(ks[i]))
		vals = append(vals, a.deref(vs[i]))
	}
	return keys, vals
}

func asUint(v interface{}) (uint64, bool) {
	switch n := v.(type) {
	case uint64:
		return n, true
	case int64:
		return uint64(n), true
	case int:
		return uint64(n), true
	case uint8, uint16, uint32:
		return asUint(int64(toI(n)))
	}
	return 0, false
}

func toI(v interface{}) int64 {
	switch n := v.(type) {
	case uint8:
		return int64(n)
	case uint16:
		return int64(n)
	case uint32:
		return int64(n)
	}
	return 0
}

func (a *keyedArchive) objectID(v interface{}) (uint64, bool) {
	return asUint(a.field(v, "objectID"))
}

// parseRect reads "{{x, y}, {w, h}}".
func parseRect(s string) ([]float64, bool) {
	r := strings.NewReplacer("{", "", "}", "", " ", "")
	parts := strings.Split(r.Replace(s), ",")
	if len(parts) != 4 {
		return nil, false
	}
	out := make([]float64, 4)
	for i, p := range parts {
		f, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return nil, false
		}
		out[i] = f
	}
	return out, true
}

// decodePhoneAXNodes unwraps one dump into its type-5 nodes keyed by objectID.
func decodePhoneAXNodes(raw []byte) (map[uint64]*phoneNode, error) {
	outer, root, err := openArchive(raw)
	if err != nil {
		return nil, err
	}
	data, _ := outer.field(root, "NS.data").([]byte)
	if data == nil {
		return nil, errors.New("outer archive root is not NSData")
	}
	a, resp, err := openArchive(data)
	if err != nil {
		return nil, fmt.Errorf("inner archive: %w", err)
	}

	nodes := map[uint64]*phoneNode{}
	keys, vals := a.dict(a.field(resp, "resultData"))
	for i, k := range keys {
		if s, _ := k.(string); s != "treeDump" {
			continue
		}
		for _, entry := range a.list(vals[i]) {
			if t, _ := asUint(a.field(entry, "associatedRequestType")); t != axReqAttributes {
				continue
			}
			id, ok := a.objectID(a.field(entry, "associatedTranslationObject"))
			if !ok {
				continue
			}
			nodes[id] = a.attrNode(a.field(entry, "resultData"))
		}
	}
	if len(nodes) == 0 {
		return nil, errPhoneAXEmpty
	}
	return nodes, nil
}

func (a *keyedArchive) attrNode(dict interface{}) *phoneNode {
	n := &phoneNode{}
	keys, vals := a.dict(dict)
	for i, k := range keys {
		code, ok := asUint(k)
		if !ok {
			continue
		}
		switch code {
		case axAttrTraits:
			if l := a.list(vals[i]); len(l) > 0 {
				if s, ok := l[0].(string); ok {
					n.role, n.hasRole = phoneAXRole(s), true
				}
			}
		case axAttrLabel:
			// A plain string, or an NSAttributedString wrapping one in NS.string.
			s, ok := vals[i].(string)
			if !ok {
				s, ok = a.field(vals[i], "NS.string").(string)
			}
			if ok {
				n.label, n.hasLabel = s, true
			}
		case axAttrFrame:
			if s, ok := a.field(vals[i], "NS.rectval").(string); ok {
				if f, ok := parseRect(s); ok {
					n.frame, n.hasFrame = f, true
				}
			}
		case axAttrChildren:
			n.hasKids = true
			for _, c := range a.list(vals[i]) {
				if id, ok := a.objectID(c); ok {
					n.childIDs = append(n.childIDs, id)
				}
			}
		}
	}
	return n
}

// mergePhoneNode fills dst from src; src's present attributes win.
func mergePhoneNode(dst, src *phoneNode) {
	if src.hasRole {
		dst.role, dst.hasRole = src.role, true
	}
	if src.hasLabel {
		dst.label, dst.hasLabel = src.label, true
	}
	if src.hasFrame {
		dst.frame, dst.hasFrame = src.frame, true
	}
	if src.hasKids {
		dst.childIDs, dst.hasKids = src.childIDs, true
	}
}

// decodePhoneAX decodes a single dump into an axNode tree.
func decodePhoneAX(raw []byte) (*axNode, error) {
	return decodePhoneAXMerged(raw)
}

// decodePhoneAXMerged unions several dumps by objectID; later dumps win.
func decodePhoneAXMerged(raws ...[]byte) (*axNode, error) {
	all := map[uint64]*phoneNode{}
	for _, raw := range raws {
		nodes, err := decodePhoneAXNodes(raw)
		if err != nil {
			return nil, err
		}
		for id, n := range nodes {
			if cur, ok := all[id]; ok {
				mergePhoneNode(cur, n)
			} else {
				all[id] = n
			}
		}
	}
	if len(all) == 0 {
		return nil, errPhoneAXEmpty
	}

	referenced := map[uint64]bool{}
	for _, n := range all {
		for _, c := range n.childIDs {
			referenced[c] = true
		}
	}
	var rootID uint64
	found := false
	for id := range all {
		if !referenced[id] {
			// Several candidates would be ambiguous; take the largest frame.
			if !found || phoneArea(all[id]) > phoneArea(all[rootID]) {
				rootID, found = id, true
			}
		}
	}
	if !found {
		return nil, errors.New("phone accessibility dump has no root (cycle)")
	}

	seen := map[uint64]bool{}
	var build func(id uint64) *axNode
	build = func(id uint64) *axNode {
		n := all[id]
		seen[id] = true
		out := &axNode{Role: n.role, Label: n.label, Frame: n.frame}
		for _, c := range n.childIDs {
			if _, ok := all[c]; !ok || seen[c] {
				continue
			}
			out.Children = append(out.Children, build(c))
		}
		return out
	}
	return build(rootID), nil
}

func phoneArea(n *phoneNode) float64 {
	if len(n.frame) != 4 {
		return 0
	}
	return n.frame[2] * n.frame[3]
}

// phoneAXToUI mirrors parseAXTree's scale computation for a decoded tree.
func phoneAXToUI(root *axNode, width, height int) (*UI, error) {
	if root == nil || len(root.Frame) != 4 || root.Frame[2] <= 0 {
		role := ""
		if root != nil {
			role = root.Role
		}
		return nil, fmt.Errorf("%w: %q", errAXNoFrame, role)
	}
	scale := math.Round(float64(width) / root.Frame[2])
	if scale < 1 {
		scale = 1
	}
	return &UI{Width: width, Height: height, Root: axToNode(root, scale), scale: scale}, nil
}
