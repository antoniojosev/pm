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

func TestDashboardFormKeepsLimitsAndStoredFields(t *testing.T) {
	sub, err := FS()
	if err != nil {
		t.Fatalf("FS: %v", err)
	}
	b, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	// Saving POSTs the whole record, which replaces the stored one: the form
	// must carry the limits and start from the stored record so an edit does
	// not drop them (or port_strategy, kind...).
	for _, want := range []string{`id="f_mem"`, `id="f_cpu"`, "limits:{memory:", "Object.assign({kind:'native'},editingProject||{},form)",
		// Saving from the dashboard confirms a scan draft, as pm edit does.
		"draft:false"} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard lacks %q", want)
		}
	}
}
