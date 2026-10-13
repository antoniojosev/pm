package api

import (
	"fmt"
	"strings"
	"sync"

	"github.com/antoniojosev/pm/internal/registry"
	"github.com/antoniojosev/pm/internal/runner"
)

// Names of the events a Service publishes about owners (see
// SubscribeOwnerEvents). The daemon relays them on /api/v1/events.
const (
	// EventOwnerStopped: an owner's processes were stopped; its leases stay.
	EventOwnerStopped = "owner.stopped"
	// EventOwnerDeleted: an owner's processes were stopped and its leases released.
	EventOwnerDeleted = "owner.deleted"
)

// OwnerResult is what stopping or deleting an owner did.
type OwnerResult struct {
	Owner string `json:"owner"`
	// Stopped is every process brought down: projects, runs (forgotten once
	// stopped), and processes that only carry the PM_OWNER marker.
	Stopped []runner.Stopped `json:"stopped"`
	// LeasesReleased are the ids of the leases released, always empty for a
	// stop that keeps them.
	LeasesReleased []string `json:"leases_released"`
	// Errors are the things that could not be stopped, one message each.
	Errors []string `json:"errors,omitempty"`
}

// OwnerEvent is published when an owner is stopped or deleted through a Service.
type OwnerEvent struct {
	Name   string // EventOwnerStopped | EventOwnerDeleted
	Result OwnerResult
}

// StopOwner stops everything that belongs to owner and keeps its leases, so the
// owner can start again on the same ports. An owner with nothing running is not
// an error: the result is simply empty. When some process cannot be stopped the
// rest still are, and the error lists the failures (the result is complete
// either way).
func (s *Service) StopOwner(owner string) (OwnerResult, error) {
	res, err := s.stopOwner(owner)
	if err != nil {
		s.publishOwner(EventOwnerStopped, res)
		return res, err
	}
	if len(res.Stopped) > 0 {
		s.publishOwner(EventOwnerStopped, res)
	}
	return res, nil
}

// DeleteOwner stops everything that belongs to owner and then releases its
// leases and removes its explicit routes: afterwards `pm ps`, `pm lease ls`
// and `pm route ls` show nothing of it. Registered
// projects that name the owner stay in the catalog, stopped, as after pm stop.
// If something could not be stopped the leases and routes are kept (its ports may still
// be in use) and the error says so: DeleteOwner can be repeated.
func (s *Service) DeleteOwner(owner string) (OwnerResult, error) {
	res, err := s.stopOwner(owner)
	if err != nil {
		s.publishOwner(EventOwnerStopped, res)
		return res, fmt.Errorf("%w; its leases were kept", err)
	}
	released, rerr := s.Store.ReleaseOwnerLeases(owner)
	for _, l := range released {
		res.LeasesReleased = append(res.LeasesReleased, l.ID)
	}
	if len(released) > 0 {
		s.leaseNotify.broadcast()
	}
	if rerr != nil {
		res.Errors = append(res.Errors, "release leases: "+rerr.Error())
		s.publishOwner(EventOwnerStopped, res)
		return res, rerr
	}
	// Its explicit routes go with it: they name ports that belonged to it.
	routes, routeErr := s.Store.RemoveOwnerRoutes(owner)
	if len(routes) > 0 {
		s.routeNotify.broadcast()
	}
	if routeErr != nil {
		res.Errors = append(res.Errors, "remove routes: "+routeErr.Error())
		s.publishOwner(EventOwnerStopped, res)
		return res, routeErr
	}
	if len(res.Stopped) > 0 || len(res.LeasesReleased) > 0 || len(routes) > 0 {
		s.publishOwner(EventOwnerDeleted, res)
	}
	return res, nil
}

func (s *Service) stopOwner(owner string) (OwnerResult, error) {
	res := OwnerResult{Owner: owner, Stopped: []runner.Stopped{}, LeasesReleased: []string{}}
	if err := registry.ValidateOwner(owner); err != nil {
		return res, err
	}
	stopped, errs := s.Run.StopOwner(owner)
	if stopped != nil {
		res.Stopped = stopped
	}
	for _, e := range errs {
		res.Errors = append(res.Errors, e.Error())
	}
	if len(errs) > 0 {
		return res, fmt.Errorf("owner %s: could not stop everything: %s", owner, strings.Join(res.Errors, "; "))
	}
	return res, nil
}

// SubscribeOwnerEvents delivers the owner events this Service publishes from
// now on, until cancel is called. A subscriber that falls behind misses events
// rather than holding up the caller. An action performed by another process
// (`pm down --owner` from the CLI) publishes nothing here.
func (s *Service) SubscribeOwnerEvents() (events <-chan OwnerEvent, cancel func()) {
	return s.ownerHub.subscribe()
}

func (s *Service) publishOwner(name string, res OwnerResult) {
	s.ownerHub.publish(OwnerEvent{Name: name, Result: res})
}

// ownerHub fans owner events out to subscribers.
type ownerHub struct {
	mu   sync.Mutex
	subs map[chan OwnerEvent]struct{}
}

func (h *ownerHub) subscribe() (<-chan OwnerEvent, func()) {
	ch := make(chan OwnerEvent, 16)
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[chan OwnerEvent]struct{}{}
	}
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

func (h *ownerHub) publish(ev OwnerEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- ev:
		default: // a slow consumer must not block a stop
		}
	}
}
