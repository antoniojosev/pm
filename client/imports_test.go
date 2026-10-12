package client

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// The package promises to need nothing but the standard library: importing it
// must not drag pm's internals or a third-party module into the caller's build.
// Test files are exempt (the contract test runs the real daemon).
func TestOnlyTheStandardLibrary(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v (%d files)", err, len(files))
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			// Standard library paths have no dot in their first element.
			if first, _, _ := strings.Cut(path, "/"); strings.Contains(first, ".") {
				t.Errorf("%s imports %s: the client may only use the standard library", f, path)
			}
		}
	}
}
