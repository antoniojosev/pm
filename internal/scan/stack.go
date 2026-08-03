// Package scan infers a project's stack, launch command, port and port
// injection strategy from files on disk. Everything it produces is a *draft*
// the user can override; a manual field always wins over a rescan.
package scan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/antoniojosev/pm/internal/registry"
)

// Detected is the inferred launch profile for a directory.
type Detected struct {
	Stack        string
	Kind         registry.Kind
	Cmd          []string
	PreferPort   int
	PortStrategy registry.PortStrategy
}

// Detect inspects dir and returns a best-effort launch profile, ok=false if
// nothing recognizable is found.
func Detect(dir string) (Detected, bool) {
	switch {
	case exists(dir, "docker-compose.yml"), exists(dir, "docker-compose.yaml"), exists(dir, "compose.yml"), exists(dir, "compose.yaml"):
		return Detected{
			Stack: "docker", Kind: registry.KindDocker,
			Cmd:          []string{"docker", "compose", "up"},
			PortStrategy: registry.PortStrategy{Kind: "docker"},
		}, true
	case exists(dir, "pubspec.yaml"):
		return Detected{
			Stack: "flutter", Kind: registry.KindNative,
			Cmd:          []string{"flutter", "run", "-d", "web-server"},
			PreferPort:   8080,
			PortStrategy: registry.PortStrategy{Kind: "flutter", Flag: "--web-port"},
		}, true
	case exists(dir, "manage.py"):
		return Detected{
			Stack: "django", Kind: registry.KindNative,
			Cmd:          []string{"python", "manage.py", "runserver"},
			PreferPort:   8000,
			PortStrategy: registry.PortStrategy{Kind: "django", Template: "0.0.0.0:{port}"},
		}, true
	case exists(dir, "go.mod"):
		return Detected{
			Stack: "go", Kind: registry.KindNative,
			Cmd:          []string{"go", "run", "."},
			PortStrategy: registry.PortStrategy{Kind: "env", Env: "PORT"},
		}, true
	case exists(dir, "package.json"):
		return detectNode(dir), true
	}
	return Detected{}, false
}

func detectNode(dir string) Detected {
	d := Detected{Stack: "node", Kind: registry.KindNative}
	var pkg struct {
		Scripts      map[string]string `json:"scripts"`
		Dependencies map[string]string `json:"dependencies"`
		DevDeps      map[string]string `json:"devDependencies"`
	}
	if b, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
		_ = json.Unmarshal(b, &pkg)
	}
	runner := nodeRunner(dir)
	// pick the most dev-like script available
	script := "dev"
	for _, cand := range []string{"dev", "start", "serve"} {
		if _, ok := pkg.Scripts[cand]; ok {
			script = cand
			break
		}
	}
	d.Cmd = append(runner, "run", script)

	all := map[string]string{}
	for k := range pkg.Dependencies {
		all[k] = ""
	}
	for k := range pkg.DevDeps {
		all[k] = ""
	}
	switch {
	case has(all, "next"):
		d.Stack = "next"
		d.PreferPort = 3000
		d.PortStrategy = registry.PortStrategy{Kind: "next", Flag: "-p"}
	case has(all, "vite"):
		d.Stack = "vite"
		d.PreferPort = 5173
		d.PortStrategy = registry.PortStrategy{Kind: "vite", Flag: "--port"}
	case has(all, "nuxt"):
		d.Stack = "nuxt"
		d.PreferPort = 3000
		d.PortStrategy = registry.PortStrategy{Kind: "env", Env: "PORT"}
	default:
		d.PortStrategy = registry.PortStrategy{Kind: "env", Env: "PORT"}
	}
	return d
}

// nodeRunner picks the package manager by lockfile.
func nodeRunner(dir string) []string {
	switch {
	case exists(dir, "bun.lockb"), exists(dir, "bun.lock"):
		return []string{"bun"}
	case exists(dir, "pnpm-lock.yaml"):
		return []string{"pnpm"}
	case exists(dir, "yarn.lock"):
		return []string{"yarn"}
	default:
		return []string{"npm"}
	}
}

func has(m map[string]string, k string) bool { _, ok := m[k]; return ok }

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// Roots scans each root one level deep for recognizable projects and returns
// draft entries. Names collide-safe: derived from the directory base name.
func Roots(roots []string) []registry.Project {
	var out []registry.Project
	seen := map[string]bool{}
	for _, root := range roots {
		entries, err := os.ReadDir(expand(root))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			dir := filepath.Join(expand(root), e.Name())
			det, ok := Detect(dir)
			if !ok {
				continue
			}
			name := e.Name()
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, registry.Project{
				Name:         name,
				Dir:          dir,
				Cmd:          det.Cmd,
				Kind:         det.Kind,
				Tier:         registry.TierEphemeral,
				PreferPort:   det.PreferPort,
				PortStrategy: det.PortStrategy,
				Stack:        det.Stack,
				Draft:        true,
			})
		}
	}
	return out
}

func expand(p string) string {
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}
