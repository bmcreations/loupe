package ios

import (
	"github.com/bmcreations/loupe/internal/target"
)

func find(n *target.Node, pred func(*target.Node) bool) *target.Node {
	if n == nil {
		return nil
	}
	if pred(n) {
		return n
	}
	for _, c := range n.Children {
		if got := find(c, pred); got != nil {
			return got
		}
	}
	return nil
}
