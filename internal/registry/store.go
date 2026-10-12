package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/antoniojosev/pm/internal/config"
)

// Store is a concurrency-safe, file-backed registry. Every mutation takes an
// in-process mutex and an flock on a sidecar file, and starts from what is on
// disk, so the CLI and the daemon (two processes sharing one registry.json)
// never overwrite each other's changes.
type Store struct {
	mu    sync.RWMutex
	path  string
	state State
}

// Open loads the registry from disk, creating an empty one if absent.
func Open(paths config.Paths) (*Store, error) {
	s := &Store{path: paths.Registry, state: State{Version: 1}}
	data, err := os.ReadFile(paths.Registry)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &s.state); err != nil {
			return nil, fmt.Errorf("registro corrupto (%s): %w", paths.Registry, err)
		}
	}
	if s.state.Version == 0 {
		s.state.Version = 1
	}
	return s, nil
}

// Reload re-reads the registry from disk, picking up changes made by other
// processes (e.g. the CLI while the daemon is running).
func (s *Store) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

// loadLocked replaces the in-memory state with the file's. Caller holds s.mu.
func (s *Store) loadLocked() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.state = State{Version: 1}
			return nil
		}
		return err
	}
	var st State
	if len(data) > 0 {
		if err := json.Unmarshal(data, &st); err != nil {
			return err
		}
	}
	if st.Version == 0 {
		st.Version = 1
	}
	s.state = st
	return nil
}

// update is the one way to mutate the registry: it serialises writers across
// goroutines and processes, loads the file's current state, runs fn on it and
// saves the result. If fn fails the change is dropped and the in-memory state
// goes back to what is on disk. Without the reload a process holding a stale
// copy would write it back and erase what another process added meanwhile,
// leases included.
func (s *Store) update(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.flock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	if err := fn(); err != nil {
		_ = s.loadLocked()
		return err
	}
	return s.save()
}

// flock takes an exclusive advisory lock on <registry>.lock and returns the
// function that releases it. The lock is on a sidecar because save() replaces
// the registry file by rename, which would orphan a lock held on the file.
func (s *Store) flock() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// save writes state atomically. Caller must hold the write lock.
func (s *Store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// List returns a copy of all projects sorted by name.
func (s *Store) List() []Project {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Project, len(s.state.Projects))
	copy(out, s.state.Projects)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Groups returns a copy of all groups.
func (s *Store) Groups() []Group {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Group, len(s.state.Groups))
	copy(out, s.state.Groups)
	return out
}

// Get returns a project by name (case-insensitive).
func (s *Store) Get(name string) (Project, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.state.Projects {
		if strings.EqualFold(p.Name, name) {
			return p, true
		}
	}
	return Project{}, false
}

// FindByDir returns the project whose Dir contains dir (longest match wins).
func (s *Store) FindByDir(dir string) (Project, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var best Project
	found := false
	for _, p := range s.state.Projects {
		if p.Dir == "" {
			continue
		}
		if dir == p.Dir || strings.HasPrefix(dir+string(os.PathSeparator), p.Dir+string(os.PathSeparator)) {
			if !found || len(p.Dir) > len(best.Dir) {
				best, found = p, true
			}
		}
	}
	return best, found
}

// Upsert inserts or replaces a project by name and persists.
func (s *Store) Upsert(p Project) error {
	if err := p.Validate(); err != nil {
		return err
	}
	return s.update(func() error {
		for i, existing := range s.state.Projects {
			if strings.EqualFold(existing.Name, p.Name) {
				s.state.Projects[i] = p
				return nil
			}
		}
		s.state.Projects = append(s.state.Projects, p)
		return nil
	})
}

// Remove deletes a project by name.
func (s *Store) Remove(name string) error {
	return s.update(func() error {
		out := s.state.Projects[:0]
		removed := false
		for _, p := range s.state.Projects {
			if strings.EqualFold(p.Name, name) {
				removed = true
				continue
			}
			out = append(out, p)
		}
		if !removed {
			return fmt.Errorf("project %q is not in the registry", name)
		}
		s.state.Projects = out
		return nil
	})
}

// UpsertGroup inserts or replaces a group.
func (s *Store) UpsertGroup(g Group) error {
	return s.update(func() error {
		for i, existing := range s.state.Groups {
			if strings.EqualFold(existing.Name, g.Name) {
				s.state.Groups[i] = g
				return nil
			}
		}
		s.state.Groups = append(s.state.Groups, g)
		return nil
	})
}

// GetGroup returns a group by name.
func (s *Store) GetGroup(name string) (Group, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, g := range s.state.Groups {
		if strings.EqualFold(g.Name, name) {
			return g, true
		}
	}
	return Group{}, false
}

// RemoveGroup deletes a group by name.
func (s *Store) RemoveGroup(name string) error {
	return s.update(func() error {
		out := s.state.Groups[:0]
		removed := false
		for _, g := range s.state.Groups {
			if strings.EqualFold(g.Name, name) {
				removed = true
				continue
			}
			out = append(out, g)
		}
		if !removed {
			return fmt.Errorf("group %q does not exist", name)
		}
		s.state.Groups = out
		return nil
	})
}
