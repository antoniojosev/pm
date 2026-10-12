package api

import (
	"fmt"
	"strings"
	"sync"

	"github.com/antoniojosev/pm/internal/registry"
)

const (
	// DefaultLeaseBase is where a lease with no preferred port starts looking:
	// above the ports dev servers ask for by default (3000, 5173, 8000, 8080)
	// and below the kernel's ephemeral range (32768+).
	DefaultLeaseBase = 20000
	// MaxLeaseCount caps how many ports one lease can hold.
	MaxLeaseCount = 100
	// leaseScanWindow is how many consecutive candidates a lease inspects
	// before giving up, counted from the preferred (or base) port.
	leaseScanWindow = 2000
)

// LeaseRequest is the body of POST /api/v1/leases.
type LeaseRequest struct {
	Owner string `json:"owner"`
	// Count is how many ports to reserve; 0 means one.
	Count int `json:"count"`
	// Prefer is the first port to try. Busy ports are skipped, so the lease
	// may hold higher ones. 0 starts at DefaultLeaseBase.
	Prefer int `json:"prefer"`
}

// CreateLease reserves ports for an owner. A port is handed out only when no
// listener holds it (the `ss` snapshot), nothing refuses a test bind (see
// ports.Free) and no other lease has it. The last check and the write that
// records the lease are one atomic step in the store, so concurrent requests,
// from the daemon or the CLI, never get the same port.
func (s *Service) CreateLease(req LeaseRequest) (registry.Lease, error) {
	if err := registry.ValidateOwner(req.Owner); err != nil {
		return registry.Lease{}, err
	}
	count := req.Count
	if count == 0 {
		count = 1
	}
	if count < 0 || count > MaxLeaseCount {
		return registry.Lease{}, fmt.Errorf("invalid count %d: ask for 1 to %d ports", req.Count, MaxLeaseCount)
	}
	if req.Prefer < 0 || req.Prefer > 65535 {
		return registry.Lease{}, fmt.Errorf("invalid prefer %d: use a port from 1 to 65535", req.Prefer)
	}
	start := req.Prefer
	if start == 0 {
		start = DefaultLeaseBase
	}

	// The slow probe (ss, docker) runs before the store lock. A listener that
	// appears after it is caught by the bind test inside the lock.
	live := s.Run.LivePorts()
	s.Run.ReleaseDeadRunPorts() // ports of runs that died are free again
	lease, err := s.Store.AddLease(req.Owner, func(leased map[int]bool) ([]int, error) {
		var got []int
		for p := start; p < start+leaseScanWindow && p <= 65535 && len(got) < count; p++ {
			if leased[p] || live[p] || !s.PortFree(p) {
				continue
			}
			got = append(got, p)
		}
		if len(got) < count {
			return nil, fmt.Errorf("could not find %d free ports from %d (found %d in the next %d)", count, start, len(got), leaseScanWindow)
		}
		return got, nil
	})
	if err != nil {
		return registry.Lease{}, err
	}
	s.leaseNotify.broadcast()
	return lease, nil
}

// Leases lists the leases, oldest first, optionally only those of one owner.
// It re-reads the registry first so leases taken by another process show up.
func (s *Service) Leases(owner string) []registry.Lease {
	_ = s.Store.Reload()
	all := s.Store.Leases()
	if owner == "" {
		return all
	}
	out := []registry.Lease{}
	for _, l := range all {
		if strings.EqualFold(l.Owner, owner) {
			out = append(out, l)
		}
	}
	return out
}

// ReleaseLease frees a lease's ports and returns the lease.
func (s *Service) ReleaseLease(id string) (registry.Lease, error) {
	l, err := s.Store.ReleaseLease(id)
	if err != nil {
		return registry.Lease{}, err
	}
	s.leaseNotify.broadcast()
	return l, nil
}

// LeaseChanged returns a channel that closes the next time a lease is created
// or released through this Service. Fetch a fresh one after each wake-up. A
// change made by another process does not wake it: poll Leases for those.
func (s *Service) LeaseChanged() <-chan struct{} { return s.leaseNotify.wait() }

// notifier is a broadcast: every waiter holding the current channel is woken
// by the next broadcast, then fetches the replacement.
type notifier struct {
	mu sync.Mutex
	ch chan struct{}
}

func (n *notifier) wait() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ch == nil {
		n.ch = make(chan struct{})
	}
	return n.ch
}

func (n *notifier) broadcast() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ch != nil {
		close(n.ch)
		n.ch = nil
	}
}
