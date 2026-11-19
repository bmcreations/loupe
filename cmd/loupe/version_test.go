package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// main declares its own version for the scrcpy server, so passing the bare
// name to SetVersion reports scrcpy's version on /api/version. Handset
// compares that with the loupe it bundles and restarts loupe on every launch
// when they differ.
func TestAPIReportsLoupesVersionNotScrcpys(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "SetVersion" || len(call.Args) != 1 {
			return true
		}
		found = true
		arg, _ := call.Args[0].(*ast.CallExpr)
		if arg == nil {
			t.Errorf("SetVersion is passed %T, want loupeVersion()", call.Args[0])
		} else if id, _ := arg.Fun.(*ast.Ident); id == nil || id.Name != "loupeVersion" {
			t.Errorf("SetVersion is passed a call to %T, want loupeVersion()", arg.Fun)
		}
		return true
	})
	if !found {
		t.Fatal("no SetVersion call in main.go")
	}
}
