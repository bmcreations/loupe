// Command apischema regenerates docs/api.schema.json from the server's own
// response types.
//
//	go run ./cmd/apischema
//
// It writes relative to the current working directory, so run it from the
// repository root. TestSchemaFileMatchesGenerated in
// internal/apischema/schema_test.go is what actually enforces the file is
// up to date; this command is how a diff gets fixed once that test fails.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/bmcreations/loupe/internal/apischema"
)

func main() {
	out := flag.String("out", "docs/api.schema.json", "path to write the generated schema to")
	flag.Parse()

	b, err := apischema.Generate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "apischema:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "apischema:", err)
		os.Exit(1)
	}
}
