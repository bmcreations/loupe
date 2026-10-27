package composetree

import (
	"context"
	"os"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func find(roots []*Composable, name string) *Composable {
	for _, c := range roots {
		if c.Name == name {
			return c
		}
		if f := find(c.Children, name); f != nil {
			return f
		}
	}
	return nil
}

// The fixture is the acmechat chat screen as the spike captured it: one
// window root, 484 composables.
func TestDecodeAcmechatMessages(t *testing.T) {
	roots, err := Decode(fixture(t, "acmechat-messages.pb"))
	if err != nil {
		t.Fatal(err)
	}
	if n := Count(roots); n != 484 {
		t.Fatalf("Count = %d, want 484", n)
	}
	c := roots[0]
	for _, want := range []string{"ProvidePermissionChecker", "Rinku", "App"} {
		if c == nil || c.Name != want {
			t.Fatalf("chain from root: got %+v, want %s", c, want)
		}
		if len(c.Children) > 0 {
			c = c.Children[0]
		} else {
			c = nil
		}
	}
	row := find(roots, "MessageRow")
	if row == nil {
		t.Fatal("no MessageRow")
	}
	if row.File != "MessageList.kt" || row.Line != 661 || row.Bounds != [4]int{53, 1674, 1027, 2125} {
		t.Fatalf("MessageRow = %s:%d %v", row.File, row.Line, row.Bounds)
	}
}

func TestDecodeRejectsTruncatedInput(t *testing.T) {
	b := fixture(t, "acmechat-messages.pb")
	if _, err := Decode(b[:len(b)/2]); err == nil {
		t.Fatal("truncated reply decoded without error")
	}
}

// Right after attach the inspector answers with what Compose has recorded
// so far (95 nodes in the spike); the next request has the whole screen.
func TestStableWaitsForTheTreeToStopGrowing(t *testing.T) {
	cold, full := fixture(t, "acmechat-cold.pb"), fixture(t, "acmechat-messages.pb")
	calls := 0
	fetch := func(context.Context) ([][]byte, error) {
		calls++
		if calls == 1 {
			return [][]byte{cold}, nil
		}
		return [][]byte{full}, nil
	}
	roots, err := Stable(context.Background(), fetch, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if n := Count(roots); n != 484 {
		t.Fatalf("Count = %d, want 484", n)
	}
	if calls < 3 {
		t.Fatalf("fetch called %d times, want at least 3", calls)
	}
}

func TestStableReturnsWhatItHasAtTheDeadline(t *testing.T) {
	cold, full := fixture(t, "acmechat-cold.pb"), fixture(t, "acmechat-messages.pb")
	calls := 0
	fetch := func(context.Context) ([][]byte, error) {
		calls++
		if calls%2 == 1 {
			return [][]byte{cold}, nil
		}
		return [][]byte{full}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	roots, err := Stable(ctx, fetch, time.Millisecond)
	if err != nil {
		t.Fatalf("err = %v, want the last tree", err)
	}
	if len(roots) == 0 {
		t.Fatal("no tree at the deadline")
	}
}
