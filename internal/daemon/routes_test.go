package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/pm/internal/registry"
	"github.com/antoniojosev/pm/internal/testfake"
)

// doStatus is do for answers that may have no body (204).
func doStatus(t *testing.T, srv *httptest.Server, method, path, body string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(raw)
}

func TestRoutesOverHTTP(t *testing.T) {
	_, srv := newTestDaemon(t)

	// empty at first, and an array, not null
	code, _, raw := do(t, srv, "GET", "/api/v1/routes", "")
	if code != 200 || strings.TrimSpace(raw) != "[]" {
		t.Fatalf("GET /routes = %d %s", code, raw)
	}

	code, obj, raw := do(t, srv, "POST", "/api/v1/routes", `{"host":"Crb-144.localhost","port":9810,"owner":"wt-qa"}`)
	if code != 201 || obj["host"] != "crb-144.localhost" || obj["port"] != float64(9810) || obj["owner"] != "wt-qa" {
		t.Fatalf("POST /routes = %d %s", code, raw)
	}
	assertConformsStatus(t, "POST", "/routes", "201", raw)

	// without an owner the field is still there
	code, obj, raw = do(t, srv, "POST", "/api/v1/routes", `{"host":"x.localhost","port":9811}`)
	if code != 201 || obj["owner"] != "" {
		t.Fatalf("POST /routes without owner = %d %s", code, raw)
	}
	assertConformsStatus(t, "POST", "/routes", "201", raw)

	// repeating it is fine
	if code, _, raw := do(t, srv, "POST", "/api/v1/routes", `{"host":"x.localhost","port":9811}`); code != 201 {
		t.Fatalf("repeat = %d %s", code, raw)
	}

	_, _, raw = do(t, srv, "GET", "/api/v1/routes", "")
	assertConforms(t, "GET", "/routes", raw)
	var list []map[string]any
	if err := json.Unmarshal([]byte(raw), &list); err != nil || len(list) != 2 {
		t.Fatalf("GET /routes = %s (%v)", raw, err)
	}
	if list[0]["host"] != "crb-144.localhost" || list[0]["source"] != "explicit" || list[1]["host"] != "x.localhost" {
		t.Fatalf("GET /routes = %s", raw)
	}

	// the legacy alias serves it too
	if code, _, _ := do(t, srv, "GET", "/api/routes", ""); code != 200 {
		t.Fatalf("GET /api/routes = %d", code)
	}

	// delete: no body; the host is matched case-insensitively
	code, raw2 := doStatus(t, srv, "DELETE", "/api/v1/routes/X.localhost", "")
	if code != 204 || raw2 != "" {
		t.Fatalf("DELETE = %d %q", code, raw2)
	}
	_, _, raw = do(t, srv, "GET", "/api/v1/routes", "")
	if strings.Contains(raw, `"x.localhost"`) || !strings.Contains(raw, "crb-144.localhost") {
		t.Fatalf("after delete: %s", raw)
	}
}

func TestRouteErrors(t *testing.T) {
	_, srv := newTestDaemon(t)
	do(t, srv, "POST", "/api/v1/routes", `{"host":"x.localhost","port":9810}`)

	// a host that already routes elsewhere is a 409 with the usual error body
	code, obj, raw := do(t, srv, "POST", "/api/v1/routes", `{"host":"x.localhost","port":9811}`)
	if code != 409 || !strings.Contains(obj["error"].(string), "9810") {
		t.Fatalf("conflict = %d %s", code, raw)
	}
	assertConformsStatus(t, "POST", "/routes", "409", raw)

	for name, body := range map[string]string{
		"bad host":      `{"host":"x.example.com","port":1}`,
		"reserved host": `{"host":"pm.localhost","port":1}`,
		"no port":       `{"host":"y.localhost"}`,
		"port too big":  `{"host":"y.localhost","port":65536}`,
		"bad owner":     `{"host":"y.localhost","port":1,"owner":"a b"}`,
		"unknown field": `{"host":"y.localhost","port":1,"ownr":"wt-qa"}`,
		"not json":      `nope`,
	} {
		code, obj, raw := do(t, srv, "POST", "/api/v1/routes", body)
		if code != 400 || obj["error"] == nil {
			t.Errorf("%s: %d %s, want 400 with an error", name, code, raw)
		}
	}

	code, raw = doStatus(t, srv, "DELETE", "/api/v1/routes/ghost.localhost", "")
	if code != 404 || !strings.Contains(raw, `"error"`) {
		t.Fatalf("delete unknown = %d %s", code, raw)
	}
	assertConformsStatus(t, "DELETE", "/routes/{host}", "404", raw)
}

func TestDeletingAnOwnerDeletesItsRoutesOverHTTP(t *testing.T) {
	_, srv := newTestDaemon(t)
	testfake.Install(t)
	do(t, srv, "POST", "/api/v1/routes", `{"host":"a.localhost","port":1,"owner":"wt-qa"}`)
	do(t, srv, "POST", "/api/v1/routes", `{"host":"b.localhost","port":2,"owner":"wt-dev"}`)

	if code, _, raw := do(t, srv, "DELETE", "/api/v1/owners/wt-qa", ""); code != 200 {
		t.Fatalf("delete owner = %d %s", code, raw)
	}
	_, _, raw := do(t, srv, "GET", "/api/v1/routes", "")
	if strings.Contains(raw, "a.localhost") || !strings.Contains(raw, "b.localhost") {
		t.Fatalf("routes after owner delete = %s", raw)
	}
}

// nextRouteEvent skips ps and lease frames until a route.* frame arrives.
func nextRouteEvent(t *testing.T, r *bufio.Reader) (string, registry.Route) {
	t.Helper()
	for {
		name, data := readEvent(t, r)
		if strings.HasPrefix(name, "route.") {
			var route registry.Route
			if err := json.Unmarshal([]byte(data), &route); err != nil {
				t.Fatalf("not a route: %s (%v)", data, err)
			}
			return name, route
		}
	}
}

func TestEventsReportRoutes(t *testing.T) {
	d, srv := newTestDaemon(t)
	testfake.Install(t)
	if _, err := d.svc.AddRoute(registry.Route{Host: "old.localhost", Port: 1}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/events", nil)
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	r := bufio.NewReader(res.Body)
	readEvent(t, r) // ps: the client is subscribed

	// through the daemon: at once, and the route that existed before is not replayed
	t0 := time.Now()
	do(t, srv, "POST", "/api/v1/routes", `{"host":"x.localhost","port":9810,"owner":"wt-qa"}`)
	name, got := nextRouteEvent(t, r)
	if name != "route.created" || got.Host != "x.localhost" || got.Port != 9810 || got.Owner != "wt-qa" {
		t.Fatalf("event = %s %+v", name, got)
	}
	if d := time.Since(t0); d > 1500*time.Millisecond {
		t.Errorf("route.created took %v: it should not wait for the poll", d)
	}

	// the same call again changes nothing and says nothing; another owner is
	// an update of the route, not a new one
	do(t, srv, "POST", "/api/v1/routes", `{"host":"x.localhost","port":9810,"owner":"wt-qa"}`)
	do(t, srv, "POST", "/api/v1/routes", `{"host":"x.localhost","port":9810,"owner":"wt-dev"}`)
	name, got = nextRouteEvent(t, r)
	if name != "route.updated" || got.Host != "x.localhost" || got.Port != 9810 || got.Owner != "wt-dev" {
		t.Fatalf("event = %s %+v", name, got)
	}

	// deleting the owner removes the route and says so
	do(t, srv, "DELETE", "/api/v1/owners/wt-dev", "")
	name, got = nextRouteEvent(t, r)
	if name != "route.deleted" || got.Host != "x.localhost" {
		t.Fatalf("event = %s %+v", name, got)
	}

	doStatus(t, srv, "DELETE", "/api/v1/routes/old.localhost", "")
	name, got = nextRouteEvent(t, r)
	if name != "route.deleted" || got.Host != "old.localhost" {
		t.Fatalf("event = %s %+v", name, got)
	}

	// made by another process (the CLI): noticed by the poll
	other, err := registry.Open(d.svc.Paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.AddRoute(registry.Route{Host: "cli.localhost", Port: 5}); err != nil {
		t.Fatal(err)
	}
	name, got = nextRouteEvent(t, r)
	if name != "route.created" || got.Host != "cli.localhost" {
		t.Fatalf("event for a CLI route = %s %+v", name, got)
	}
	if _, err := other.RemoveRoute("cli.localhost"); err != nil {
		t.Fatal(err)
	}
	name, got = nextRouteEvent(t, r)
	if name != "route.deleted" || got.Host != "cli.localhost" {
		t.Fatalf("event for a CLI removal = %s %+v", name, got)
	}
}

func TestDiffExplicitRoutes(t *testing.T) {
	a := registry.Route{Host: "a.localhost", Port: 1}
	b := registry.Route{Host: "b.localhost", Port: 2}
	c := registry.Route{Host: "c.localhost", Port: 3}
	b2 := registry.Route{Host: "b.localhost", Port: 22}
	by := func(rs ...registry.Route) map[string]registry.Route { return routesByHost(rs) }

	evs := diffExplicitRoutes(by(a, b), by(b, c))
	if len(evs) != 2 || evs[0].Name != "route.deleted" || evs[0].Route.Host != "a.localhost" ||
		evs[1].Name != "route.created" || evs[1].Route.Host != "c.localhost" {
		t.Fatalf("events = %+v", evs)
	}
	// a host that moved to another port reads as the old route going, the new arriving
	evs = diffExplicitRoutes(by(b), by(b2))
	if len(evs) != 2 || evs[0].Name != "route.deleted" || evs[0].Route.Port != 2 ||
		evs[1].Name != "route.created" || evs[1].Route.Port != 22 {
		t.Fatalf("port change events = %+v", evs)
	}
	if evs := diffExplicitRoutes(by(a), by(a)); len(evs) != 0 {
		t.Fatalf("no change produced %+v", evs)
	}
	// same host and port with another owner is one route.updated, as it is now
	a2 := registry.Route{Host: "a.localhost", Port: 1, Owner: "wt-qa"}
	evs = diffExplicitRoutes(by(a), by(a2))
	if len(evs) != 1 || evs[0].Name != "route.updated" || evs[0].Route != a2 {
		t.Fatalf("owner change events = %+v", evs)
	}
}

// A reload that fails is not remembered as done: the next round writes and
// reloads again until Caddy accepts it, instead of leaving the route unserved
// until some other route changes.
func TestSyncProxyRetriesAFailedReload(t *testing.T) {
	d, srv := newTestDaemon(t)
	sd := testfake.Install(t)
	sd.Caddy(t)
	last := d.SyncProxy("")
	reloads := func() int { return len(sd.Calls("caddy")) }

	sd.FailCaddy(t)
	if code, _, raw := do(t, srv, "POST", "/api/v1/routes", `{"host":"x.localhost","port":9810}`); code != 201 {
		t.Fatalf("POST /routes = %d %s", code, raw)
	}
	failed := d.SyncProxy(last)
	if failed == last || failed != "" {
		t.Fatalf("a failed reload was taken as done: %q", failed)
	}
	if reloads() != 2 {
		t.Fatalf("caddy calls = %d, want the failed reload", reloads())
	}
	if d.SyncProxy(failed) != "" || reloads() != 3 {
		t.Fatalf("the next round did not retry (%d calls)", reloads())
	}

	sd.FixCaddy(t)
	last = d.SyncProxy(failed)
	if last == failed || reloads() != 4 {
		t.Fatalf("the retry did not go through (%d calls)", reloads())
	}
	if !strings.Contains(caddyfile(t, d), "reverse_proxy 127.0.0.1:9810") {
		t.Fatalf("route missing:\n%s", caddyfile(t, d))
	}
	// now it is in force: nothing more to do
	if d.SyncProxy(last) != last || reloads() != 4 {
		t.Fatalf("an applied sync reloaded again (%d calls)", reloads())
	}
}

// A route removed while Caddy is down must not survive in the Caddyfile: the
// file written by the failed round holds it, and pm-proxy reads that file when
// it starts again.
func TestSyncProxyDropsARouteRemovedDuringAnOutage(t *testing.T) {
	d, srv := newTestDaemon(t)
	sd := testfake.Install(t)
	sd.Caddy(t)
	last := d.SyncProxy("")

	sd.FailCaddy(t)
	if code, _, raw := do(t, srv, "POST", "/api/v1/routes", `{"host":"x.localhost","port":9810}`); code != 201 {
		t.Fatalf("POST /routes = %d %s", code, raw)
	}
	failed := d.SyncProxy(last)
	if !strings.Contains(caddyfile(t, d), "9810") {
		t.Fatalf("the failed round should have written the route:\n%s", caddyfile(t, d))
	}
	if code, raw := doStatus(t, srv, "DELETE", "/api/v1/routes/x.localhost", ""); code != 204 {
		t.Fatalf("DELETE /routes = %d %s", code, raw)
	}

	sd.FixCaddy(t)
	now := d.SyncProxy(failed)
	if strings.Contains(caddyfile(t, d), "9810") {
		t.Fatalf("the removed route is still in the Caddyfile:\n%s", caddyfile(t, d))
	}
	if now == "" || now == failed {
		t.Fatalf("the recovery round was not taken as applied: %q", now)
	}
	if d.SyncProxy(now) != now {
		t.Fatal("an applied sync was repeated")
	}
}

// caddyfile reads what SyncProxy wrote.
func caddyfile(t *testing.T, d *Daemon) string {
	t.Helper()
	b, err := os.ReadFile(d.svc.Paths.Caddyfile)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The step's criterion: a route added by hand ends up in the Caddyfile and
// Caddy is told to reload it. caddy here is a fake that records its calls.
func TestExplicitRouteReachesTheCaddyfile(t *testing.T) {
	d, srv := newTestDaemon(t)
	sd := testfake.Install(t)
	sd.Caddy(t)

	last := d.SyncProxy("")
	if strings.Contains(caddyfile(t, d), "x.localhost") {
		t.Fatalf("a route nobody added is in the Caddyfile:\n%s", caddyfile(t, d))
	}
	reloads := func() int { return len(sd.Calls("caddy")) }
	if reloads() != 1 {
		t.Fatalf("first sync made %d caddy calls", reloads())
	}

	if code, _, raw := do(t, srv, "POST", "/api/v1/routes", `{"host":"x.localhost","port":9810}`); code != 201 {
		t.Fatalf("POST /routes = %d %s", code, raw)
	}
	last = d.SyncProxy(last)
	out := caddyfile(t, d)
	if !strings.Contains(out, "http://x.localhost:80") || !strings.Contains(out, "reverse_proxy 127.0.0.1:9810") {
		t.Fatalf("x.localhost → 9810 is not in the Caddyfile:\n%s", out)
	}
	if reloads() != 2 {
		t.Fatalf("calls = %v, want a reload after the route", sd.Calls("caddy"))
	}
	reload := sd.Calls("caddy")[1]
	if len(reload) < 4 || reload[0] != "reload" || reload[1] != "--config" || reload[2] != d.svc.Paths.Caddyfile {
		t.Fatalf("caddy was called as %v", reload)
	}

	// nothing changed: no reload
	last = d.SyncProxy(last)
	if reloads() != 2 {
		t.Fatalf("an unchanged sync reloaded caddy: %v", sd.Calls("caddy"))
	}

	// removing the route removes it from the Caddyfile
	doStatus(t, srv, "DELETE", "/api/v1/routes/x.localhost", "")
	d.SyncProxy(last)
	if strings.Contains(caddyfile(t, d), "x.localhost") || reloads() != 3 {
		t.Fatalf("route still there or no reload (%d):\n%s", reloads(), caddyfile(t, d))
	}
}

// A route added from the CLI (another process, straight into the registry) is
// picked up by the next round.
func TestRouteAddedByAnotherProcessReachesTheCaddyfile(t *testing.T) {
	d, _ := newTestDaemon(t)
	sd := testfake.Install(t)
	sd.Caddy(t)
	last := d.SyncProxy("")

	other, err := registry.Open(d.svc.Paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.AddRoute(registry.Route{Host: "x.localhost", Port: 9810}); err != nil {
		t.Fatal(err)
	}
	d.SyncProxy(last)
	if !strings.Contains(caddyfile(t, d), "reverse_proxy 127.0.0.1:9810") {
		t.Fatalf("not in the Caddyfile:\n%s", caddyfile(t, d))
	}
}

// With a project named web running, an explicit web.localhost replaces its
// upstream in the Caddyfile instead of adding a second site.
func TestExplicitRouteReplacesTheProjectsInTheCaddyfile(t *testing.T) {
	d, srv := newTestDaemon(t)
	d.svc.PortFree = func(int) bool { return true }
	sd := testfake.Install(t)
	sd.Caddy(t)
	dir, _ := json.Marshal(t.TempDir())
	if code, _, raw := do(t, srv, "POST", "/api/v1/run", `{"name":"web","dir":`+string(dir)+`,"cmd":["sleep","100"],"ports":[4100]}`); code != 200 {
		t.Fatalf("run = %d %s", code, raw)
	}

	last := d.SyncProxy("")
	out := caddyfile(t, d)
	if !strings.Contains(out, "reverse_proxy 127.0.0.1:4100") {
		t.Fatalf("the project's route is missing:\n%s", out)
	}

	do(t, srv, "POST", "/api/v1/routes", `{"host":"web.localhost","port":9810}`)
	d.SyncProxy(last)
	out = caddyfile(t, d)
	if !strings.Contains(out, "reverse_proxy 127.0.0.1:9810") || strings.Contains(out, "127.0.0.1:4100") {
		t.Fatalf("the explicit route must replace the project's:\n%s", out)
	}
	if n := strings.Count(out, "http://web.localhost:80"); n != 1 {
		t.Fatalf("web.localhost has %d sites:\n%s", n, out)
	}
	_, _, raw := do(t, srv, "GET", "/api/v1/routes", "")
	var list []map[string]any
	_ = json.Unmarshal([]byte(raw), &list)
	if len(list) != 1 || list[0]["source"] != "explicit" || list[0]["port"] != float64(9810) {
		t.Fatalf("GET /routes = %s", raw)
	}
}

// A Caddy that stays down is one line in the log, not one per round, even
// though the real caddy puts a line with a new timestamp before its error (the
// fake does the same), and coming back is one more line.
func TestSyncProxyLogsAnOutageOnce(t *testing.T) {
	d, srv := newTestDaemon(t)
	sd := testfake.Install(t)
	sd.Caddy(t)
	last := d.SyncProxy("")

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	sd.FailCaddy(t)
	if code, _, raw := do(t, srv, "POST", "/api/v1/routes", `{"host":"x.localhost","port":9810}`); code != 201 {
		t.Fatalf("POST /routes = %d %s", code, raw)
	}
	for i := 0; i < 3; i++ {
		last = d.SyncProxy(last)
	}
	out := buf.String()
	if n := strings.Count(out, "proxy:"); n != 1 || strings.Count(out, "\n") != 1 {
		t.Fatalf("3 failed rounds logged %d proxy lines, want one single-line entry:\n%s", n, out)
	}
	if !strings.Contains(out, "reload refused") || strings.Contains(out, "using provided configuration") {
		t.Fatalf("the entry should carry the error line only:\n%s", out)
	}

	buf.Reset()
	sd.FixCaddy(t)
	last = d.SyncProxy(last)
	if out := buf.String(); strings.Count(out, "\n") != 1 || !strings.Contains(out, "succeeded again") {
		t.Fatalf("recovery log = %q, want one line", out)
	}
	buf.Reset()
	d.SyncProxy(last)
	if buf.Len() != 0 {
		t.Fatalf("a healthy round logged %q", buf.String())
	}
}
