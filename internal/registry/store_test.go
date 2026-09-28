package registry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/antoniojosev/pm/internal/config"
)

// tempPaths returns config.Paths rooted in a throwaway directory so tests never
// touch the real ~/.pm.
func tempPaths(t *testing.T) config.Paths {
	t.Helper()
	root := t.TempDir()
	return config.Paths{
		Root:      root,
		Registry:  filepath.Join(root, "registry.json"),
		LogsDir:   filepath.Join(root, "logs"),
		RunDir:    filepath.Join(root, "run"),
		Caddyfile: filepath.Join(root, "Caddyfile"),
	}
}

func TestOpenMissingFileStartsEmpty(t *testing.T) {
	s, err := Open(tempPaths(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := s.List(); len(got) != 0 {
		t.Fatalf("expected empty registry, got %d projects", len(got))
	}
	if s.state.Version != 1 {
		t.Fatalf("expected version 1, got %d", s.state.Version)
	}
}

func TestOpenCorruptFile(t *testing.T) {
	paths := tempPaths(t)
	if err := os.WriteFile(paths.Registry, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(paths); err == nil {
		t.Fatal("expected error for corrupt registry")
	}
}

func TestOpenEmptyFileDefaultsVersion(t *testing.T) {
	paths := tempPaths(t)
	if err := os.WriteFile(paths.Registry, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(paths)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.state.Version != 1 {
		t.Fatalf("version = %d, want 1", s.state.Version)
	}
}

func TestUpsertPersistsAndReplaces(t *testing.T) {
	paths := tempPaths(t)
	s, _ := Open(paths)

	if err := s.Upsert(Project{Name: "web", Dir: "/srv/web", PreferPort: 3000}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := s.Upsert(Project{Name: "api", Dir: "/srv/api"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// same name, different case → replaces, does not duplicate
	if err := s.Upsert(Project{Name: "WEB", Dir: "/srv/web2", PreferPort: 3001}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	list := s.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 projects, got %d: %+v", len(list), list)
	}
	web, ok := s.Get("web")
	if !ok || web.PreferPort != 3001 || web.Dir != "/srv/web2" || web.Name != "WEB" {
		t.Fatalf("upsert did not replace the existing entry: %+v", web)
	}

	// re-open from disk: state must have been persisted atomically
	if _, err := os.Stat(paths.Registry + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind after save")
	}
	s2, err := Open(paths)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	if got := s2.List(); len(got) != 2 {
		t.Fatalf("persisted registry has %d projects, want 2", len(got))
	}

	var doc State
	b, _ := os.ReadFile(paths.Registry)
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("registry on disk is not valid JSON: %v", err)
	}
	if doc.Version != 1 {
		t.Fatalf("on-disk version = %d, want 1", doc.Version)
	}
}

func TestGetIsCaseInsensitive(t *testing.T) {
	s, _ := Open(tempPaths(t))
	_ = s.Upsert(Project{Name: "MyApp"})

	if _, ok := s.Get("myapp"); !ok {
		t.Fatal("Get should match case-insensitively")
	}
	if _, ok := s.Get("other"); ok {
		t.Fatal("Get returned a project that does not exist")
	}
}

func TestRemove(t *testing.T) {
	s, _ := Open(tempPaths(t))
	_ = s.Upsert(Project{Name: "a"})
	_ = s.Upsert(Project{Name: "b"})

	if err := s.Remove("A"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := s.Get("a"); ok {
		t.Fatal("project still present after Remove")
	}
	if len(s.List()) != 1 {
		t.Fatalf("expected 1 project left, got %d", len(s.List()))
	}
	if err := s.Remove("missing"); err == nil {
		t.Fatal("expected error removing unknown project")
	}
}

func TestFindByDir(t *testing.T) {
	s, _ := Open(tempPaths(t))
	_ = s.Upsert(Project{Name: "mono", Dir: "/srv/mono"})
	_ = s.Upsert(Project{Name: "web", Dir: "/srv/mono/apps/web"})
	_ = s.Upsert(Project{Name: "nodir"}) // registered by name only

	tests := []struct {
		name  string
		dir   string
		want  string
		found bool
	}{
		{"exact match", "/srv/mono", "mono", true},
		{"nested dir picks longest prefix", "/srv/mono/apps/web/src", "web", true},
		{"sibling of nested stays with parent", "/srv/mono/apps/api", "mono", true},
		{"prefix without separator is not a match", "/srv/monorepo", "", false},
		{"unknown dir", "/tmp/elsewhere", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := s.FindByDir(tc.dir)
			if ok != tc.found {
				t.Fatalf("found = %v, want %v", ok, tc.found)
			}
			if p.Name != tc.want {
				t.Fatalf("project = %q, want %q", p.Name, tc.want)
			}
		})
	}
}

func TestReloadPicksUpExternalChanges(t *testing.T) {
	paths := tempPaths(t)
	s, _ := Open(paths)
	_ = s.Upsert(Project{Name: "old"})

	// another process rewrites the file
	other, _ := Open(paths)
	_ = other.Upsert(Project{Name: "new"})

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if _, ok := s.Get("new"); !ok {
		t.Fatal("Reload did not pick up the externally added project")
	}

	// file removed → reload yields empty state, not an error
	_ = os.Remove(paths.Registry)
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload after delete: %v", err)
	}
	if len(s.List()) != 0 {
		t.Fatal("expected empty state after registry file vanished")
	}

	// corrupt file → error surfaces
	_ = os.WriteFile(paths.Registry, []byte("nope"), 0o644)
	if err := s.Reload(); err == nil {
		t.Fatal("expected error reloading a corrupt registry")
	}
}

func TestGroups(t *testing.T) {
	s, _ := Open(tempPaths(t))

	if err := s.UpsertGroup(Group{Name: "stack", Projects: []string{"web"}}); err != nil {
		t.Fatalf("UpsertGroup: %v", err)
	}
	if err := s.UpsertGroup(Group{Name: "STACK", Projects: []string{"web", "api"}}); err != nil {
		t.Fatalf("UpsertGroup: %v", err)
	}
	groups := s.Groups()
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	g, ok := s.GetGroup("stack")
	if !ok || len(g.Projects) != 2 {
		t.Fatalf("GetGroup = %+v, %v", g, ok)
	}
	if _, ok := s.GetGroup("nope"); ok {
		t.Fatal("GetGroup found a group that does not exist")
	}
	if err := s.RemoveGroup("stack"); err != nil {
		t.Fatalf("RemoveGroup: %v", err)
	}
	if err := s.RemoveGroup("stack"); err == nil {
		t.Fatal("expected error removing an unknown group")
	}
	if len(s.Groups()) != 0 {
		t.Fatal("group still present after RemoveGroup")
	}
}

func TestListIsSortedByName(t *testing.T) {
	s, _ := Open(tempPaths(t))
	for _, n := range []string{"web", "api", "db"} {
		_ = s.Upsert(Project{Name: n})
	}
	got := s.List()
	want := []string{"api", "db", "web"}
	for i := range want {
		if got[i].Name != want[i] {
			t.Fatalf("List order = %v, want %v", names(got), want)
		}
	}
}

func names(ps []Project) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name
	}
	return out
}

func TestListReturnsCopy(t *testing.T) {
	s, _ := Open(tempPaths(t))
	_ = s.Upsert(Project{Name: "a", PreferPort: 1})

	list := s.List()
	list[0].PreferPort = 999
	if p, _ := s.Get("a"); p.PreferPort != 1 {
		t.Fatal("List leaked internal state (mutation visible)")
	}
}
