package composetree

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Composable is one node of the inspector's composable tree.
type Composable struct {
	Name   string `json:"name"`
	File   string `json:"file,omitempty"`
	Line   int    `json:"line,omitempty"`
	Bounds [4]int `json:"bounds"` // left, top, right, bottom, as target.Node
	// Semantics is set when the composable carries semantics of its own,
	// merged or not: the ones uiautomator can show as a node.
	Semantics bool          `json:"semantics,omitempty"`
	Children  []*Composable `json:"children,omitempty"`
}

// Field numbers from compose_layout_inspection.proto, shipped inside the
// inspector.jar. The module has no protobuf dependency and a reader for
// five messages doesn't justify one.
const (
	respGetComposables = 1 // Response.get_composables_response
	gcStrings          = 1 // GetComposablesResponse.strings
	gcRoots            = 2 // GetComposablesResponse.roots
	strID              = 1 // StringEntry.id
	strStr             = 2 // StringEntry.str
	rootNodes          = 2 // ComposableRoot.nodes
	nodeChildren       = 2
	nodeFilename       = 4
	nodeLine           = 5
	nodeName           = 7
	nodeBounds         = 8
	nodeFlags          = 9
	boundsLayout       = 1 // Bounds.layout
	flagSystemCreated  = 0x1
	flagSemantics      = 0x2 | 0x4 // HAS_MERGED_SEMANTICS | HAS_UNMERGED_SEMANTICS
)

var errTruncated = errors.New("compose reply: truncated")

// field is one decoded protobuf field: a varint or a length-delimited slice.
type field struct {
	num   int
	value uint64
	bytes []byte
}

func fields(b []byte) ([]field, error) {
	var out []field
	for len(b) > 0 {
		key, n := uvarint(b)
		if n <= 0 {
			return nil, errTruncated
		}
		b = b[n:]
		f := field{num: int(key >> 3)}
		switch key & 7 {
		case 0:
			v, n := uvarint(b)
			if n <= 0 {
				return nil, errTruncated
			}
			f.value, b = v, b[n:]
		case 1:
			if len(b) < 8 {
				return nil, errTruncated
			}
			b = b[8:]
		case 2:
			l, n := uvarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				return nil, errTruncated
			}
			f.bytes, b = b[n:n+int(l)], b[n+int(l):]
		case 5:
			if len(b) < 4 {
				return nil, errTruncated
			}
			b = b[4:]
		default:
			return nil, fmt.Errorf("compose reply: wire type %d", key&7)
		}
		out = append(out, f)
	}
	return out, nil
}

func uvarint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		v |= uint64(b[i]&0x7f) << (7 * i)
		if b[i]&0x80 == 0 {
			return v, i + 1
		}
	}
	return 0, 0
}

// Decode reads one GetComposables Response into its root composables.
// System-created nodes are dropped and their children take their place.
func Decode(resp []byte) ([]*Composable, error) {
	top, err := fields(resp)
	if err != nil {
		return nil, err
	}
	var body []byte
	for _, f := range top {
		if f.num == respGetComposables {
			body = f.bytes
		}
	}
	gc, err := fields(body)
	if err != nil {
		return nil, err
	}
	strs := map[uint64]string{}
	for _, f := range gc {
		if f.num != gcStrings {
			continue
		}
		e, err := fields(f.bytes)
		if err != nil {
			return nil, err
		}
		var id uint64
		var s string
		for _, g := range e {
			switch g.num {
			case strID:
				id = g.value
			case strStr:
				s = string(g.bytes)
			}
		}
		strs[id] = s
	}
	var roots []*Composable
	for _, f := range gc {
		if f.num != gcRoots {
			continue
		}
		r, err := fields(f.bytes)
		if err != nil {
			return nil, err
		}
		for _, g := range r {
			if g.num != rootNodes {
				continue
			}
			nodes, err := node(g.bytes, strs)
			if err != nil {
				return nil, err
			}
			roots = append(roots, nodes...)
		}
	}
	return roots, nil
}

// node decodes one ComposableNode. It returns a slice so a system-created
// node can hand back its children in its place.
func node(b []byte, strs map[uint64]string) ([]*Composable, error) {
	fs, err := fields(b)
	if err != nil {
		return nil, err
	}
	c := &Composable{}
	var flags uint64
	for _, f := range fs {
		switch f.num {
		case nodeChildren:
			kids, err := node(f.bytes, strs)
			if err != nil {
				return nil, err
			}
			c.Children = append(c.Children, kids...)
		case nodeFilename:
			c.File = strs[f.value]
		case nodeLine:
			c.Line = int(int32(f.value))
		case nodeName:
			c.Name = strs[f.value]
		case nodeBounds:
			if c.Bounds, err = bounds(f.bytes); err != nil {
				return nil, err
			}
		case nodeFlags:
			flags = f.value
		}
	}
	c.Semantics = flags&flagSemantics != 0
	if flags&flagSystemCreated != 0 {
		return c.Children, nil
	}
	return []*Composable{c}, nil
}

// bounds reads Bounds.layout, a Rect{x, y, w, h}, as left, top, right, bottom.
func bounds(b []byte) ([4]int, error) {
	var out [4]int
	fs, err := fields(b)
	if err != nil {
		return out, err
	}
	for _, f := range fs {
		if f.num != boundsLayout {
			continue
		}
		r, err := fields(f.bytes)
		if err != nil {
			return out, err
		}
		var xywh [4]int
		for _, g := range r {
			if g.num >= 1 && g.num <= 4 {
				// int32 fields: negatives arrive sign-extended to 64 bits.
				xywh[g.num-1] = int(int32(g.value))
			}
		}
		out = [4]int{xywh[0], xywh[1], xywh[0] + xywh[2], xywh[1] + xywh[3]}
	}
	return out, nil
}

// Count is the number of composables in the trees under roots.
func Count(roots []*Composable) int {
	n := 0
	for _, c := range roots {
		n += 1 + Count(c.Children)
	}
	return n
}

// Stable fetches until two consecutive replies hold the same number of
// composables, and returns the last. Right after attach the inspector
// answers before Compose has recorded the whole screen (95 composables
// against 489 a moment later in the spike), so a single fetch isn't enough.
// When ctx ends after at least one good fetch, it returns what it has.
func Stable(ctx context.Context, fetch func(context.Context) ([][]byte, error), every time.Duration) ([]*Composable, error) {
	var last []*Composable
	have := false
	for {
		replies, err := fetch(ctx)
		if err != nil {
			if have && ctx.Err() != nil {
				return last, nil
			}
			return nil, err
		}
		var roots []*Composable
		for _, r := range replies {
			rs, err := Decode(r)
			if err != nil {
				return nil, err
			}
			roots = append(roots, rs...)
		}
		if have && Count(roots) == Count(last) {
			return roots, nil
		}
		last, have = roots, true
		select {
		case <-ctx.Done():
			return last, nil
		case <-time.After(every):
		}
	}
}
