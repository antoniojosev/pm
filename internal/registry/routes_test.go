package registry

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateHost(t *testing.T) {
	for _, ok := range []string{"x.localhost", "crb-144.localhost", "a.b.localhost", "X.LOCALHOST", " web.localhost "} {
		if err := ValidateHost(ok); err != nil {
			t.Errorf("ValidateHost(%q) = %v, want ok", ok, err)
		}
	}
	for _, bad := range []string{
		"", ".localhost", "localhost", "x", "x.example.com", "x.localhost.evil.com",
		"-x.localhost", "x-.localhost", "a..b.localhost", "x y.localhost", "x{.localhost",
		"x.localhost\nrespond 200", "x/y.localhost", "pm.localhost", "x_y.localhost",
		strings.Repeat("a", 64) + ".localhost",
	} {
		if err := ValidateHost(bad); err == nil {
			t.Errorf("ValidateHost(%q) accepted it", bad)
		}
	}
}

func TestAddRouteValidatesAndNormalises(t *testing.T) {
	s, _ := Open(tempPaths(t))

	r, err := s.AddRoute(Route{Host: "CRB-144.localhost", Port: 9810, Owner: "wt-qa"})
	if err != nil || r.Host != "crb-144.localhost" || r.Port != 9810 || r.Owner != "wt-qa" {
		t.Fatalf("AddRoute = %+v, %v", r, err)
	}
	for _, bad := range []Route{
		{Host: "x.localhost", Port: 0},
		{Host: "x.localhost", Port: 70000},
		{Host: "x.localhost", Port: -1},
		{Host: "x.localhost", Port: 80, Owner: "a b"},
		{Host: "x.test", Port: 80},
	} {
		if _, err := s.AddRoute(bad); err == nil {
			t.Errorf("AddRoute(%+v) accepted it", bad)
		}
	}
	if got := s.Routes(); len(got) != 1 {
		t.Fatalf("invalid routes were stored: %+v", got)
	}
}

func TestAddRouteIsRepeatableAndRefusesAnotherPort(t *testing.T) {
	paths := tempPaths(t)
	s, _ := Open(paths)
	if _, err := s.AddRoute(Route{Host: "x.localhost", Port: 9810}); err != nil {
		t.Fatal(err)
	}
	// same host and port: fine, and the owner is taken
	r, err := s.AddRoute(Route{Host: "x.localhost", Port: 9810, Owner: "wt-qa"})
	if err != nil || r.Owner != "wt-qa" {
		t.Fatalf("repeat = %+v, %v", r, err)
	}
	// without an owner the existing one stays
	r, err = s.AddRoute(Route{Host: "x.localhost", Port: 9810})
	if err != nil || r.Owner != "wt-qa" {
		t.Fatalf("repeat without owner = %+v, %v", r, err)
	}
	if n := len(s.Routes()); n != 1 {
		t.Fatalf("a repeat made %d routes", n)
	}
	// another port: conflict, nothing changes
	_, err = s.AddRoute(Route{Host: "X.localhost", Port: 9811})
	if !errors.Is(err, ErrRouteConflict) || !strings.Contains(err.Error(), "9810") {
		t.Fatalf("conflict err = %v", err)
	}
	if got := s.Routes(); got[0].Port != 9810 {
		t.Fatalf("a conflict changed the route: %+v", got)
	}

	// persisted: a fresh process sees it
	again, _ := Open(paths)
	if got := again.Routes(); len(got) != 1 || got[0].Host != "x.localhost" || got[0].Owner != "wt-qa" {
		t.Fatalf("reopened = %+v", got)
	}
}

func TestRemoveRoute(t *testing.T) {
	s, _ := Open(tempPaths(t))
	_, _ = s.AddRoute(Route{Host: "b.localhost", Port: 2})
	_, _ = s.AddRoute(Route{Host: "a.localhost", Port: 1})
	if got := s.Routes(); got[0].Host != "a.localhost" {
		t.Fatalf("routes must come sorted by host: %+v", got)
	}
	r, err := s.RemoveRoute("A.localhost")
	if err != nil || r.Host != "a.localhost" || r.Port != 1 {
		t.Fatalf("RemoveRoute = %+v, %v", r, err)
	}
	if _, err := s.RemoveRoute("a.localhost"); !errors.Is(err, ErrRouteNotFound) {
		t.Fatalf("removing twice: %v", err)
	}
	if got := s.Routes(); len(got) != 1 || got[0].Host != "b.localhost" {
		t.Fatalf("routes = %+v", got)
	}
}

func TestRemoveOwnerRoutes(t *testing.T) {
	s, _ := Open(tempPaths(t))
	_, _ = s.AddRoute(Route{Host: "a.localhost", Port: 1, Owner: "wt-qa"})
	_, _ = s.AddRoute(Route{Host: "b.localhost", Port: 2, Owner: "WT-QA"})
	_, _ = s.AddRoute(Route{Host: "c.localhost", Port: 3, Owner: "wt-dev"})
	_, _ = s.AddRoute(Route{Host: "d.localhost", Port: 4})

	got, err := s.RemoveOwnerRoutes("wt-qa")
	if err != nil || len(got) != 2 || got[0].Host != "a.localhost" || got[1].Host != "b.localhost" {
		t.Fatalf("RemoveOwnerRoutes = %+v, %v", got, err)
	}
	left := s.Routes()
	if len(left) != 2 || left[0].Host != "c.localhost" || left[1].Host != "d.localhost" {
		t.Fatalf("left = %+v", left)
	}
	// an owner with nothing is not an error, and "" never matches ownerless routes
	if got, err := s.RemoveOwnerRoutes("ghost"); err != nil || len(got) != 0 {
		t.Fatalf("ghost = %+v, %v", got, err)
	}
	if got, err := s.RemoveOwnerRoutes(""); err != nil || len(got) != 0 || len(s.Routes()) != 2 {
		t.Fatalf("empty owner removed ownerless routes: %+v, %v", got, err)
	}
}

// The CLI and the daemon share the file: a route one adds survives a write by
// the other.
func TestRoutesSurviveAnotherProcessWriting(t *testing.T) {
	paths := tempPaths(t)
	daemon, _ := Open(paths)
	cli, _ := Open(paths)
	if _, err := cli.AddRoute(Route{Host: "x.localhost", Port: 9810}); err != nil {
		t.Fatal(err)
	}
	if err := daemon.Upsert(Project{Name: "web"}); err != nil { // stale copy writes
		t.Fatal(err)
	}
	again, _ := Open(paths)
	if got := again.Routes(); len(got) != 1 {
		t.Fatalf("a stale writer erased the route: %+v", got)
	}
}
