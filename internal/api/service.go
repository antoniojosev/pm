// Package api is the service layer shared by the HTTP daemon and the MCP
// server. It composes registry + detector + runner into high-level operations
// and JSON-friendly view models.
package api

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/antoniojosev/pm/internal/config"
	"github.com/antoniojosev/pm/internal/detector"
	"github.com/antoniojosev/pm/internal/registry"
	"github.com/antoniojosev/pm/internal/runner"
	"github.com/antoniojosev/pm/internal/scan"
)

// Service is the single entry point for all pm operations.
type Service struct {
	Paths config.Paths
	Store *registry.Store
	Run   *runner.Runner
	Roots []string // directories scanned for projects
}

// New wires a Service around a store.
func New(paths config.Paths, store *registry.Store, roots []string) *Service {
	return &Service{
		Paths: paths,
		Store: store,
		Run:   runner.New(paths, store),
		Roots: roots,
	}
}

// Row is a unified view of a project: catalog metadata + live status.
type Row struct {
	Name     string `json:"name"`
	Port     int    `json:"port"`
	URL      string `json:"url"`                 // http://<host>.localhost (via proxy)
	LocalURL string `json:"local_url,omitempty"` // http://localhost:<port> (direct, always works)
	Tier     string `json:"tier"`
	Status   string `json:"status"` // up | down | unclaimed
	Kind     string `json:"kind"`
	Stack    string `json:"stack,omitempty"`
	Source   string `json:"source,omitempty"`
	PID      int    `json:"pid,omitempty"`
	Unit     string `json:"unit,omitempty"`
	Group    string `json:"group,omitempty"`
	Cmd      string `json:"cmd,omitempty"`
	Cwd      string `json:"cwd,omitempty"`
	Docker   bool   `json:"docker,omitempty"`
	Draft    bool   `json:"draft,omitempty"`
	System   bool   `json:"system,omitempty"` // unmanaged with no PID: system/root process, not manageable
}

// PS returns the docker-ps-like unified table: every registered project plus
// any live listener not attributable to one (unclaimed).
func (s *Service) PS() ([]Row, error) {
	_ = s.Store.Reload()
	insts, err := s.Run.Det.Snapshot()
	if err != nil {
		return nil, err
	}
	// index live instances by project name and by port; highest-confidence
	// source wins (marker/cgroup/docker beats the cwd heuristic).
	live := map[string]detector.Instance{}
	byPort := map[int]detector.Instance{}
	for _, i := range insts {
		if i.Project != "" {
			key := strings.ToLower(i.Project)
			if cur, ok := live[key]; !ok || sourceRank(i.Source) > sourceRank(cur.Source) {
				live[key] = i
			}
		}
		if i.Port != 0 {
			if cur, ok := byPort[i.Port]; !ok || sourceRank(i.Source) > sourceRank(cur.Source) {
				byPort[i.Port] = i
			}
		}
	}

	var rows []Row
	registered := map[string]bool{}
	consumed := map[int]bool{} // live ports already linked to a registered project
	for _, p := range s.Store.List() {
		registered[strings.ToLower(p.Name)] = true
		r := Row{
			Name:  p.Name,
			Tier:  string(p.Tier),
			Kind:  string(p.Kind),
			Stack: p.Stack,
			Group: p.Group,
			URL:   "http://" + p.Hostname(),
			Cmd:   strings.Join(p.Cmd, " "),
			Cwd:   p.Dir,
			Draft: p.Draft,
			Port:  p.PreferPort,
		}
		// match a live listener: first by attributed name, then by the
		// project's preferred port (so claimed/port-only projects link too).
		inst, matched := live[strings.ToLower(p.Name)]
		if !matched && p.PreferPort != 0 {
			if bi, ok := byPort[p.PreferPort]; ok {
				inst, matched = bi, true
			}
		}
		if matched {
			r.Status = "up"
			r.Port = inst.Port
			r.PID = inst.PID
			r.Source = string(inst.Source)
			r.Unit = inst.Unit
			r.Docker = inst.Docker
			if inst.Port != 0 {
				r.LocalURL = fmt.Sprintf("http://localhost:%d", inst.Port)
				consumed[inst.Port] = true
			}
		} else {
			r.Status = "down"
		}
		rows = append(rows, r)
	}

	// unclaimed: live listeners not attributed to (nor sharing a port with) a
	// registered project. Those without a visible PID are system/root ports.
	for _, i := range insts {
		name := strings.ToLower(i.Project)
		if name != "" && registered[name] {
			continue
		}
		if consumed[i.Port] {
			continue
		}
		disp := i.Project
		if disp == "" {
			disp = "?"
		}
		rows = append(rows, Row{
			Name:   disp,
			Port:   i.Port,
			Status: "unclaimed",
			Source: string(i.Source),
			PID:    i.PID,
			Cmd:    i.Cmd,
			Cwd:    i.Cwd,
			Docker: i.Docker,
			System: i.PID == 0 && !i.Docker,
		})
	}

	sort.SliceStable(rows, func(a, b int) bool {
		if rows[a].Status != rows[b].Status {
			return statusRank(rows[a].Status) < statusRank(rows[b].Status)
		}
		return rows[a].Name < rows[b].Name
	})
	return rows, nil
}

// sourceRank orders attribution confidence: launched-by-us > docker labels >
// cwd heuristic > unknown.
func sourceRank(s detector.Source) int {
	switch s {
	case detector.SourceMarker:
		return 4
	case detector.SourceCgroup:
		return 3
	case detector.SourceDocker:
		return 3
	case detector.SourceCwd:
		return 2
	default:
		return 1
	}
}

func statusRank(s string) int {
	switch s {
	case "up":
		return 0
	case "down":
		return 1
	default:
		return 2
	}
}

// --- lifecycle passthroughs ---------------------------------------------

func (s *Service) Start(name string, opts runner.LaunchOpts) (runner.Result, error) {
	return s.Run.Start(name, opts)
}
func (s *Service) Stop(name string) error { return s.Run.Stop(name) }
func (s *Service) Restart(name string, opts runner.LaunchOpts) (runner.Result, error) {
	return s.Run.Restart(name, opts)
}
func (s *Service) Promote(name string) error                { return s.Run.Promote(name) }
func (s *Service) Demote(name string) error                 { return s.Run.Demote(name) }
func (s *Service) Up(group string) ([]runner.Result, error) { return s.Run.Up(group) }
func (s *Service) Down(group string) error                  { return s.Run.Down(group) }
func (s *Service) RunAdhoc(dir string, cmd []string, opts runner.AdhocOpts) (runner.Result, error) {
	return s.Run.RunAdhoc(dir, cmd, opts)
}

// StopInstance / RestartInstance act on a live listener by port (for
// unclaimed processes pm did not launch).
func (s *Service) StopInstance(port int) error { return s.Run.StopInstance(port) }
func (s *Service) RestartInstance(port int) (runner.Result, error) {
	return s.Run.RestartInstance(port)
}

// --- registry CRUD -------------------------------------------------------

func (s *Service) Add(p registry.Project) error {
	if p.Name == "" {
		return fmt.Errorf("name is required")
	}
	if p.Tier == "" {
		p.Tier = registry.TierEphemeral
	}
	if p.Kind == "" {
		p.Kind = registry.KindNative
	}
	return s.Store.Upsert(p)
}

func (s *Service) Edit(p registry.Project) error { return s.Store.Upsert(p) }
func (s *Service) Remove(name string) error {
	_ = s.Run.Stop(name)
	return s.Store.Remove(name)
}
func (s *Service) Get(name string) (registry.Project, bool) { return s.Store.Get(name) }
func (s *Service) List() []registry.Project                 { return s.Store.List() }

// Claim adopts a live unclaimed listener into the registry.
func (s *Service) Claim(port int, name string) (registry.Project, error) {
	insts, err := s.Run.Det.Snapshot()
	if err != nil {
		return registry.Project{}, err
	}
	for _, i := range insts {
		if i.Port != port {
			continue
		}
		dir := i.Cwd
		p := registry.Project{
			Name:       name,
			Dir:        dir,
			Tier:       registry.TierEphemeral,
			Kind:       registry.KindNative,
			PreferPort: port,
		}
		if i.Docker {
			p.Kind = registry.KindDocker
		}
		if dir != "" {
			if det, ok := scan.Detect(dir); ok {
				p.Cmd = det.Cmd
				p.Stack = det.Stack
				p.PortStrategy = det.PortStrategy
			}
		}
		return p, s.Store.Upsert(p)
	}
	return registry.Project{}, fmt.Errorf("no listener on port %d", port)
}

// Scan discovers draft projects under the roots and merges the new ones
// (never overwriting existing entries). Returns the drafts added.
func (s *Service) Scan() ([]registry.Project, error) {
	drafts := scan.Roots(s.Roots)
	var added []registry.Project
	for _, d := range drafts {
		if _, exists := s.Store.Get(d.Name); exists {
			continue
		}
		if err := s.Store.Upsert(d); err != nil {
			return added, err
		}
		added = append(added, d)
	}
	return added, nil
}

// --- groups --------------------------------------------------------------

func (s *Service) Groups() []registry.Group        { return s.Store.Groups() }
func (s *Service) SetGroup(g registry.Group) error { return s.Store.UpsertGroup(g) }
func (s *Service) RemoveGroup(name string) error   { return s.Store.RemoveGroup(name) }

// Logs returns the last n lines of a project's log file.
func (s *Service) Logs(name string, n int) (string, error) {
	b, err := os.ReadFile(s.Paths.LogFile(name))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	lines := strings.Split(string(b), "\n")
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n"), nil
}

// LogPath exposes the log file path (for streaming tails).
func (s *Service) LogPath(name string) string { return s.Paths.LogFile(name) }

// BestURL returns the most reliable URL to open a project: the direct
// localhost:<port> if it's running (works with or without the proxy), falling
// back to its preferred port, then to the .localhost hostname.
func (s *Service) BestURL(name string) (string, error) {
	_ = s.Store.Reload()
	insts, _ := s.Run.Det.Snapshot()
	best := detector.Instance{}
	for _, i := range insts {
		if strings.EqualFold(i.Project, name) && i.Port != 0 {
			if best.Port == 0 || sourceRank(i.Source) > sourceRank(best.Source) {
				best = i
			}
		}
	}
	if best.Port != 0 {
		return fmt.Sprintf("http://localhost:%d", best.Port), nil
	}
	p, ok := s.Get(name)
	if !ok {
		return "", fmt.Errorf("project %q does not exist", name)
	}
	if p.PreferPort != 0 {
		return fmt.Sprintf("http://localhost:%d", p.PreferPort), nil
	}
	return "http://" + p.Hostname(), nil
}

// FSEntry is a directory entry for the web file browser.
type FSEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Stack string `json:"stack,omitempty"` // detected stack if it's a project dir
}

// FSListing is the response of a Browse call.
type FSListing struct {
	Path    string    `json:"path"`
	Parent  string    `json:"parent"`
	Entries []FSEntry `json:"entries"`
}

// Browse lists the sub-directories of path (defaulting to the first scan root
// or $HOME) so the dashboard can offer a folder picker. Files are omitted; we
// only pick project directories.
func (s *Service) Browse(path string) (FSListing, error) {
	if path == "" {
		if len(s.Roots) > 0 {
			path = s.Roots[0]
		} else {
			path, _ = os.UserHomeDir()
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return FSListing{}, err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return FSListing{}, err
	}
	out := FSListing{Path: abs, Parent: filepath.Dir(abs)}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		child := filepath.Join(abs, e.Name())
		fe := FSEntry{Name: e.Name(), Path: child, IsDir: true}
		if det, ok := scan.Detect(child); ok {
			fe.Stack = det.Stack
		}
		out.Entries = append(out.Entries, fe)
	}
	sort.Slice(out.Entries, func(a, b int) bool { return out.Entries[a].Name < out.Entries[b].Name })
	return out, nil
}

// DetectPath returns the inferred launch profile for a directory so the
// create-project form can prefill command / port / stack.
func (s *Service) DetectPath(path string) (scan.Detected, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return scan.Detected{}, false
	}
	return scan.Detect(abs)
}
