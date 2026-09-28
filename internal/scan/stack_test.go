package scan

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/antoniojosev/pm/internal/registry"
)

// dirWith creates a temp project dir holding the given files (name → body).
func dirWith(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDetect(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  Detected
		ok    bool
	}{
		{
			name:  "nothing recognizable",
			files: map[string]string{"README.md": "hi"},
			ok:    false,
		},
		{
			name:  "docker compose",
			files: map[string]string{"compose.yaml": "services: {}"},
			want: Detected{Stack: "docker", Kind: registry.KindDocker, Cmd: []string{"docker", "compose", "up"},
				PortStrategy: registry.PortStrategy{Kind: "docker"}},
			ok: true,
		},
		{
			name:  "compose wins over package.json",
			files: map[string]string{"docker-compose.yml": "", "package.json": "{}"},
			want: Detected{Stack: "docker", Kind: registry.KindDocker, Cmd: []string{"docker", "compose", "up"},
				PortStrategy: registry.PortStrategy{Kind: "docker"}},
			ok: true,
		},
		{
			name:  "flutter",
			files: map[string]string{"pubspec.yaml": ""},
			want: Detected{Stack: "flutter", Kind: registry.KindNative, Cmd: []string{"flutter", "run", "-d", "web-server"},
				PreferPort: 8080, PortStrategy: registry.PortStrategy{Kind: "flutter", Flag: "--web-port"}},
			ok: true,
		},
		{
			name:  "django",
			files: map[string]string{"manage.py": ""},
			want: Detected{Stack: "django", Kind: registry.KindNative, Cmd: []string{"python", "manage.py", "runserver"},
				PreferPort: 8000, PortStrategy: registry.PortStrategy{Kind: "django", Template: "0.0.0.0:{port}"}},
			ok: true,
		},
		{
			name:  "go",
			files: map[string]string{"go.mod": "module x"},
			want: Detected{Stack: "go", Kind: registry.KindNative, Cmd: []string{"go", "run", "."},
				PortStrategy: registry.PortStrategy{Kind: "env", Env: "PORT"}},
			ok: true,
		},
		{
			name:  "next with pnpm",
			files: map[string]string{"package.json": `{"scripts":{"dev":"next dev"},"dependencies":{"next":"15"}}`, "pnpm-lock.yaml": ""},
			want: Detected{Stack: "next", Kind: registry.KindNative, Cmd: []string{"pnpm", "run", "dev"},
				PreferPort: 3000, PortStrategy: registry.PortStrategy{Kind: "next", Flag: "-p"}},
			ok: true,
		},
		{
			name:  "vite as devDependency with yarn",
			files: map[string]string{"package.json": `{"scripts":{"dev":"vite"},"devDependencies":{"vite":"5"}}`, "yarn.lock": ""},
			want: Detected{Stack: "vite", Kind: registry.KindNative, Cmd: []string{"yarn", "run", "dev"},
				PreferPort: 5173, PortStrategy: registry.PortStrategy{Kind: "vite", Flag: "--port"}},
			ok: true,
		},
		{
			name:  "nuxt with bun",
			files: map[string]string{"package.json": `{"scripts":{"dev":"nuxt dev"},"dependencies":{"nuxt":"3"}}`, "bun.lock": ""},
			want: Detected{Stack: "nuxt", Kind: registry.KindNative, Cmd: []string{"bun", "run", "dev"},
				PreferPort: 3000, PortStrategy: registry.PortStrategy{Kind: "env", Env: "PORT"}},
			ok: true,
		},
		{
			name:  "plain node falls back to start script",
			files: map[string]string{"package.json": `{"scripts":{"start":"node server.js"}}`},
			want: Detected{Stack: "node", Kind: registry.KindNative, Cmd: []string{"npm", "run", "start"},
				PortStrategy: registry.PortStrategy{Kind: "env", Env: "PORT"}},
			ok: true,
		},
		{
			name:  "serve script when neither dev nor start",
			files: map[string]string{"package.json": `{"scripts":{"serve":"x"}}`},
			want: Detected{Stack: "node", Kind: registry.KindNative, Cmd: []string{"npm", "run", "serve"},
				PortStrategy: registry.PortStrategy{Kind: "env", Env: "PORT"}},
			ok: true,
		},
		{
			name:  "unparseable package.json still counts as node",
			files: map[string]string{"package.json": `{oops`},
			want: Detected{Stack: "node", Kind: registry.KindNative, Cmd: []string{"npm", "run", "dev"},
				PortStrategy: registry.PortStrategy{Kind: "env", Env: "PORT"}},
			ok: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Detect(dirWith(t, tc.files))
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (got %+v)", ok, tc.ok, got)
			}
			if ok && !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Detect =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}

func TestRoots(t *testing.T) {
	root1 := t.TempDir()
	root2 := t.TempDir()
	mk := func(root, name string, files map[string]string) {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for f, body := range files {
			if err := os.WriteFile(filepath.Join(dir, f), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk(root1, "web", map[string]string{"package.json": `{"devDependencies":{"vite":"5"}}`})
	mk(root1, "notes", map[string]string{"todo.txt": ""})         // not a project
	mk(root1, ".hidden", map[string]string{"go.mod": "module h"}) // hidden → skipped
	mk(root2, "web", map[string]string{"go.mod": "module dup"})   // same name → first wins
	mk(root2, "api", map[string]string{"manage.py": ""})
	if err := os.WriteFile(filepath.Join(root1, "go.mod"), []byte("module file"), 0o644); err != nil {
		t.Fatal(err) // a file at root level, not a dir → ignored
	}

	got := Roots([]string{root1, "/does/not/exist", root2})
	if len(got) != 2 {
		t.Fatalf("expected 2 drafts, got %d: %+v", len(got), got)
	}
	web, api := got[0], got[1]
	if web.Name != "web" || web.Stack != "vite" || web.Dir != filepath.Join(root1, "web") || !web.Draft || web.Tier != registry.TierEphemeral {
		t.Fatalf("web draft = %+v", web)
	}
	if api.Name != "api" || api.Stack != "django" || api.PreferPort != 8000 || !api.Draft {
		t.Fatalf("api draft = %+v", api)
	}
}

func TestExpand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tests := []struct{ in, want string }{
		{"~/projects", filepath.Join(home, "projects")},
		{"~", home},
		{"/abs/path", "/abs/path"},
		{"relative", "relative"},
	}
	for _, tc := range tests {
		if got := expand(tc.in); got != tc.want {
			t.Errorf("expand(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
