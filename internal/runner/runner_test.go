package runner

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/antoniojosev/pm/internal/config"
	"github.com/antoniojosev/pm/internal/detector"
	"github.com/antoniojosev/pm/internal/registry"
)

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

func TestResolvePort(t *testing.T) {
	native := registry.Project{Name: "web", Kind: registry.KindNative, PreferPort: 3000}
	docker := registry.Project{Name: "db", Kind: registry.KindDocker, PreferPort: 5432}

	tests := []struct {
		name     string
		p        registry.Project
		opts     LaunchOpts
		occupied map[int]bool
		wantPort int
		wantNote string
		wantErr  bool
	}{
		{
			name: "preferred port is free",
			p:    native, occupied: map[int]bool{},
			wantPort: 3000,
		},
		{
			name: "second service jumps to the next port",
			p:    native, occupied: map[int]bool{3000: true},
			wantPort: 3001, wantNote: "port 3000 taken → using 3001",
		},
		{
			name: "skips a run of taken ports",
			p:    native, occupied: map[int]bool{3000: true, 3001: true, 3002: true},
			wantPort: 3003, wantNote: "port 3000 taken → using 3003",
		},
		{
			name: "explicit port overrides prefer",
			p:    native, opts: LaunchOpts{Port: 4000}, occupied: map[int]bool{3000: true},
			wantPort: 4000,
		},
		{
			name: "explicit port also collides",
			p:    native, opts: LaunchOpts{Port: 4000}, occupied: map[int]bool{4000: true},
			wantPort: 4001, wantNote: "port 4000 taken → using 4001",
		},
		{
			name: "force refuses to reassign",
			p:    native, opts: LaunchOpts{Force: true}, occupied: map[int]bool{3000: true},
			wantErr: true,
		},
		{
			name: "force with free port is fine",
			p:    native, opts: LaunchOpts{Force: true}, occupied: map[int]bool{},
			wantPort: 3000,
		},
		{
			name: "docker keeps its compose mapping even if taken",
			p:    docker, occupied: map[int]bool{5432: true},
			wantPort: 5432,
		},
		{
			name: "no preferred port means let the stack choose",
			p:    registry.Project{Name: "x", Kind: registry.KindNative}, occupied: map[int]bool{3000: true},
			wantPort: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			port, note, err := resolvePort(tc.p, tc.opts, tc.occupied)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if port != tc.wantPort {
				t.Fatalf("port = %d, want %d", port, tc.wantPort)
			}
			if note != tc.wantNote {
				t.Fatalf("note = %q, want %q", note, tc.wantNote)
			}
		})
	}
}

func TestNextFree(t *testing.T) {
	tests := []struct {
		name     string
		from     int
		occupied map[int]bool
		want     int
	}{
		{"immediate neighbour", 3000, map[int]bool{3000: true}, 3001},
		{"gap after a run", 3000, map[int]bool{3000: true, 3001: true}, 3002},
		{"never returns the starting port", 8080, map[int]bool{}, 8081},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextFree(tc.from, tc.occupied); got != tc.want {
				t.Fatalf("nextFree(%d) = %d, want %d", tc.from, got, tc.want)
			}
		})
	}
	t.Run("gives up after the search window", func(t *testing.T) {
		occ := map[int]bool{}
		for p := 3000; p < 3300; p++ {
			occ[p] = true
		}
		if got := nextFree(3000, occ); got != 3000 {
			t.Fatalf("expected fallback to the original port, got %d", got)
		}
	})
}

func TestOccupiedFrom(t *testing.T) {
	insts := []detector.Instance{
		{Port: 3000}, {Port: 0}, {Port: 3000}, {Port: 5432, Docker: true},
	}
	got := occupiedFrom(insts)
	if !reflect.DeepEqual(SortedPorts(got), []int{3000, 5432}) {
		t.Fatalf("occupied = %v", SortedPorts(got))
	}
}

func TestApplyPort(t *testing.T) {
	tests := []struct {
		name    string
		p       registry.Project
		port    int
		wantCmd []string
		wantEnv []string
	}{
		{
			name:    "vite flag through npm needs --",
			p:       registry.Project{Kind: registry.KindNative, Cmd: []string{"npm", "run", "dev"}, PortStrategy: registry.PortStrategy{Flag: "--port"}},
			port:    5173,
			wantCmd: []string{"npm", "run", "dev", "--", "--port", "5173"},
		},
		{
			name:    "pnpm with explicit -- already present",
			p:       registry.Project{Kind: registry.KindNative, Cmd: []string{"pnpm", "dev", "--"}, PortStrategy: registry.PortStrategy{Flag: "-p"}},
			port:    3000,
			wantCmd: []string{"pnpm", "dev", "--", "-p", "3000"},
		},
		{
			name:    "direct binary takes flag without --",
			p:       registry.Project{Kind: registry.KindNative, Cmd: []string{"flutter", "run", "-d", "web-server"}, PortStrategy: registry.PortStrategy{Flag: "--web-port"}},
			port:    8080,
			wantCmd: []string{"flutter", "run", "-d", "web-server", "--web-port", "8080"},
		},
		{
			name:    "env strategy leaves command alone",
			p:       registry.Project{Kind: registry.KindNative, Cmd: []string{"go", "run", "."}, PortStrategy: registry.PortStrategy{Env: "PORT"}},
			port:    8090,
			wantCmd: []string{"go", "run", "."},
			wantEnv: []string{"PORT=8090"},
		},
		{
			name:    "template splices {port}",
			p:       registry.Project{Kind: registry.KindNative, Cmd: []string{"python", "manage.py", "runserver"}, PortStrategy: registry.PortStrategy{Template: "0.0.0.0:{port}"}},
			port:    8000,
			wantCmd: []string{"python", "manage.py", "runserver", "0.0.0.0:8000"},
		},
		{
			name:    "docker is untouched",
			p:       registry.Project{Kind: registry.KindDocker, Cmd: []string{"docker", "compose", "up"}, PortStrategy: registry.PortStrategy{Env: "PORT"}},
			port:    5432,
			wantCmd: []string{"docker", "compose", "up"},
		},
		{
			name:    "zero port injects nothing",
			p:       registry.Project{Kind: registry.KindNative, Cmd: []string{"npm", "run", "dev"}, PortStrategy: registry.PortStrategy{Flag: "--port"}},
			port:    0,
			wantCmd: []string{"npm", "run", "dev"},
		},
		{
			name:    "no strategy injects nothing",
			p:       registry.Project{Kind: registry.KindNative, Cmd: []string{"./server"}},
			port:    9000,
			wantCmd: []string{"./server"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			original := append([]string{}, tc.p.Cmd...)
			cmd, env := applyPort(tc.p, tc.port)
			if !reflect.DeepEqual(cmd, tc.wantCmd) {
				t.Fatalf("cmd = %v, want %v", cmd, tc.wantCmd)
			}
			if !reflect.DeepEqual(env, tc.wantEnv) {
				t.Fatalf("env = %v, want %v", env, tc.wantEnv)
			}
			if !reflect.DeepEqual(tc.p.Cmd, original) {
				t.Fatal("applyPort mutated the project's Cmd slice")
			}
		})
	}
}

func TestShellJoin(t *testing.T) {
	tests := []struct {
		in   []string
		want string
	}{
		{[]string{"npm", "run", "dev"}, "npm run dev"},
		{[]string{"sh", "-c", "echo hi"}, `sh -c "echo hi"`},
		{[]string{"node", `--title="x"`}, `node "--title=\"x\""`},
		{nil, ""},
	}
	for _, tc := range tests {
		if got := shellJoin(tc.in); got != tc.want {
			t.Errorf("shellJoin(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestUnitName(t *testing.T) {
	if got := UnitName("web"); got != "pm-web.service" {
		t.Fatalf("UnitName = %q", got)
	}
}

func TestStartRefusesProjectWithoutDir(t *testing.T) {
	paths := tempPaths(t)
	store, _ := registry.Open(paths)
	_ = store.Upsert(registry.Project{Name: "remote", Kind: registry.KindNative, Cmd: []string{"true"}})
	r := New(paths, store)

	_, err := r.Start("remote", LaunchOpts{})
	if err == nil {
		t.Fatal("expected error launching a project registered by name only")
	}
	if !strings.Contains(err.Error(), "no local directory") {
		t.Fatalf("unexpected error: %v", err)
	}
	// the guard must fire before any side effect
	if _, err := os.Stat(paths.LogsDir); !os.IsNotExist(err) {
		t.Fatal("launch created runtime dirs before validating the project")
	}
}

func TestStartUnknownProject(t *testing.T) {
	paths := tempPaths(t)
	store, _ := registry.Open(paths)
	r := New(paths, store)
	if _, err := r.Start("ghost", LaunchOpts{}); err == nil {
		t.Fatal("expected error for unregistered project")
	}
}

func TestUpUnknownGroup(t *testing.T) {
	paths := tempPaths(t)
	store, _ := registry.Open(paths)
	r := New(paths, store)
	if _, err := r.Up("nope"); err == nil {
		t.Fatal("expected error for unknown group")
	}
}

func TestUpReportsPerProjectErrors(t *testing.T) {
	paths := tempPaths(t)
	store, _ := registry.Open(paths)
	_ = store.Upsert(registry.Project{Name: "remote", Kind: registry.KindNative})
	_ = store.UpsertGroup(registry.Group{Name: "g", Projects: []string{"remote", "ghost"}})
	r := New(paths, store)

	res, err := r.Up("g")
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("expected 2 results, got %d", len(res))
	}
	for _, x := range res {
		if !strings.HasPrefix(x.Note, "error:") {
			t.Fatalf("expected an error note for %s, got %q", x.Project, x.Note)
		}
	}
}

func TestWriteUnit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	paths := tempPaths(t)
	store, _ := registry.Open(paths)
	r := New(paths, store)

	p := registry.Project{Name: "web", Dir: "/srv/web", AutoRestart: true}
	if err := r.writeUnit(p, []string{"npm", "run", "dev", "--", "--port", "5173"}, []string{"NODE_ENV=development"}); err != nil {
		t.Fatalf("writeUnit: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(home, ".config", "systemd", "user", "pm-web.service"))
	if err != nil {
		t.Fatalf("unit not written under $HOME: %v", err)
	}
	unit := string(b)
	for _, want := range []string{
		"Description=pm project web",
		"WorkingDirectory=/srv/web",
		"Environment=PM_PROJECT=web",
		"Environment=NODE_ENV=development",
		"ExecStart=npm run dev -- --port 5173",
		"StandardOutput=append:" + paths.LogFile("web"),
		"Restart=on-failure",
		"WantedBy=default.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit missing %q:\n%s", want, unit)
		}
	}

	p.AutoRestart = false
	_ = r.writeUnit(p, []string{"true"}, nil)
	b, _ = os.ReadFile(r.unitPath("web"))
	if strings.Contains(string(b), "Restart=") {
		t.Fatal("Restart= present without AutoRestart")
	}
}

func TestDetectorResolvesByDir(t *testing.T) {
	paths := tempPaths(t)
	store, _ := registry.Open(paths)
	_ = store.Upsert(registry.Project{Name: "web", Dir: "/srv/web"})
	r := New(paths, store)

	name, unit, ok := r.Det.DirToName("/srv/web/src")
	if !ok || name != "web" || unit != "pm-web.service" {
		t.Fatalf("DirToName = %q, %q, %v", name, unit, ok)
	}
	if _, _, ok := r.Det.DirToName("/elsewhere"); ok {
		t.Fatal("DirToName matched an unknown dir")
	}
}
