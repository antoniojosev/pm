package registry

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Route publishes a name for a port pm did not start: Caddy sends Host to
// 127.0.0.1:Port. It is explicit, as opposed to the routes pm derives from the
// projects it knows, and it persists until it is removed (or its owner is).
type Route struct {
	// Host is the full name, always <label>.localhost, lowercase.
	Host string `json:"host"`
	Port int    `json:"port"`
	// Owner is whoever asked: a worktree, a script, an agent. Empty when the
	// route belongs to nobody. Deleting the owner deletes the route.
	Owner string `json:"owner"`
}

var (
	// ErrRouteConflict is returned when a host already routes to another port.
	ErrRouteConflict = errors.New("route conflict")
	// ErrRouteNotFound is returned when no explicit route has the host.
	ErrRouteNotFound = errors.New("route not found")
)

// ReservedHost is the name of the dashboard, which no route can take.
const ReservedHost = "pm.localhost"

const hostSuffix = ".localhost"

// maxHostLen is the longest name DNS allows.
const maxHostLen = 253

// hostLabelRe is one DNS label: letters, digits and inner hyphens, at most 63.
var hostLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// NormalizeHost lowercases and trims host, the form routes are stored and
// compared in.
func NormalizeHost(host string) string { return strings.ToLower(strings.TrimSpace(host)) }

// ValidateHost rejects a host that is not <label>[.<label>...].localhost. The
// value ends up in a Caddyfile, so anything outside the DNS grammar (a space,
// a brace, a newline) must never get that far.
func ValidateHost(host string) error {
	h := NormalizeHost(host)
	if h == "" {
		return fmt.Errorf("host is required")
	}
	prefix, ok := strings.CutSuffix(h, hostSuffix)
	if !ok || prefix == "" || len(h) > maxHostLen {
		return fmt.Errorf("invalid host %q: use <name>%s, with DNS labels of letters, digits and '-'", host, hostSuffix)
	}
	for label := range strings.SplitSeq(prefix, ".") {
		if !hostLabelRe.MatchString(label) {
			return fmt.Errorf("invalid host %q: use <name>%s, with DNS labels of letters, digits and '-'", host, hostSuffix)
		}
	}
	if h == ReservedHost {
		return fmt.Errorf("host %q is reserved for the pm dashboard", h)
	}
	return nil
}

// Validate checks a route and returns it in stored form: host lowercased.
func (r Route) Validate() (Route, error) {
	if err := ValidateHost(r.Host); err != nil {
		return Route{}, err
	}
	r.Host = NormalizeHost(r.Host)
	if r.Port < 1 || r.Port > 65535 {
		return Route{}, fmt.Errorf("invalid port %d: use a port from 1 to 65535", r.Port)
	}
	if r.Owner != "" {
		if err := ValidateOwner(r.Owner); err != nil {
			return Route{}, err
		}
	}
	return r, nil
}

// Routes returns a copy of every explicit route, sorted by host.
func (s *Store) Routes() []Route {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := slices.Clone(s.state.Routes)
	slices.SortFunc(out, func(a, b Route) int { return strings.Compare(a.Host, b.Host) })
	return out
}

// AddRoute records a route. A host that already routes to the same port is not
// an error: the call is repeatable, and a non-empty owner replaces the old
// one. A host that routes to another port is ErrRouteConflict: the caller must
// remove the old route first, so one never silently takes another's name.
func (s *Store) AddRoute(r Route) (Route, error) {
	r, err := r.Validate()
	if err != nil {
		return Route{}, err
	}
	var stored Route
	err = s.update(func() error {
		for i, existing := range s.state.Routes {
			if existing.Host != r.Host {
				continue
			}
			if existing.Port != r.Port {
				return fmt.Errorf("%w: %s already routes to port %d", ErrRouteConflict, r.Host, existing.Port)
			}
			if r.Owner != "" {
				s.state.Routes[i].Owner = r.Owner
			}
			stored = s.state.Routes[i]
			return nil
		}
		s.state.Routes = append(s.state.Routes, r)
		stored = r
		return nil
	})
	if err != nil {
		return Route{}, err
	}
	return stored, nil
}

// RemoveRoute deletes the explicit route of host and returns it.
func (s *Store) RemoveRoute(host string) (Route, error) {
	host = NormalizeHost(host)
	var removed Route
	err := s.update(func() error {
		for i, r := range s.state.Routes {
			if r.Host == host {
				removed = r
				s.state.Routes = slices.Delete(s.state.Routes, i, i+1)
				return nil
			}
		}
		return fmt.Errorf("%w: no explicit route for %q", ErrRouteNotFound, host)
	})
	return removed, err
}

// RemoveOwnerRoutes deletes every route of owner (case-insensitive) in one
// atomic step and returns them, sorted by host. None is not an error.
func (s *Store) RemoveOwnerRoutes(owner string) ([]Route, error) {
	var removed []Route
	err := s.update(func() error {
		removed = nil
		kept := s.state.Routes[:0]
		for _, r := range s.state.Routes {
			if r.Owner != "" && strings.EqualFold(r.Owner, owner) {
				removed = append(removed, r)
				continue
			}
			kept = append(kept, r)
		}
		s.state.Routes = kept
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(removed, func(a, b Route) int { return strings.Compare(a.Host, b.Host) })
	return removed, nil
}
