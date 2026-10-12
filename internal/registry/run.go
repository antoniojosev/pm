package registry

import (
	"fmt"
	"strings"
)

// DefaultOwner is the owner of a run that names none: whoever starts a process
// by hand from the CLI.
const DefaultOwner = "local"

// AddRun records p, an entry created by a run-by-specification (p.Run set).
// The whole step, checking and writing, runs under the store's lock, in-process
// and across processes, so two callers racing for one name or one port cannot
// both succeed. It fails when
//
//   - the name belongs to a registered project, or to a run for which
//     replaceable returns false (still alive). The record of a dead run is
//     overwritten.
//   - a port of the run is leased to another owner or declared by another run.
//     A port leased to the run's own owner is the point of leases, so it passes.
//
// replaceable runs under the lock: keep it quick.
func (s *Store) AddRun(p Project, replaceable func(existing Project) bool) error {
	if !p.IsRun() {
		return fmt.Errorf("%q is not a run entry", p.Name)
	}
	if err := p.Validate(); err != nil {
		return err
	}
	return s.update(func() error {
		replace := -1
		for i, existing := range s.state.Projects {
			if !strings.EqualFold(existing.Name, p.Name) {
				continue
			}
			if err := NameTaken(existing, replaceable(existing)); err != nil {
				return err
			}
			replace = i
		}
		for _, port := range p.Run.Ports {
			for _, l := range s.state.Leases {
				if !strings.EqualFold(l.Owner, p.Owner) && hasPort(l.Ports, port) {
					return fmt.Errorf("port %d is leased to %q (lease %s)", port, l.Owner, l.ID)
				}
			}
			for i, other := range s.state.Projects {
				if i != replace && other.IsRun() && hasPort(other.Run.Ports, port) {
					return fmt.Errorf("port %d is already used by %q", port, other.Name)
				}
			}
		}
		if replace >= 0 {
			s.state.Projects[replace] = p
			return nil
		}
		s.state.Projects = append(s.state.Projects, p)
		return nil
	})
}

// NameTaken says why a run cannot take the name of existing: nil when existing
// is a run that is no longer alive and may be replaced.
func NameTaken(existing Project, dead bool) error {
	switch {
	case !existing.IsRun():
		return fmt.Errorf("%q is a registered project: use pm start %s, or pick another name", existing.Name, existing.Name)
	case !dead:
		return fmt.Errorf("%q is already running: stop it first (pm stop %s) or pick another name", existing.Name, existing.Name)
	}
	return nil
}

func hasPort(ports []int, port int) bool {
	for _, p := range ports {
		if p == port {
			return true
		}
	}
	return false
}

// ValidateRunName rejects names outside the grammar of a run: the name ends up
// in the unit name (pm-<name>.service) and a log file name, so it follows the
// owner grammar.
func ValidateRunName(name string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if len(name) > MaxOwnerLen || !ownerRe.MatchString(name) {
		return fmt.Errorf("invalid name %q: use letters, digits, '.', '_' or '-', starting with a letter or digit, at most %d characters", name, MaxOwnerLen)
	}
	return nil
}
