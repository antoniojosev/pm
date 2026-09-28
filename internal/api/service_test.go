package api

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/antoniojosev/pm/internal/config"
	"github.com/antoniojosev/pm/internal/detector"
	"github.com/antoniojosev/pm/internal/registry"
	"github.com/antoniojosev/pm/internal/runner"
)

// newTestService wires a Service against a throwaway PM_HOME with no live
// listeners and no external binaries reachable, so nothing can leak into the
// real ~/.pm, systemd or docker.
func newTestService(t *testing.T, roots ...string) *Service {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	paths := config.Paths{
		Root:      root,
		Registry:  filepath.Join(root, "registry.json"),
		LogsDir:   filepath.Join(root, "logs"),
		RunDir:    filepath.Join(root, "run"),
		Caddyfile: filepath.Join(root, "Caddyfile"),
	}
	if err := paths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	store, err := registry.Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	svc := New(paths, store, roots)
	svc.Run.Snap = func() ([]detector.Instance, error) { return nil, nil }
	return svc
}

func live(svc *Service, insts ...detector.Instance) {
	svc.Run.Snap = func() ([]detector.Instance, error) { return insts, nil }
}

func TestAddValidation(t *testing.T) {
	tests := []struct {
		name    string
		p       registry.Project
		wantErr string
		check   func(t *testing.T, p registry.Project)
	}{
		{
			name:    "name is required",
			p:       registry.Project{Dir: "/srv/x"},
			wantErr: "name is required",
		},
		{
			name: "dir is optional and defaults are filled",
			p:    registry.Project{Name: "remote"},
			check: func(t *testing.T, p registry.Project) {
				if p.Dir != "" || p.Tier != registry.TierEphemeral || p.Kind != registry.KindNative {
					t.Fatalf("stored = %+v", p)
				}
			},
		},
		{
			name: "explicit tier and kind are preserved",
			p:    registry.Project{Name: "db", Tier: registry.TierService, Kind: registry.KindDocker},
			check: func(t *testing.T, p registry.Project) {
				if p.Tier != registry.TierService || p.Kind != registry.KindDocker {
					t.Fatalf("stored = %+v", p)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t)
			err := svc.Add(tc.p)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				if len(svc.List()) != 0 {
					t.Fatal("invalid project must not be stored")
				}
				return
			}
			if err != nil {
				t.Fatalf("Add: %v", err)
			}
			stored, ok := svc.Get(tc.p.Name)
			if !ok {
				t.Fatal("project not stored")
			}
			tc.check(t, stored)
		})
	}
}

func TestEditGetListRemove(t *testing.T) {
	svc := newTestService(t)
	_ = svc.Add(registry.Project{Name: "web", PreferPort: 3000})
	_ = svc.Add(registry.Project{Name: "api"})

	if err := svc.Edit(registry.Project{Name: "web", PreferPort: 3100, Tier: registry.TierEphemeral, Kind: registry.KindNative}); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if p, _ := svc.Get("WEB"); p.PreferPort != 3100 {
		t.Fatalf("Edit not applied: %+v", p)
	}
	if got := svc.List(); len(got) != 2 || got[0].Name != "api" {
		t.Fatalf("List = %+v", got)
	}
	// Remove stops best-effort (nothing running here) and drops the entry.
	if err := svc.Remove("web"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := svc.Get("web"); ok {
		t.Fatal("project still present after Remove")
	}
	if err := svc.Remove("web"); err == nil {
		t.Fatal("expected error removing an unknown project")
	}
}

func TestGroups(t *testing.T) {
	svc := newTestService(t)
	if err := svc.SetGroup(registry.Group{Name: "stack", Projects: []string{"web", "api"}}); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}
	if g := svc.Groups(); len(g) != 1 || g[0].Name != "stack" {
		t.Fatalf("Groups = %+v", g)
	}
	if err := svc.RemoveGroup("stack"); err != nil {
		t.Fatalf("RemoveGroup: %v", err)
	}
	if err := svc.RemoveGroup("stack"); err == nil {
		t.Fatal("expected error removing an unknown group")
	}
}

func TestLogs(t *testing.T) {
	svc := newTestService(t)
	if out, err := svc.Logs("nolog", 10); err != nil || out != "" {
		t.Fatalf("missing log = %q, %v", out, err)
	}
	if err := os.WriteFile(svc.LogPath("web"), []byte("l1\nl2\nl3\nl4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		n    int
		want string
	}{
		{0, "l1\nl2\nl3\nl4\n"},
		{2, "l4\n"}, // trailing newline counts as an (empty) last line
		{3, "l3\nl4\n"},
		{100, "l1\nl2\nl3\nl4\n"},
	}
	for _, tc := range tests {
		out, err := svc.Logs("web", tc.n)
		if err != nil {
			t.Fatalf("Logs(%d): %v", tc.n, err)
		}
		if out != tc.want {
			t.Errorf("Logs(%d) = %q, want %q", tc.n, out, tc.want)
		}
	}
}

func TestPS(t *testing.T) {
	svc := newTestService(t)
	_ = svc.Add(registry.Project{Name: "web", Dir: "/srv/web", PreferPort: 5173, Stack: "vite", Group: "front", Cmd: []string{"npm", "run", "dev"}})
	_ = svc.Add(registry.Project{Name: "api", PreferPort: 8000})
	_ = svc.Add(registry.Project{Name: "idle", PreferPort: 7000})
	live(svc,
		// web is seen twice: the cwd heuristic and the marker; marker must win.
		detector.Instance{Project: "web", Port: 5174, PID: 11, Source: detector.SourceCwd},
		detector.Instance{Project: "web", Port: 5173, PID: 10, Source: detector.SourceMarker, Unit: "pm-web.service"},
		// api was started outside pm: unattributed, linked through its preferred port.
		detector.Instance{Port: 8000, PID: 20, Source: detector.SourceUnknown, Cmd: "python manage.py runserver"},
		// unknown native listener with a visible pid → unclaimed, manageable
		detector.Instance{Port: 9000, PID: 30, Source: detector.SourceUnknown, Cmd: "node x"},
		// root-owned listener → unclaimed, system
		detector.Instance{Port: 53, Source: detector.SourceUnknown},
		// docker stack not in the registry → unclaimed, named by compose project
		detector.Instance{Project: "shop", Port: 5432, Docker: true, Source: detector.SourceDocker},
	)

	rows, err := svc.PS()
	if err != nil {
		t.Fatalf("PS: %v", err)
	}
	byName := map[string]Row{}
	var order []string
	for _, r := range rows {
		key := r.Name
		if r.Name == "?" {
			key = "?" + strconv.Itoa(r.Port)
		}
		byName[key] = r
		order = append(order, key)
	}

	web := byName["web"]
	if web.Status != "up" || web.Port != 5173 || web.PID != 10 || web.Source != "marker" || web.Unit != "pm-web.service" {
		t.Fatalf("web row = %+v", web)
	}
	if web.URL != "http://web.localhost" || web.LocalURL != "http://localhost:5173" || web.Stack != "vite" || web.Group != "front" || web.Cmd != "npm run dev" || web.Cwd != "/srv/web" {
		t.Fatalf("web row = %+v", web)
	}
	api := byName["api"]
	if api.Status != "up" || api.Port != 8000 || api.PID != 20 || api.Source != "unknown" {
		t.Fatalf("api row (linked by preferred port) = %+v", api)
	}
	idle := byName["idle"]
	if idle.Status != "down" || idle.Port != 7000 || idle.LocalURL != "" {
		t.Fatalf("idle row = %+v", idle)
	}
	node := byName["?9000"]
	if node.Status != "unclaimed" || node.System || node.PID != 30 || node.Cmd != "node x" {
		t.Fatalf("unclaimed native row = %+v", node)
	}
	dns := byName["?53"]
	if dns.Status != "unclaimed" || !dns.System {
		t.Fatalf("system row = %+v", dns)
	}
	shop := byName["shop"]
	if shop.Status != "unclaimed" || !shop.Docker || shop.System {
		t.Fatalf("docker row = %+v", shop)
	}
	if _, dup := byName["?8000"]; dup {
		t.Fatal("listener consumed by a registered project must not also appear as unclaimed")
	}

	// up, down, unclaimed; then by name. Nameless rows share "?" and keep
	// their detection order (the sort is stable).
	want := []string{"api", "web", "idle", "?9000", "?53", "shop"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

func TestPSPicksUpRegistryChangesFromDisk(t *testing.T) {
	svc := newTestService(t)
	other, _ := registry.Open(svc.Paths)
	_ = other.Upsert(registry.Project{Name: "external"})

	rows, err := svc.PS()
	if err != nil {
		t.Fatalf("PS: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "external" || rows[0].Status != "down" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestClaim(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"dependencies":{"next":"15"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := newTestService(t)
	live(svc,
		detector.Instance{Port: 3000, PID: 5, Cwd: dir},
		detector.Instance{Port: 5432, Docker: true, Project: "shop"},
	)

	p, err := svc.Claim(3000, "front")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if p.Name != "front" || p.Dir != dir || p.PreferPort != 3000 || p.Stack != "next" || p.Kind != registry.KindNative {
		t.Fatalf("claimed = %+v", p)
	}
	if strings.Join(p.Cmd, " ") != "npm run dev" || p.PortStrategy.Flag != "-p" {
		t.Fatalf("claimed launch profile = %+v", p)
	}
	if _, ok := svc.Get("front"); !ok {
		t.Fatal("claimed project not persisted")
	}

	db, err := svc.Claim(5432, "db")
	if err != nil || db.Kind != registry.KindDocker || db.Dir != "" {
		t.Fatalf("docker claim = %+v, %v", db, err)
	}

	if _, err := svc.Claim(1, "x"); err == nil {
		t.Fatal("expected error claiming a port with no listener")
	}
}

func TestScanAddsDraftsOnce(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app", "go.mod"), []byte("module app"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := newTestService(t, root)
	_ = svc.Add(registry.Project{Name: "manual"})

	added, err := svc.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(added) != 1 || added[0].Name != "app" || !added[0].Draft || added[0].Stack != "go" {
		t.Fatalf("added = %+v", added)
	}
	again, _ := svc.Scan()
	if len(again) != 0 {
		t.Fatalf("second scan re-added %+v", again)
	}
	if p, _ := svc.Get("manual"); p.Draft {
		t.Fatal("scan must not overwrite existing entries")
	}
}

func TestBestURL(t *testing.T) {
	svc := newTestService(t)
	_ = svc.Add(registry.Project{Name: "web", PreferPort: 5173})
	_ = svc.Add(registry.Project{Name: "noport"})

	if _, err := svc.BestURL("ghost"); err == nil {
		t.Fatal("expected error for unknown project")
	}
	if got, _ := svc.BestURL("web"); got != "http://localhost:5173" {
		t.Fatalf("down project should use its preferred port, got %q", got)
	}
	if got, _ := svc.BestURL("noport"); got != "http://noport.localhost" {
		t.Fatalf("project without port should use its hostname, got %q", got)
	}

	live(svc,
		detector.Instance{Project: "web", Port: 5180, Source: detector.SourceCwd},
		detector.Instance{Project: "web", Port: 5174, Source: detector.SourceMarker},
	)
	if got, _ := svc.BestURL("web"); got != "http://localhost:5174" {
		t.Fatalf("live project should use its most trusted live port, got %q", got)
	}
}

func TestBrowseAndDetectPath(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string, files ...string) {
		dir := filepath.Join(root, rel)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("zeta")
	mk("alpha", "manage.py")
	mk(".git", "HEAD")
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := newTestService(t, root)

	listing, err := svc.Browse("")
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if listing.Path != root || listing.Parent != filepath.Dir(root) {
		t.Fatalf("listing = %+v", listing)
	}
	if len(listing.Entries) != 2 || listing.Entries[0].Name != "alpha" || listing.Entries[1].Name != "zeta" {
		t.Fatalf("entries = %+v (dirs only, sorted, hidden skipped)", listing.Entries)
	}
	if listing.Entries[0].Stack != "django" || listing.Entries[1].Stack != "" {
		t.Fatalf("stack annotation = %+v", listing.Entries)
	}
	if _, err := svc.Browse(filepath.Join(root, "missing")); err == nil {
		t.Fatal("expected error browsing a missing dir")
	}

	det, ok := svc.DetectPath(filepath.Join(root, "alpha"))
	if !ok || det.Stack != "django" {
		t.Fatalf("DetectPath = %+v, %v", det, ok)
	}
	if _, ok := svc.DetectPath(filepath.Join(root, "zeta")); ok {
		t.Fatal("DetectPath should report nothing for an empty dir")
	}
}

func TestBrowseWithoutRootsFallsBackToHome(t *testing.T) {
	svc := newTestService(t)
	home, _ := os.UserHomeDir()
	listing, err := svc.Browse("")
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if listing.Path != home {
		t.Fatalf("Path = %q, want $HOME %q", listing.Path, home)
	}
}

func TestStartPassthroughRefusesDirlessProject(t *testing.T) {
	svc := newTestService(t)
	_ = svc.Add(registry.Project{Name: "remote"})
	if _, err := svc.Start("remote", runner.LaunchOpts{}); err == nil {
		t.Fatal("expected error")
	}
}
