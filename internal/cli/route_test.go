package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/antoniojosev/pm/internal/api"
	"github.com/antoniojosev/pm/internal/config"
	"github.com/antoniojosev/pm/internal/daemon"
	"github.com/antoniojosev/pm/internal/registry"
)

func TestRouteAddLsRm(t *testing.T) {
	isolate(t)

	out, _, err := run(t, "route", "add", "x.localhost", "9810")
	if err != nil || !strings.Contains(out, "x.localhost") || !strings.Contains(out, ":9810") {
		t.Fatalf("pm route add = %q, %v", out, err)
	}
	if _, _, err := run(t, "route", "add", "wt.localhost", "9811", "--owner", "wt-qa"); err != nil {
		t.Fatal(err)
	}

	// the registry file has them, so a second invocation sees them
	out, _, err = run(t, "route", "ls")
	if err != nil || !strings.Contains(out, "2 routes") || !strings.Contains(out, "x.localhost") ||
		!strings.Contains(out, ":9811") || !strings.Contains(out, "wt-qa") || !strings.Contains(out, "explicit") {
		t.Fatalf("pm route ls = %q, %v", out, err)
	}

	// the same route again is fine; another port for the host is not
	if _, _, err := run(t, "route", "add", "x.localhost", "9810"); err != nil {
		t.Fatalf("repeating pm route add: %v", err)
	}
	if _, _, err := run(t, "route", "add", "x.localhost", "9999"); err == nil || !strings.Contains(err.Error(), "9810") {
		t.Fatalf("a host routed elsewhere: %v", err)
	}

	out, _, err = run(t, "route", "rm", "x.localhost")
	if err != nil || !strings.Contains(out, "x.localhost") || !strings.Contains(out, "removed") {
		t.Fatalf("pm route rm = %q, %v", out, err)
	}
	if _, _, err := run(t, "route", "rm", "x.localhost"); err == nil {
		t.Fatal("removing twice must fail")
	}
	out, _, _ = run(t, "route", "ls")
	if strings.Contains(out, "x.localhost") || !strings.Contains(out, "wt.localhost") {
		t.Fatalf("pm route ls after rm = %q", out)
	}
}

func TestRouteJSON(t *testing.T) {
	isolate(t)
	out, _, err := run(t, "route", "add", "x.localhost", "9810", "--owner", "wt-qa", "--json")
	var r registry.Route
	if err != nil || json.Unmarshal([]byte(out), &r) != nil || r != (registry.Route{Host: "x.localhost", Port: 9810, Owner: "wt-qa"}) {
		t.Fatalf("route add --json = %q, %v", out, err)
	}
	out, _, err = run(t, "route", "ls", "--json")
	var all []api.RouteInfo
	if err != nil || json.Unmarshal([]byte(out), &all) != nil || len(all) != 1 || all[0].Source != "explicit" || all[0].Port != 9810 {
		t.Fatalf("route ls --json = %q, %v", out, err)
	}
}

func TestRouteEmptyList(t *testing.T) {
	isolate(t)
	out, _, err := run(t, "route", "ls")
	if err != nil || !strings.Contains(out, "no routes") {
		t.Fatalf("pm route ls = %q, %v", out, err)
	}
	out, _, _ = run(t, "route", "ls", "--json")
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("route ls --json on nothing = %q, want []", out)
	}
}

func TestRouteArguments(t *testing.T) {
	isolate(t)
	for name, args := range map[string][]string{
		"no port":       {"route", "add", "x.localhost"},
		"port not num":  {"route", "add", "x.localhost", "abc"},
		"port zero":     {"route", "add", "x.localhost", "0"},
		"bad host":      {"route", "add", "x.example.com", "80"},
		"reserved host": {"route", "add", "pm.localhost", "80"},
		"bad owner":     {"route", "add", "x.localhost", "80", "--owner", "a b"},
		"rm no host":    {"route", "rm"},
		"rm unknown":    {"route", "rm", "ghost.localhost"},
	} {
		if _, _, err := run(t, args...); err == nil {
			t.Errorf("%s: pm %v should fail", name, args)
		}
	}
}

func TestDownOwnerRemovesItsRoutes(t *testing.T) {
	isolateWithSystemd(t)
	for _, args := range [][]string{
		{"route", "add", "a.localhost", "9810", "--owner", "wt-qa"},
		{"route", "add", "b.localhost", "9811", "--owner", "wt-dev"},
	} {
		if _, _, err := run(t, args...); err != nil {
			t.Fatalf("pm %v: %v", args, err)
		}
	}
	// keeping the leases keeps the routes
	if _, _, err := run(t, "down", "--owner", "wt-qa", "--keep-leases"); err != nil {
		t.Fatal(err)
	}
	if out, _, _ := run(t, "route", "ls"); !strings.Contains(out, "a.localhost") {
		t.Fatalf("--keep-leases dropped the route:\n%s", out)
	}
	if _, _, err := run(t, "down", "--owner", "wt-qa"); err != nil {
		t.Fatal(err)
	}
	out, _, _ := run(t, "route", "ls")
	if strings.Contains(out, "a.localhost") || !strings.Contains(out, "b.localhost") {
		t.Fatalf("pm route ls after down --owner:\n%s", out)
	}
}

// The step's criterion, through the real CLI: pm route add x.localhost 9810
// ends up in the Caddyfile the daemon generates, and caddy is asked to reload.
func TestRouteAddReachesTheCaddyfile(t *testing.T) {
	_, sd := isolateWithSystemd(t)
	sd.Caddy(t)

	if _, _, err := run(t, "route", "add", "x.localhost", "9810"); err != nil {
		t.Fatal(err)
	}
	s, err := svc()
	if err != nil {
		t.Fatal(err)
	}
	daemon.New(s, config.DefaultDaemonPort).SyncProxy("")

	b, err := os.ReadFile(config.Resolve().Caddyfile)
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	if !strings.Contains(out, "http://x.localhost:80") || !strings.Contains(out, "reverse_proxy 127.0.0.1:9810") {
		t.Fatalf("x.localhost → 9810 is not in the Caddyfile:\n%s", out)
	}
	calls := sd.Calls("caddy")
	if len(calls) != 1 || calls[0][0] != "reload" {
		t.Fatalf("caddy calls = %v, want one reload", calls)
	}
}
