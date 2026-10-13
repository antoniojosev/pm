package api

import (
	"sort"
	"strings"

	"github.com/antoniojosev/pm/internal/registry"
)

// Where a route comes from, as GET /routes reports it.
const (
	// RouteExplicit: declared with POST /routes (pm route add).
	RouteExplicit = "explicit"
	// RouteProject: derived from a running project's own name.
	RouteProject = "project"
)

// RouteInfo is one route Caddy serves.
type RouteInfo struct {
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Owner  string `json:"owner"`
	Source string `json:"source"` // explicit | project
}

// AddRoute publishes host for port. See registry.Store.AddRoute for what a
// repeat or a clash does.
func (s *Service) AddRoute(r registry.Route) (registry.Route, error) {
	stored, err := s.Store.AddRoute(r)
	if err != nil {
		return registry.Route{}, err
	}
	s.routeNotify.broadcast()
	return stored, nil
}

// RemoveRoute deletes the explicit route of host. A host that only exists as a
// project's name has no explicit route and is registry.ErrRouteNotFound: it
// goes away when the project stops.
func (s *Service) RemoveRoute(host string) (registry.Route, error) {
	r, err := s.Store.RemoveRoute(host)
	if err != nil {
		return registry.Route{}, err
	}
	s.routeNotify.broadcast()
	return r, nil
}

// ExplicitRoutes lists the explicit routes by host. It re-reads the registry
// first so routes added by another process (the CLI) show up.
func (s *Service) ExplicitRoutes() []registry.Route {
	_ = s.Store.Reload()
	return s.Store.Routes()
}

// RouteChanged returns a channel that closes the next time a route is added or
// removed through this Service. Fetch a fresh one after each wake-up. A change
// made by another process does not wake it: poll ExplicitRoutes for those.
func (s *Service) RouteChanged() <-chan struct{} { return s.routeNotify.wait() }

// Routes is every route to serve, sorted by host: the explicit ones plus one
// per running project. When both name the same host the explicit one wins and
// the project's is left out, so a host never has two upstreams.
func (s *Service) Routes() ([]RouteInfo, error) {
	rows, err := s.PS() // reloads the registry
	if err != nil {
		return nil, err
	}
	var out []RouteInfo
	taken := map[string]bool{}
	for _, r := range s.Store.Routes() {
		out = append(out, RouteInfo{Host: r.Host, Port: r.Port, Owner: r.Owner, Source: RouteExplicit})
		taken[r.Host] = true
	}
	for _, r := range rows {
		if r.Status != "up" || r.Port == 0 || r.Name == "?" {
			continue
		}
		host := s.HostFor(r.Name)
		if taken[host] {
			continue
		}
		out = append(out, RouteInfo{Host: host, Port: r.Port, Owner: r.Owner, Source: RouteProject})
		taken[host] = true
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, nil
}

// HostFor is the .localhost name a project answers to; for a name that is not
// registered, the name itself.
func (s *Service) HostFor(name string) string {
	if p, ok := s.Get(name); ok {
		return p.Hostname()
	}
	return strings.ToLower(name) + ".localhost"
}
