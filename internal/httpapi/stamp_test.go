package httpapi

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/bmcreations/loupe/web"
)

// relativeImport matches a relative './foo.js' module specifier after
// either "import" or "from", covering every form ES modules allow: a static
// import with a from-clause (`import x from './a.js'`), a side-effect
// import with none (`import './a.js'`), a dynamic import (`import('./a.js')`),
// and either quote style, with or without the space `from'./a.js'` omits
// (review round 5, finding 4 -- the single-quoted, spaced, from-only form
// this used to require missed all four).
var relativeImport = regexp.MustCompile(`\b(?:import|from)\b\s*\(?\s*['"]\./([A-Za-z0-9_.-]+\.js)['"]`)

// TestRelativeImportMatchesEveryForm pins relativeImport against the four
// import shapes review round 5's finding 4 found it missing: it used to
// require a single-quoted, space-then-quote `from './x.js'`, so a
// double-quoted specifier, a side-effect import with no from-clause, a
// dynamic import() call, and a from-clause with no space before the quote
// all passed the old regex by without producing a match -- meaning an
// unlisted module reached that way never failed TestEveryClientModuleIsStamped
// at all.
func TestRelativeImportMatchesEveryForm(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"double-quoted specifier", `import { x } from "./unlisted.js";`},
		{"side-effect import, no from-clause", `import './unlisted.js';`},
		{"dynamic import()", `const m = await import('./unlisted.js');`},
		{"from-clause with no space before the quote", `import{x}from'./unlisted.js';`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := relativeImport.FindStringSubmatch(c.src)
			if m == nil {
				t.Fatalf("relativeImport did not match %q, so a module reachable this way would silently skip TestEveryClientModuleIsStamped", c.src)
			}
			if m[1] != "unlisted.js" {
				t.Fatalf("relativeImport captured %q, want %q", m[1], "unlisted.js")
			}
		})
	}
}

// importsOf reads a web/ source file from disk and returns the modules it
// imports via a relative './foo.js' specifier.
func importsOf(t *testing.T, name string) []string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("../../web", name))
	if err != nil {
		t.Fatalf("read web/%s: %v", name, err)
	}
	var out []string
	for _, m := range relativeImport.FindAllStringSubmatch(string(src), -1) {
		out = append(out, m[1])
	}
	return out
}

// This checks one half of trap 7: that clientModules lists every module
// reachable from app.js by import, at any depth, not that stampIndex
// actually rewrites the specifier in the file that imports it. It does not
// -- see the stamping-gap comment on clientModules in server.go -- so a
// module missing from this list is stronger evidence of trouble than a
// module present in it is evidence of safety.
//
// Reading only app.js's own imports missed this once already: toast.js
// stopped being imported directly by app.js when corners.js, appcard.js,
// devicesettings.js and inspect.js each started importing it instead
// (review round 3's shared-toast fix), and the single-file scan had nothing
// left to catch its absence from clientModules -- a mutation that dropped
// toast.js from the list passed clean. So this walks the import graph
// transitively from app.js across every web/*.js file, not just the entry
// point, skipping *.test.mjs (which import modules under test but are never
// shipped -- see TestTestFilesAreNotEmbedded).
//
// It reads from disk rather than the embedded FS so it fails on the source
// the author just edited.
func TestEveryClientModuleIsStamped(t *testing.T) {
	stamped := map[string]bool{}
	for _, m := range clientModules {
		stamped[m] = true
	}

	seen := map[string]bool{"app.js": true}
	queue := []string{"app.js"}
	found := 0
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		for _, imp := range importsOf(t, name) {
			found++
			if !seen[imp] {
				seen[imp] = true
				queue = append(queue, imp)
			}
			if imp == name {
				continue // a module does not need to list itself
			}
			if !stamped[imp] {
				t.Errorf("%s imports %s but clientModules does not list it, "+
					"so it is served unversioned and a stale copy can outlive a build", name, imp)
			}
		}
	}
	if found == 0 {
		t.Fatal("found no relative imports reachable from app.js, so this test is checking nothing")
	}
}

// Stamping a module is not the same as shipping it. The URL in the page can be
// correct and versioned while the binary has never embedded the file, which is
// a 404 for an ES module import: the page loads, app.js stops part way through
// evaluation, and the only symptom is a client that never reaches its target
// list.
func TestEveryClientModuleIsEmbedded(t *testing.T) {
	for _, name := range append([]string{"app.js", "index.html", "style.css"}, clientModules...) {
		if _, err := fs.Stat(web.FS, name); err != nil {
			t.Errorf("%s is not embedded, so the server answers 404 for it: %v", name, err)
		}
	}
}

// The embed patterns are globs, so a test harness dropped next to the client
// would otherwise be shipped to every browser.
func TestTestFilesAreNotEmbedded(t *testing.T) {
	entries, err := fs.ReadDir(web.FS, ".")
	if err != nil {
		t.Fatalf("read embedded FS: %v", err)
	}
	for _, e := range entries {
		if regexp.MustCompile(`\.test\.[a-z]+$`).MatchString(e.Name()) {
			t.Errorf("%s is embedded in the binary", e.Name())
		}
	}
}
