package api

import (
	"errors"
	"testing"

	"github.com/antoniojosev/pm/internal/registry"
)

func routeByHost(routes []RouteInfo, host string) (RouteInfo, bool) {
	for _, r := range routes {
		if r.Host == host {
			return r, true
		}
	}
	return RouteInfo{}, false
}

// The step's rule: a route declared by hand beats the one a project derives
// for the same host, and a host never appears twice.
func TestExplicitRouteWinsOverTheProjectsOwn(t *testing.T) {
	svc, _ := runService(t)
	if _, err := svc.StartRun(runSpec(t, "web", 4100)); err != nil {
		t.Fatal(err)
	}

	routes, err := svc.Routes()
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := routeByHost(routes, "web.localhost"); !ok || r.Source != RouteProject || r.Port != 4100 {
		t.Fatalf("derived route = %+v, %v in %+v", r, ok, routes)
	}

	if _, err := svc.AddRoute(registry.Route{Host: "web.localhost", Port: 9810, Owner: "wt-qa"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddRoute(registry.Route{Host: "tunnel.localhost", Port: 9811}); err != nil {
		t.Fatal(err)
	}
	routes, err = svc.Routes()
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 {
		t.Fatalf("routes = %+v, want web (explicit) and tunnel only", routes)
	}
	if r, _ := routeByHost(routes, "web.localhost"); r.Source != RouteExplicit || r.Port != 9810 || r.Owner != "wt-qa" {
		t.Fatalf("web.localhost = %+v, want the explicit route", r)
	}
	if routes[0].Host != "tunnel.localhost" {
		t.Fatalf("routes must be sorted by host: %+v", routes)
	}

	// removing the explicit one gives the host back to the project
	if _, err := svc.RemoveRoute("web.localhost"); err != nil {
		t.Fatal(err)
	}
	routes, _ = svc.Routes()
	if r, _ := routeByHost(routes, "web.localhost"); r.Source != RouteProject || r.Port != 4100 {
		t.Fatalf("after removal = %+v", r)
	}
	// and the derived route cannot be removed as if it were explicit
	if _, err := svc.RemoveRoute("web.localhost"); !errors.Is(err, registry.ErrRouteNotFound) {
		t.Fatalf("removing a derived route: %v", err)
	}
}

func TestDeletingAnOwnerDeletesItsRoutes(t *testing.T) {
	svc, _ := runService(t)
	_, _ = svc.AddRoute(registry.Route{Host: "a.localhost", Port: 1, Owner: "wt-qa"})
	_, _ = svc.AddRoute(registry.Route{Host: "b.localhost", Port: 2, Owner: "wt-dev"})
	_, _ = svc.AddRoute(registry.Route{Host: "c.localhost", Port: 3})
	events, cancel := svc.SubscribeOwnerEvents()
	defer cancel()

	// a stop keeps them, like the leases
	if _, err := svc.StopOwner("wt-qa"); err != nil {
		t.Fatal(err)
	}
	if got := svc.ExplicitRoutes(); len(got) != 3 {
		t.Fatalf("a stop removed routes: %+v", got)
	}

	// a route is something to clean up even when nothing else was running
	if _, err := svc.DeleteOwner("wt-qa"); err != nil {
		t.Fatal(err)
	}
	got := svc.ExplicitRoutes()
	if len(got) != 2 || got[0].Host != "b.localhost" || got[1].Host != "c.localhost" {
		t.Fatalf("routes after delete = %+v", got)
	}
	select {
	case ev := <-events:
		if ev.Name != EventOwnerDeleted || ev.Result.Owner != "wt-qa" {
			t.Fatalf("event = %+v", ev)
		}
	default:
		t.Fatal("deleting an owner that only had routes published no owner.deleted")
	}
	// repeatable
	if _, err := svc.DeleteOwner("wt-qa"); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteOwnerKeepsRoutesWhenSomethingWontStop(t *testing.T) {
	svc, sd := runService(t)
	startOwned(t, svc, "wt-qa")
	_, _ = svc.AddRoute(registry.Route{Host: "a.localhost", Port: 1, Owner: "wt-qa"})
	sd.FailStop(t)

	if _, err := svc.DeleteOwner("wt-qa"); err == nil {
		t.Fatal("expected the failed stop to be reported")
	}
	if got := svc.ExplicitRoutes(); len(got) != 1 {
		t.Fatalf("routes = %+v, want kept", got)
	}
}

func TestRouteChangedWakesOnAddAndRemove(t *testing.T) {
	svc := newTestService(t)
	wake := svc.RouteChanged()
	if _, err := svc.AddRoute(registry.Route{Host: "a.localhost", Port: 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("AddRoute did not wake")
	}
	wake = svc.RouteChanged()
	if _, err := svc.RemoveRoute("a.localhost"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("RemoveRoute did not wake")
	}
	// a failure wakes nobody
	wake = svc.RouteChanged()
	_, _ = svc.RemoveRoute("a.localhost")
	select {
	case <-wake:
		t.Fatal("a failed removal woke")
	default:
	}
}
