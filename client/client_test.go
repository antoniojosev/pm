package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antoniojosev/pm/client"
)

// fakeDaemon is a stand-in for the pm API that records the requests it gets.
type fakeDaemon struct {
	mu       sync.Mutex
	requests []string // "METHOD /path?query"
	bodies   []string
	api      int
	handler  func(w http.ResponseWriter, r *http.Request)
}

func newFake(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) (*fakeDaemon, *client.Client) {
	t.Helper()
	f := &fakeDaemon{api: client.APIVersion, handler: h}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
		f.bodies = append(f.bodies, string(b))
		f.mu.Unlock()
		if r.URL.Path == "/api/v1/version" {
			writeJSON(w, 200, map[string]any{"version": "9.9.9", "api": f.api, "caddy": true, "docker": false, "systemd": true})
			return
		}
		if f.handler != nil {
			f.handler(w, r)
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))
	t.Cleanup(srv.Close)
	return f, client.New(srv.URL + "/") // a trailing slash must not matter
}

func (f *fakeDaemon) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.requests...)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func TestVersionReportsTheDaemon(t *testing.T) {
	_, c := newFake(t, nil)
	v, err := c.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v.Version != "9.9.9" || v.API != 1 || !v.Caddy || v.Docker || !v.Systemd {
		t.Fatalf("version = %+v", v)
	}
}

func TestAnotherAPIGenerationFailsClearly(t *testing.T) {
	f, c := newFake(t, nil)
	f.api = 2

	v, err := c.Version(context.Background())
	var ve *client.VersionError
	if !errors.As(err, &ve) || !errors.Is(err, client.ErrIncompatible) {
		t.Fatalf("Version err = %v, want a *VersionError matching ErrIncompatible", err)
	}
	if ve.Got != 2 || ve.Want != client.APIVersion || ve.PM != "9.9.9" || v.API != 2 {
		t.Fatalf("version error = %+v (info %+v)", ve, v)
	}
	if msg := err.Error(); !strings.Contains(msg, "API v2") || !strings.Contains(msg, "API v1") || !strings.Contains(msg, "9.9.9") {
		t.Fatalf("message does not say what mismatched: %q", msg)
	}

	// Nothing else is sent to a daemon that does not match.
	if _, err := c.Leases.Create(context.Background(), client.LeaseRequest{Owner: "a"}); !errors.Is(err, client.ErrIncompatible) {
		t.Fatalf("Leases.Create err = %v", err)
	}
	if err := c.Stop(context.Background(), "x"); !errors.Is(err, client.ErrIncompatible) {
		t.Fatalf("Stop err = %v", err)
	}
	for _, r := range f.seen() {
		if !strings.HasPrefix(r, "GET /api/v1/version") {
			t.Fatalf("a request got through to an incompatible daemon: %v", f.seen())
		}
	}
}

// TestADaemonWithoutTheVersionEndpointIsIncompatible covers every way a pm
// older than API v1 can answer GET /api/v1/version. The released v0.1.0 does
// not say 404: its dashboard answers any unknown path with 200 and index.html.
func TestADaemonWithoutTheVersionEndpointIsIncompatible(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"404": func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
		"dashboard fallback": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, "<!doctype html><html><head><title>pm</title></head><body><div id=app></div></body></html>")
		},
		"json without api": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"version":"v0.1.0"}`)
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			t.Cleanup(srv.Close)
			c := client.New(srv.URL)
			_, err := c.Version(context.Background())
			var ve *client.VersionError
			if !errors.As(err, &ve) || ve.Got != 0 || ve.Want != client.APIVersion || !errors.Is(err, client.ErrIncompatible) {
				t.Fatalf("err = %v", err)
			}
			if !strings.Contains(err.Error(), "upgrade pm") {
				t.Fatalf("message gives no way out: %q", err)
			}
			// The automatic check says the same before any other request.
			if err := c.Stop(context.Background(), "x"); !errors.Is(err, client.ErrIncompatible) {
				t.Fatalf("Stop err = %v", err)
			}
		})
	}
}

// A 2xx that is not JSON on any other endpoint is not an API mismatch.
func TestGarbageAnswerOnAnotherEndpointIsNotAMismatch(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<!doctype html>")
	})
	_, err := c.Leases.List(context.Background(), "")
	if err == nil || errors.Is(err, client.ErrIncompatible) {
		t.Fatalf("err = %v", err)
	}
}

func TestTheVersionIsCheckedOnceAndCanBeSkipped(t *testing.T) {
	f, c := newFake(t, nil)
	ctx := context.Background()
	for range 3 {
		if err := c.Stop(ctx, "x"); err != nil {
			t.Fatal(err)
		}
	}
	versions := 0
	for _, r := range f.seen() {
		if strings.HasPrefix(r, "GET /api/v1/version") {
			versions++
		}
	}
	if versions != 1 {
		t.Fatalf("version checks = %d, want 1: %v", versions, f.seen())
	}

	// Against a daemon that has no /version, the check off lets calls through.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]bool{"ok": true}) }))
	t.Cleanup(srv.Close)
	if err := client.New(srv.URL, client.WithoutVersionCheck()).Stop(ctx, "x"); err != nil {
		t.Fatalf("with the check off: %v", err)
	}
}

func TestRequestsAreShapedLikeTheAPI(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/leases":
			writeJSON(w, 200, []client.Lease{})
		case r.URL.Path == "/api/v1/projects/a b/logs" || r.URL.Path == "/api/v1/projects/a/logs":
			writeJSON(w, 200, map[string]string{"logs": "hello"})
		default:
			writeJSON(w, 200, map[string]bool{"ok": true})
		}
	})
	ctx := context.Background()
	if _, err := c.Leases.List(ctx, "wt qa"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Leases.List(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if err := c.Leases.Delete(ctx, "a/b"); err != nil {
		t.Fatal(err)
	}
	if out, err := c.Logs(ctx, "a b", 5); err != nil || out != "hello" {
		t.Fatalf("Logs = %q, %v", out, err)
	}
	if _, err := c.Logs(ctx, "a", 0); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(ctx, "a/../b"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Owners.Stop(ctx, "wt-qa"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Owners.Delete(ctx, "wt-qa"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET /api/v1/version",
		"GET /api/v1/leases?owner=wt+qa",
		"GET /api/v1/leases",
		"DELETE /api/v1/leases/a%2Fb",
		"GET /api/v1/projects/a%20b/logs?n=5",
		"GET /api/v1/projects/a/logs",
		"POST /api/v1/projects/a%2F..%2Fb/stop",
		"POST /api/v1/owners/wt-qa/stop",
		"DELETE /api/v1/owners/wt-qa",
	}
	got := f.seen()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestRunSendsTheSpecAsJSON(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, client.RunResult{Unit: "pm-x.service", PID: 7, Ports: []int{4100}})
	})
	res, err := c.Run(context.Background(), client.RunSpec{
		Name: "x", Dir: "/srv", Cmd: []string{"sleep", "1"},
		Env:    map[string]string{"PORT": "4100"},
		Limits: client.Limits{Memory: "2G", CPU: "150%"},
		Owner:  "wt-qa", Ports: []int{4100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Unit != "pm-x.service" || res.PID != 7 || len(res.Ports) != 1 {
		t.Fatalf("result = %+v", res)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(f.bodies[len(f.bodies)-1]), &sent); err != nil {
		t.Fatal(err)
	}
	lim, _ := sent["limits"].(map[string]any)
	if sent["name"] != "x" || sent["owner"] != "wt-qa" || lim["memory"] != "2G" || lim["cpu"] != "150%" {
		t.Fatalf("body = %v", sent)
	}
	// optional parts left out stay out: an empty limits object is not sent
	for _, k := range []string{"compose", "log"} {
		if _, has := sent[k]; has {
			t.Fatalf("%q must be omitted when empty: %v", k, sent)
		}
	}
	if _, err := c.Run(context.Background(), client.RunSpec{Name: "y", Dir: "/srv", Cmd: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.bodies[len(f.bodies)-1], "limits") {
		t.Fatalf("empty limits were sent: %s", f.bodies[len(f.bodies)-1])
	}
}

func TestFailedRequestsAreErrors(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/leases":
			writeJSON(w, 400, map[string]string{"error": "invalid count 500: ask for 1 to 100 ports"})
		default:
			w.WriteHeader(502)
			_, _ = w.Write([]byte("<html>Bad Gateway</html>"))
		}
	})
	_, err := c.Leases.Create(context.Background(), client.LeaseRequest{Owner: "a", Count: 500})
	var e *client.Error
	if !errors.As(err, &e) || e.Status != 400 || e.Message != "invalid count 500: ask for 1 to 100 ports" {
		t.Fatalf("err = %#v", err)
	}
	if !strings.Contains(err.Error(), "POST /api/v1/leases") || !strings.Contains(err.Error(), "400") {
		t.Fatalf("message = %q", err)
	}
	err = c.Stop(context.Background(), "x")
	if !errors.As(err, &e) || e.Status != 502 || !strings.Contains(e.Message, "Bad Gateway") {
		t.Fatalf("a non-JSON answer must still surface: %#v", err)
	}
}

func TestOwnerFailureKeepsTheResult(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 400, map[string]any{
			"error": "owner wt-qa: could not stop everything: boom", "owner": "wt-qa",
			"stopped":         []map[string]any{{"kind": "run", "name": "api"}},
			"leases_released": []string{}, "errors": []string{"boom"},
		})
	})
	res, err := c.Owners.Stop(context.Background(), "wt-qa")
	var e *client.Error
	if !errors.As(err, &e) || e.Status != 400 {
		t.Fatalf("err = %v", err)
	}
	if res.Owner != "wt-qa" || len(res.Stopped) != 1 || res.Stopped[0].Name != "api" || len(res.Errors) != 1 {
		t.Fatalf("a failed stop must still report what was stopped: %+v", res)
	}
}

func TestContextCancelsTheRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := client.New(srv.URL).Version(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's deadline", err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("hits = %d, want the one request that was cancelled", n)
	}
}

func TestUnreachableDaemonSaysSo(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	_, err := client.New(url).Version(context.Background())
	if err == nil || !strings.Contains(err.Error(), "GET /api/v1/version") {
		t.Fatalf("err = %v", err)
	}
	var ve *client.VersionError
	if errors.As(err, &ve) {
		t.Fatalf("an unreachable daemon is not an API mismatch: %v", err)
	}
}

func TestPSDetectAndRoutesAreShapedLikeTheAPI(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/ps" || (r.Method == "GET" && r.URL.Path == "/api/v1/routes"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("null")) // a daemon that forgot [] must still give a usable slice
		case r.URL.Path == "/api/v1/detect":
			writeJSON(w, 200, map[string]any{"ok": true, "stack": "go", "cmd": []string{"go", "run", "."}, "prefer_port": 0, "kind": "native"})
		case r.Method == "POST" && r.URL.Path == "/api/v1/routes":
			writeJSON(w, 201, client.Route{Host: "x.localhost", Port: 9810})
		case r.Method == "DELETE":
			w.WriteHeader(http.StatusNoContent)
		}
	})
	ctx := context.Background()

	if rows, err := c.PS(ctx); err != nil || rows == nil || len(rows) != 0 {
		t.Fatalf("PS = %#v, %v", rows, err)
	}
	d, err := c.Detect(ctx, "/srv/my app")
	if err != nil || !d.OK || d.Stack != "go" || d.Kind != "native" {
		t.Fatalf("Detect = %+v, %v", d, err)
	}
	if r, err := c.Routes.Add(ctx, client.RouteRequest{Host: "x.localhost", Port: 9810}); err != nil || r.Port != 9810 {
		t.Fatalf("Add = %+v, %v", r, err)
	}
	if err := c.Routes.Remove(ctx, "x.localhost"); err != nil {
		t.Fatal(err)
	}
	if routes, err := c.Routes.List(ctx); err != nil || routes == nil || len(routes) != 0 {
		t.Fatalf("List = %#v, %v", routes, err)
	}

	want := []string{
		"GET /api/v1/version",
		"GET /api/v1/ps",
		"GET /api/v1/detect?path=%2Fsrv%2Fmy+app",
		"POST /api/v1/routes",
		"DELETE /api/v1/routes/x.localhost",
		"GET /api/v1/routes",
	}
	if got := f.seen(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// An owner left empty is not sent: the daemon reads it as "no owner" either way.
	if body := f.bodies[3]; body != `{"host":"x.localhost","port":9810}`+"\n" && body != `{"host":"x.localhost","port":9810}` {
		t.Fatalf("route body = %q", body)
	}
}
