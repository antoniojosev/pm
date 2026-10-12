package registry

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
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
				if i != replace && other.IsRun() && !other.Run.Released && hasPort(other.Run.Ports, port) {
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

// NewRunNonce returns the identifier of a run record: 16 hex digits.
func NewRunNonce() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on the platforms pm runs on
	}
	return hex.EncodeToString(b[:])
}

// updateRun applies fn to the run record called name whose nonce is nonce, under
// the store's lock. It reports whether such a record was there: a record of a
// later run of the same name (another nonce), or none at all, is left alone.
func (s *Store) updateRun(name, nonce string, fn func(i int)) (bool, error) {
	found := false
	err := s.update(func() error {
		for i, p := range s.state.Projects {
			if strings.EqualFold(p.Name, name) && p.IsRun() && p.Run.Nonce == nonce {
				found = true
				fn(i)
				return nil
			}
		}
		return nil
	})
	return found, err
}

// FinishLaunch marks the run record called name, with that nonce, as started:
// from now on only its unit says whether it lives.
func (s *Store) FinishLaunch(name, nonce string) error {
	_, err := s.updateRun(name, nonce, func(i int) { s.state.Projects[i].Run.Launching = false })
	return err
}

// RemoveRun deletes the run record called name only when it is the one with
// that nonce. It is how a failed launch takes back its own record without
// touching the one another launch of the same name has written.
func (s *Store) RemoveRun(name, nonce string) error {
	_, err := s.updateRun(name, nonce, func(i int) {
		s.state.Projects = slices.Delete(s.state.Projects, i, i+1)
	})
	return err
}

// ReleaseRunPorts lets go of the ports of the run record called name with that
// nonce: it died, so they stop being reserved. The record stays.
func (s *Store) ReleaseRunPorts(name, nonce string) error {
	_, err := s.updateRun(name, nonce, func(i int) { s.state.Projects[i].Run.Released = true })
	return err
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
