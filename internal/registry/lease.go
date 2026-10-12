package registry

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Lease reserves ports for an owner without starting anything. While the lease
// exists no other lease receives those ports and pm's own launches steer clear
// of them. It does not bind the ports: the owner's process does that later.
type Lease struct {
	// ID identifies the lease for release (DELETE /leases/{id}).
	ID string `json:"id"`
	// Owner is whoever asked: a worktree, a script, an agent. pm does not
	// interpret it beyond grouping and listing.
	Owner     string    `json:"owner"`
	Ports     []int     `json:"ports"`
	CreatedAt time.Time `json:"created_at"`
}

// MaxOwnerLen bounds an owner name.
const MaxOwnerLen = 64

// ownerRe keeps owners usable as a unit property, a label or a file name later
// on: no spaces, slashes, quotes or newlines.
var ownerRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidateOwner rejects owner names outside the documented grammar.
func ValidateOwner(owner string) error {
	if owner == "" {
		return fmt.Errorf("owner is required")
	}
	if len(owner) > MaxOwnerLen || !ownerRe.MatchString(owner) {
		return fmt.Errorf("invalid owner %q: use letters, digits, '.', '_' or '-', starting with a letter or digit, at most %d characters", owner, MaxOwnerLen)
	}
	return nil
}

// Leases returns a copy of every lease, oldest first.
func (s *Store) Leases() []Lease {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Lease, len(s.state.Leases))
	for i, l := range s.state.Leases {
		out[i] = copyLease(l)
	}
	return out
}

// LeasedPorts returns every port pm keeps for someone: those held by a lease
// and those a run was told to use.
func (s *Store) LeasedPorts() map[int]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reserved()
}

// reserved is every port held by a lease or declared by a run. Caller holds
// the lock.
func (s *Store) reserved() map[int]bool {
	out := leasedPorts(s.state.Leases)
	for _, p := range s.state.Projects {
		if p.Run == nil || p.Run.Released {
			continue
		}
		for _, port := range p.Run.Ports {
			out[port] = true
		}
	}
	return out
}

// GetLease returns a lease by id.
func (s *Store) GetLease(id string) (Lease, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, l := range s.state.Leases {
		if l.ID == id {
			return copyLease(l), true
		}
	}
	return Lease{}, false
}

// AddLease records a lease for owner. It calls pick with the ports already
// leased and stores whatever pick returns. The whole step (read leases, pick,
// write) runs under the store's lock, in-process and across processes, so two
// callers can never be handed the same port. pick must not call the Store.
func (s *Store) AddLease(owner string, pick func(leased map[int]bool) ([]int, error)) (Lease, error) {
	if err := ValidateOwner(owner); err != nil {
		return Lease{}, err
	}
	var lease Lease
	err := s.update(func() error {
		ports, err := pick(s.reserved())
		if err != nil {
			return err
		}
		if len(ports) == 0 {
			return fmt.Errorf("no ports to lease")
		}
		id, err := newLeaseID()
		if err != nil {
			return err
		}
		lease = Lease{ID: id, Owner: owner, Ports: ports, CreatedAt: time.Now().UTC().Truncate(time.Second)}
		s.state.Leases = append(s.state.Leases, lease)
		return nil
	})
	if err != nil {
		return Lease{}, err
	}
	return copyLease(lease), nil
}

// ReleaseLease deletes a lease by id and returns it.
func (s *Store) ReleaseLease(id string) (Lease, error) {
	var released Lease
	err := s.update(func() error {
		for i, l := range s.state.Leases {
			if l.ID == id {
				released = l
				s.state.Leases = slices.Delete(s.state.Leases, i, i+1)
				return nil
			}
		}
		return fmt.Errorf("lease %q does not exist", id)
	})
	return released, err
}

// ReleaseOwnerLeases deletes every lease of owner (case-insensitive) in one
// atomic step and returns them, oldest first. None is not an error.
func (s *Store) ReleaseOwnerLeases(owner string) ([]Lease, error) {
	var released []Lease
	err := s.update(func() error {
		released = nil
		kept := s.state.Leases[:0]
		for _, l := range s.state.Leases {
			if strings.EqualFold(l.Owner, owner) {
				released = append(released, copyLease(l))
				continue
			}
			kept = append(kept, l)
		}
		s.state.Leases = kept
		return nil
	})
	if err != nil {
		return nil, err
	}
	return released, nil
}

func leasedPorts(leases []Lease) map[int]bool {
	out := map[int]bool{}
	for _, l := range leases {
		for _, p := range l.Ports {
			out[p] = true
		}
	}
	return out
}

func copyLease(l Lease) Lease {
	l.Ports = slices.Clone(l.Ports)
	return l
}

// newLeaseID returns "l-" plus 12 hex digits.
func newLeaseID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "l-" + strings.ToLower(hex.EncodeToString(b[:])), nil
}
