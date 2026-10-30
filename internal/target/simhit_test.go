package target

import "testing"

// The Flipcash drop that found no Paste in the tree: the composer focused at
// 186..840, the drop at 259,1557, and Paste at 60..271 x 1332..1464.
func TestPasteProbesReachFlipcashPaste(t *testing.T) {
	ui := &UI{Width: 1206, Height: 2622, scale: 3, Root: &Node{
		Bounds: [4]int{0, 0, 1206, 2622},
		Children: []*Node{{
			Class: "TextField", Focused: true, Bounds: [4]int{186, 1509, 840, 1569},
		}},
	}}
	pts := pasteProbes(ui, 259, 1557)
	hit := -1
	for i, p := range pts {
		if p[0] >= 60 && p[0] < 271 && p[1] >= 1332 && p[1] < 1464 {
			hit = i
			break
		}
	}
	if hit < 0 {
		t.Fatalf("no probe lands on Paste; probes %v", pts)
	}
	// Nearest first, so the menu just above the field is reached early.
	if hit > 30 {
		t.Errorf("first probe on Paste is #%d, want it within the first 30", hit)
	}
	for _, p := range pts {
		if p[0] < 0 || p[0] >= ui.Width || p[1] < 0 || p[1] >= ui.Height {
			t.Errorf("probe %v is off the screen", p)
		}
	}
}

func TestPasteProbesWithoutFocusUseTheDropColumn(t *testing.T) {
	ui := &UI{Width: 1206, Height: 2622, scale: 3, Root: &Node{Bounds: [4]int{0, 0, 1206, 2622}}}
	for _, p := range pasteProbes(ui, 600, 30) {
		if p[0] != 600 {
			t.Fatalf("probe %v is off the drop column", p)
		}
		if p[1] < 0 {
			t.Fatalf("probe %v is above the screen", p)
		}
	}
}
