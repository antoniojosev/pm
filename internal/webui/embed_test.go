package webui

import (
	"io/fs"
	"strings"
	"testing"
)

func TestEmbeddedDashboard(t *testing.T) {
	sub, err := FS()
	if err != nil {
		t.Fatalf("FS: %v", err)
	}
	b, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		t.Fatalf("index.html missing from the embedded bundle: %v", err)
	}
	if !strings.Contains(strings.ToLower(string(b)), "<html") {
		t.Fatalf("index.html does not look like a page: %.60s", b)
	}
}
