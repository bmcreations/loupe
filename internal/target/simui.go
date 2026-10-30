package target

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

// axNode is one element as simcap's `ui` command writes it (simcap's `ax.swift`):
// AXPTranslator's macOS rendering of the iOS accessibility tree, with the
// frame in points.
type axNode struct {
	Role     string    `json:"role"`
	Subrole  string    `json:"subrole"`
	Label    string    `json:"label"`
	Value    string    `json:"value"`
	ID       string    `json:"id"`
	Frame    []float64 `json:"frame"`
	Focused  bool      `json:"focused"`
	Disabled bool      `json:"disabled"`
	Occluded bool      `json:"occluded"`
	Children []*axNode `json:"children"`
}

// Roles an agent would tap. iOS marks tappability with the element's traits,
// and AXPTranslator surfaces those only as the role it picks, so the role is
// the whole signal.
var axClickable = map[string]bool{
	"AXButton": true, "AXLink": true, "AXCell": true, "AXTextField": true,
	"AXTextArea": true, "AXCheckBox": true, "AXRadioButton": true,
	"AXPopUpButton": true, "AXMenuItem": true, "AXTab": true, "AXSlider": true,
	"AXIncrementor": true, "AXSegmentedControl": true,
}

// Inspect implements Inspector through simcap's accessibility dump. It covers
// the frontmost application only, which is what is on screen; SpringBoard is
// the frontmost application on the Home Screen.
//
// Measured on an iPhone 17 simulator, iOS 27.0: about 75ms for the Settings
// root screen, and about 480ms for the first dump from a fresh input process.
func (s *Simulator) Inspect(ctx context.Context) (*UI, error) {
	if err := s.requireBooted(); err != nil {
		return nil, err
	}
	w, h, err := s.ScreenSize(ctx)
	if err != nil {
		return nil, err
	}
	ui, err := s.inspect(ctx, w, h)
	if !errors.Is(err, errAXNoFrame) {
		return ui, err
	}
	// An empty root on a simulator whose apps have accessibility turned off
	// reads again once it is on.
	if changed, aerr := s.enableAppAccessibility(ctx); aerr != nil || !changed {
		return ui, err
	}
	return s.inspect(ctx, w, h)
}

func (s *Simulator) inspect(ctx context.Context, w, h int) (*UI, error) {
	out, err := s.input().do(ctx, 0, "ui")
	if err != nil {
		return nil, fmt.Errorf("accessibility dump: %w", err)
	}
	return parseAXTree([]byte(out), w, h)
}

// parseAXTree turns simcap's tree into loupe's, in device pixels.
//
// The scale comes from the root: the application element spans the screen, so
// pixels over points is the display scale, 3 on an iPhone 17 (1206 over 402).
// It is rounded because every iOS display scale is a whole number and the
// frame's width is itself a rounded float.
func parseAXTree(raw []byte, width, height int) (*UI, error) {
	var root axNode
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("parse accessibility tree: %w", err)
	}
	if len(root.Frame) != 4 || root.Frame[2] <= 0 {
		return nil, fmt.Errorf("%w: %q", errAXNoFrame, root.Role)
	}
	scale := math.Round(float64(width) / root.Frame[2])
	if scale < 1 {
		scale = 1
	}
	return &UI{Width: width, Height: height, Root: axToNode(&root, scale), scale: scale}, nil
}

func axToNode(a *axNode, scale float64) *Node {
	n := &Node{
		Class: strings.TrimPrefix(a.Role, "AX"),
		ID:    a.ID,
		// The label is what VoiceOver reads and, for a button or a cell, the
		// text drawn on it, so it is the closest thing to Android's text.
		Text: a.Label,
		// A text field's contents, or its placeholder when empty: the Settings
		// search field reads label "" and value "Search".
		Value:      a.Value,
		Clickable:  axClickable[a.Role],
		Scrollable: a.Role == "AXScrollArea",
		Checkable:  a.Role == "AXCheckBox" || a.Subrole == "AXSwitch",
		Focused:    a.Focused,
		Password:   a.Subrole == "AXSecureTextField",
		Disabled:   a.Disabled,
		Occluded:   a.Occluded,
	}
	if n.Checkable {
		// A switch reports its state as its value, "1" or "0".
		n.Checked = a.Value == "1"
	}
	if len(a.Frame) == 4 {
		x1 := int(math.Round(a.Frame[0] * scale))
		y1 := int(math.Round(a.Frame[1] * scale))
		x2 := int(math.Round((a.Frame[0] + a.Frame[2]) * scale))
		y2 := int(math.Round((a.Frame[1] + a.Frame[3]) * scale))
		n.Bounds = [4]int{x1, y1, x2, y2}
		n.Center = [2]int{(x1 + x2) / 2, (y1 + y2) / 2}
	}
	for _, c := range a.Children {
		n.Children = append(n.Children, axToNode(c, scale))
	}
	return n
}
