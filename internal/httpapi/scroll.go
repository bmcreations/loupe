package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// A scroll is swipes the server judges for itself: after each one it reads
// the screen again, and a list whose contents did not move is at its edge.
// Without it an agent swipes, takes a screenshot, compares, and swipes once
// more to be sure, a model turn each, at every top and bottom of a list.

const (
	// maxScrollSwipes bounds one scroll. A list longer than this many pages
	// is better searched with find, or by a second call.
	maxScrollSwipes = 30
	// defaultUntilSwipes is a scroll's limit when it runs until something
	// (the edge, or a node) and names no max of its own.
	defaultUntilSwipes = 20
	// scrollFlingMs is the stroke of a simulator scroll that runs to the
	// edge with nothing to look for: a fling, the way a person heads for the
	// top, covers several pages a stroke there. Android keeps defaultSwipeMs:
	// a 120ms `input swipe` moved its Settings list no further than a 300ms
	// one, and short swipes are where Android drops moves and lands a tap.
	// Looking for a node uses defaultSwipeMs everywhere, so a fling cannot
	// carry it past the screen unseen.
	scrollFlingMs = 120
	// scrollSettleDumps bounds the dumps after a stroke, taken until two in
	// a row agree. A simulator's list is still coasting, or bouncing at its
	// edge, when the first dump returns, for up to a second on iOS 27's
	// Settings; a dump mid-bounce differs from the one before the stroke
	// although nothing scrolled. uiautomator waits for the window to go
	// idle, so an Android dump is taken once.
	scrollSettleDumps = 15
	scrollSettleGap   = 100 * time.Millisecond
	// scrollStroke is the share of the list a stroke drags across: most of
	// it, so pages overlap by a row or two and nothing is skipped.
	scrollStroke = 0.6
)

// scrollDirections are the directions a scroll heads in: toward the content
// it reveals, as a person says it. "down" shows what is below, so the finger
// moves up.
var scrollDirections = []string{"up", "down", "left", "right"}

// handleScroll runs a scroll action. Its swipes go through handleInput one
// at a time, each with a move first, so every stroke is arbitrated, logged
// and shown to viewers like a swipe sent alone, and a person who takes the
// device back stops it at the next stroke with that stroke's 409.
func (s *Server) handleScroll(w http.ResponseWriter, r *http.Request, t target.Target, req InputRequest) {
	dir := req.Direction
	if !slices.Contains(scrollDirections, dir) {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: fmt.Sprintf("scroll direction %q; want %s", dir, strings.Join(scrollDirections, ", ")),
		})
		return
	}
	if req.Until != "" && req.Until != "edge" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("scroll until %q; want edge", req.Until)})
		return
	}
	if req.Max < 0 || req.Max > maxScrollSwipes {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: fmt.Sprintf("scroll max %d out of range [0,%d]", req.Max, maxScrollSwipes),
		})
		return
	}
	if req.Find != nil && req.Find.Empty() {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "find needs an id, text or desc"})
		return
	}
	insp, ok := t.(target.Inspector)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, ErrorResponse{
			Error: fmt.Sprintf("target %q has no view hierarchy to scroll by", t.ID()),
		})
		return
	}
	hasPoint := req.X != 0 || req.Y != 0

	limit := req.Max
	if limit == 0 {
		limit = 1
		if req.Until == "edge" || req.Find != nil {
			limit = defaultUntilSwipes
		}
	}
	// uiautomator dumps once the window is idle; a simulator's tree is read
	// as it stands, mid-coast.
	android := t.Kind() == target.KindDevice || t.Kind() == target.KindEmulator
	settles := !android
	ms := req.Ms
	if ms <= 0 {
		ms = defaultSwipeMs
		if req.Until == "edge" && req.Find == nil && !android {
			ms = scrollFlingMs
		}
	}

	dump := func() (*target.UI, error) {
		ctx, cancel := context.WithTimeout(r.Context(), agentTimeout)
		defer cancel()
		return insp.Inspect(s.peerReadCtx(ctx, r, t))
	}
	fail := func(err error) {
		s.traceAgent(r, t.ID(), "scroll", err)
		if writeOwnerError(w, err) {
			return
		}
		status := http.StatusBadGateway
		if errors.Is(err, target.ErrUIAutomationBusy) {
			status = http.StatusConflict
		}
		writeJSON(w, status, ErrorResponse{Error: err.Error()})
	}

	ui, err := dump()
	if err != nil {
		fail(err)
		return
	}
	region := scrollRegion(ui, req.X, req.Y, hasPoint)
	found := func(ui *target.UI) *target.Node {
		if req.Find == nil {
			return nil
		}
		if n := target.FindNodes(ui.Root, *req.Find); len(n) > 0 {
			return n[0].Leaf()
		}
		return nil
	}
	resp := InputResponse{OK: true, Region: &region}
	if n := found(ui); n != nil {
		resp.Matched = n
		s.traceAgent(r, t.ID(), "scroll", nil)
		writeJSON(w, http.StatusOK, resp)
		return
	}

	prev := scrollFingerprint(ui, region)
	for resp.Swipes < limit && !resp.AtEdge && resp.Matched == nil {
		x1, y1, x2, y2 := scrollStrokeFor(region, ui.Width, ui.Height, dir, req.X, req.Y, hasPoint)
		s.runStep(r, []byte(fmt.Sprintf(`{"action":"move","x":%d,"y":%d}`, x1, y1)))
		status, out := s.runStep(r, []byte(fmt.Sprintf(
			`{"action":"swipe","x":%d,"y":%d,"x2":%d,"y2":%d,"ms":%d}`, x1, y1, x2, y2, ms)))
		if status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(out)
			return
		}
		resp.Swipes++

		ui, err = dump()
		if err != nil {
			fail(err)
			return
		}
		// Settled first, then compared: a list at its edge on a simulator
		// bounces, and a dump mid-bounce differs from the one before the
		// stroke although nothing scrolled.
		fp := scrollFingerprint(ui, region)
		for i := 1; settles && i < scrollSettleDumps; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(scrollSettleGap):
			}
			next, err := dump()
			if err != nil {
				fail(err)
				return
			}
			nfp := scrollFingerprint(next, region)
			ui = next
			if nfp == fp {
				break
			}
			fp = nfp
		}
		if fp == prev {
			resp.AtEdge = true
			break
		}
		prev = fp
		resp.Matched = found(ui)
	}

	s.traceInputWith(r, t.ID(), "scroll", inputCaller{}, nil, map[string]any{
		"direction": dir, "swipes": resp.Swipes, "atEdge": resp.AtEdge, "found": resp.Matched != nil,
	})
	if req.Find != nil && resp.Matched == nil {
		where := fmt.Sprintf("after %s", plural(resp.Swipes, "swipe"))
		if resp.AtEdge {
			where = fmt.Sprintf("at the %s edge after %s", edgeName(dir), plural(resp.Swipes, "swipe"))
		}
		writeJSON(w, http.StatusNotFound, ErrorResponse{
			Error: fmt.Sprintf("%s: %s %s", target.ErrNoMatch, req.Find, where),
			Code:  ErrCodeNoMatch,
		})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func edgeName(dir string) string {
	switch dir {
	case "up":
		return "top"
	case "down":
		return "bottom"
	}
	return dir
}

// scrollRegion picks the rectangle a scroll drags in and watches: the
// innermost scrollable node under the given point, or with no point the
// largest scrollable node on screen. A screen that marks nothing scrollable
// (many iOS views do not) gets the screen less its top and bottom tenth,
// which leaves out the status bar and its clock.
func scrollRegion(ui *target.UI, x, y int, hasPoint bool) [4]int {
	var best *target.Node
	area := func(n *target.Node) int { return (n.Bounds[2] - n.Bounds[0]) * (n.Bounds[3] - n.Bounds[1]) }
	var walk func(n *target.Node)
	walk = func(n *target.Node) {
		if n == nil {
			return
		}
		b := n.Bounds
		if n.Scrollable && !n.Occluded && b[2] > b[0] && b[3] > b[1] {
			switch {
			case hasPoint:
				if x >= b[0] && x < b[2] && y >= b[1] && y < b[3] && (best == nil || area(n) <= area(best)) {
					best = n
				}
			case best == nil || area(n) > area(best):
				best = n
			}
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(ui.Root)
	if best != nil {
		b := best.Bounds
		return [4]int{max(b[0], 0), max(b[1], 0), min(b[2], ui.Width), min(b[3], ui.Height)}
	}
	return [4]int{0, ui.Height / 10, ui.Width, ui.Height - ui.Height/10}
}

// scrollStrokeFor is one stroke inside region heading in dir. It runs across
// scrollStroke of the region on the region's centre line, or through the
// given point. It keeps clear of the screen's edges, where a drag would open
// the notification shade, go back, or go home instead of scrolling.
func scrollStrokeFor(region [4]int, w, h int, dir string, px, py int, hasPoint bool) (x1, y1, x2, y2 int) {
	left, top, right, bottom := region[0], region[1], region[2], region[3]
	// The safe band, intersected with the region; a region wholly outside
	// it (a strip along an edge) is used as it is.
	if l, r := max(left, w*8/100), min(right, w-w*8/100); r > l {
		left, right = l, r
	}
	if t, b := max(top, h*8/100), min(bottom, h-h*8/100); b > t {
		top, bottom = t, b
	}
	cx, cy := (left+right)/2, (top+bottom)/2
	if hasPoint {
		cx, cy = clampInt(px, left, right-1), clampInt(py, top, bottom-1)
	}
	half := func(span int) int { return int(float64(span) * scrollStroke / 2) }
	switch dir {
	case "down":
		d := half(bottom - top)
		return cx, (top+bottom)/2 + d, cx, (top+bottom)/2 - d
	case "up":
		d := half(bottom - top)
		return cx, (top+bottom)/2 - d, cx, (top+bottom)/2 + d
	case "right":
		d := half(right - left)
		return (left+right)/2 + d, cy, (left+right)/2 - d, cy
	default: // left
		d := half(right - left)
		return (left+right)/2 - d, cy, (left+right)/2 + d, cy
	}
}

func clampInt(v, lo, hi int) int {
	return max(lo, min(v, hi))
}

// scrollFingerprint is what region shows: every node whose centre is inside
// it, with its label and bounds, in tree order. A stroke that leaves it
// unchanged moved nothing. Rows that are identical in every label and land
// exactly one row apart would read as unchanged too; a real list rarely has
// them.
func scrollFingerprint(ui *target.UI, region [4]int) string {
	var b strings.Builder
	var walk func(n *target.Node)
	walk = func(n *target.Node) {
		if n == nil {
			return
		}
		c := n.Center
		if c[0] >= region[0] && c[0] < region[2] && c[1] >= region[1] && c[1] < region[3] {
			fmt.Fprintf(&b, "%s|%s|%s|%s|%s|%v\n", n.Class, n.ID, n.Text, n.Desc, n.Value, n.Bounds)
		}
		for _, ch := range n.Children {
			walk(ch)
		}
	}
	walk(ui.Root)
	return b.String()
}
