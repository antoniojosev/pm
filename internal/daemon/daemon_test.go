package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/antoniojosev/pm/internal/api"
	"github.com/antoniojosev/pm/internal/config"
	"github.com/antoniojosev/pm/internal/detector"
	"github.com/antoniojosev/pm/internal/registry"
)

// newTestDaemon serves the API mux (no SPA, no MCP, no proxy loop) against a
// throwaway PM_HOME with no external binaries reachable.
func newTestDaemon(t *testing.T, roots ...string) (*Daemon, *httptest.Server) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	paths := config.Paths{
		Root:      root,
		Registry:  filepath.Join(root, "registry.json"),
		LogsDir:   filepath.Join(root, "logs"),
		RunDir:    filepath.Join(root, "run"),
		Caddyfile: filepath.Join(root, "Caddyfile"),
	}
	if err := paths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	store, err := registry.Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	svc := api.New(paths, store, roots)
	svc.Run.Snap = func() ([]detector.Instance, error) { return nil, nil }

	d := New(svc, 9797)
	mux := http.NewServeMux()
	d.routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return d, srv
}

func do(t *testing.T, srv *httptest.Server, method, path, body string) (int, map[string]any, string) {
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
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("%s %s: Content-Type = %q", method, path, ct)
	}
	obj := map[string]any{}
	_ = json.Unmarshal(raw, &obj)
	return res.StatusCode, obj, string(raw)
}

func TestHealth(t *testing.T) {
	_, srv := newTestDaemon(t)
	code, obj, _ := do(t, srv, "GET", "/api/health", "")
	if code != 200 || obj["status"] != "ok" {
		t.Fatalf("health = %d %v", code, obj)
	}
}

func TestProjectsCRUD(t *testing.T) {
	d, srv := newTestDaemon(t)

	code, obj, _ := do(t, srv, "POST", "/api/projects", `{"dir":"/srv/x"}`)
	if code != 400 || !strings.Contains(obj["error"].(string), "name is required") {
		t.Fatalf("nameless project: %d %v", code, obj)
	}
	code, obj, _ = do(t, srv, "POST", "/api/projects", `{bad json`)
	if code != 400 || obj["error"] == nil {
		t.Fatalf("bad json: %d %v", code, obj)
	}
	code, _, _ = do(t, srv, "POST", "/api/projects", `{"name":"web","dir":"/srv/web","prefer_port":5173}`)
	if code != 200 {
		t.Fatalf("create: %d", code)
	}
	if p, ok := d.svc.Get("web"); !ok || p.PreferPort != 5173 || p.Tier != registry.TierEphemeral {
		t.Fatalf("stored = %+v, %v", p, ok)
	}

	code, _, raw := do(t, srv, "GET", "/api/projects", "")
	var list []registry.Project
	if err := json.Unmarshal([]byte(raw), &list); err != nil || code != 200 || len(list) != 1 || list[0].Name != "web" {
		t.Fatalf("list: %d %s (%v)", code, raw, err)
	}

	code, _, raw = do(t, srv, "GET", "/api/ps", "")
	var rows []api.Row
	if err := json.Unmarshal([]byte(raw), &rows); err != nil || code != 200 || len(rows) != 1 || rows[0].Status != "down" {
		t.Fatalf("ps: %d %s (%v)", code, raw, err)
	}

	code, obj, _ = do(t, srv, "DELETE", "/api/projects/web", "")
	if code != 200 || obj["ok"] != true {
		t.Fatalf("delete: %d %v", code, obj)
	}
	code, _, _ = do(t, srv, "DELETE", "/api/projects/web", "")
	if code != 400 {
		t.Fatalf("delete twice: %d", code)
	}
}

func TestLifecycleErrorsAreReported(t *testing.T) {
	_, srv := newTestDaemon(t)
	_, _, _ = do(t, srv, "POST", "/api/projects", `{"name":"remote"}`)

	tests := []struct {
		method, path, want string
	}{
		{"POST", "/api/projects/remote/start?port=3000", "no local directory"},
		{"POST", "/api/projects/ghost/start", "not registered"},
		{"POST", "/api/projects/ghost/restart", "not registered"},
		{"POST", "/api/projects/ghost/promote", "not registered"},
		{"POST", "/api/projects/ghost/demote", "not registered"},
		{"POST", "/api/projects/remote/stop", "not running under pm"},
		{"POST", "/api/instances/1/stop", "nothing listening"},
		{"POST", "/api/instances/1/restart", "nothing listening"},
		{"POST", "/api/groups/nope/up", "does not exist"},
		{"POST", "/api/groups/nope/down", "does not exist"},
		{"POST", "/api/claim", "no listener on port 0"},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			body := ""
			if tc.path == "/api/claim" {
				body = `{"port":0,"name":"x"}`
			}
			code, obj, _ := do(t, srv, tc.method, tc.path, body)
			msg, _ := obj["error"].(string)
			if code != 400 || !strings.Contains(msg, tc.want) {
				t.Fatalf("got %d %q, want 400 containing %q", code, msg, tc.want)
			}
		})
	}
}

func TestGroupsAPI(t *testing.T) {
	_, srv := newTestDaemon(t)
	code, _, _ := do(t, srv, "POST", "/api/groups", `{"name":"stack","projects":["web","api"]}`)
	if code != 200 {
		t.Fatalf("create group: %d", code)
	}
	code, _, _ = do(t, srv, "POST", "/api/groups", `nope`)
	if code != 400 {
		t.Fatalf("bad group json: %d", code)
	}
	code, _, raw := do(t, srv, "GET", "/api/groups", "")
	var groups []registry.Group
	if err := json.Unmarshal([]byte(raw), &groups); err != nil || code != 200 || len(groups) != 1 || len(groups[0].Projects) != 2 {
		t.Fatalf("groups: %d %s", code, raw)
	}
	// members are not registered → per-project error notes, HTTP 200
	code, _, raw = do(t, srv, "POST", "/api/groups/stack/up", "")
	if code != 200 || !strings.Contains(raw, "error: ") {
		t.Fatalf("group up: %d %s", code, raw)
	}
	code, _, _ = do(t, srv, "POST", "/api/groups/stack/down", "")
	if code != 200 {
		t.Fatalf("group down: %d", code)
	}
	code, _, _ = do(t, srv, "DELETE", "/api/groups/stack", "")
	if code != 200 {
		t.Fatalf("delete group: %d", code)
	}
	code, _, _ = do(t, srv, "DELETE", "/api/groups/stack", "")
	if code != 400 {
		t.Fatalf("delete missing group: %d", code)
	}
}

func TestClaimAndScan(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "svc")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "go.mod"), []byte("module svc"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, srv := newTestDaemon(t, root)
	d.svc.Run.Snap = func() ([]detector.Instance, error) {
		return []detector.Instance{{Port: 8080, PID: 7, Cwd: proj}}, nil
	}

	code, obj, _ := do(t, srv, "POST", "/api/claim", `{"port":8080,"name":"svc"}`)
	if code != 200 || obj["name"] != "svc" || obj["stack"] != "go" {
		t.Fatalf("claim: %d %v", code, obj)
	}
	code, _, _ = do(t, srv, "POST", "/api/claim", `garbage`)
	if code != 400 {
		t.Fatalf("claim bad json: %d", code)
	}

	// svc already registered by the claim → scan adds nothing new
	code, _, raw := do(t, srv, "POST", "/api/scan", "")
	if code != 200 || strings.TrimSpace(raw) != "null" {
		t.Fatalf("scan: %d %s", code, raw)
	}
}

func TestLogsEndpoint(t *testing.T) {
	d, srv := newTestDaemon(t)
	if err := os.WriteFile(d.svc.LogPath("web"), []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, obj, _ := do(t, srv, "GET", "/api/projects/web/logs?n=2", "")
	if code != 200 || obj["logs"] != "c\n" {
		t.Fatalf("logs: %d %v", code, obj)
	}
	code, obj, _ = do(t, srv, "GET", "/api/projects/none/logs", "")
	if code != 200 || obj["logs"] != "" {
		t.Fatalf("missing logs: %d %v", code, obj)
	}
}

func TestFSAndDetect(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app", "manage.py"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	_, srv := newTestDaemon(t, root)

	code, obj, _ := do(t, srv, "GET", "/api/fs", "")
	if code != 200 || obj["path"] != root {
		t.Fatalf("fs: %d %v", code, obj)
	}
	entries := obj["entries"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["stack"] != "django" {
		t.Fatalf("entries = %v", entries)
	}
	code, _, _ = do(t, srv, "GET", "/api/fs?path="+filepath.Join(root, "missing"), "")
	if code != 400 {
		t.Fatalf("fs missing: %d", code)
	}

	code, obj, _ = do(t, srv, "GET", "/api/detect?path="+filepath.Join(root, "app"), "")
	if code != 200 || obj["ok"] != true || obj["stack"] != "django" || obj["prefer_port"] != float64(8000) {
		t.Fatalf("detect: %d %v", code, obj)
	}
	code, obj, _ = do(t, srv, "GET", "/api/detect?path="+root, "")
	if code != 200 || obj["ok"] != false {
		t.Fatalf("detect nothing: %d %v", code, obj)
	}
}

func TestEventsStreamsPSOnce(t *testing.T) {
	_, srv := newTestDaemon(t)
	_, _, _ = do(t, srv, "POST", "/api/projects", `{"name":"web"}`)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/events", nil)
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	line, err := bufio.NewReader(res.Body).ReadString('\n')
	if err != nil {
		t.Fatalf("reading first event: %v", err)
	}
	if !strings.HasPrefix(line, "data: ") || !strings.Contains(line, `"name":"web"`) {
		t.Fatalf("first event = %q", line)
	}
	cancel() // client goes away → handler returns
}

func TestLogStream(t *testing.T) {
	d, srv := newTestDaemon(t)
	if err := os.WriteFile(d.svc.LogPath("web"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/logs/stream?name=web", nil)
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	r := bufio.NewReader(res.Body)
	var got []string
	for len(got) < 2 {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended early: %v (got %v)", err, got)
		}
		if strings.HasPrefix(line, "data: ") {
			got = append(got, strings.TrimSpace(strings.TrimPrefix(line, "data: ")))
		}
	}
	if got[0] != "hello" || got[1] != "world" {
		t.Fatalf("streamed = %v", got)
	}

	t.Run("missing log keeps the stream open", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/logs/stream?name=none", nil)
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		line, _ := bufio.NewReader(res.Body).ReadString('\n')
		if !strings.Contains(line, "waiting for logs") {
			t.Fatalf("first line = %q", line)
		}
	})
}

func TestSPAHandler(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html": {Data: []byte("<html>dash</html>")},
		"app.js":     {Data: []byte("console.log(1)")},
	}
	srv := httptest.NewServer(spaHandler(fsys))
	defer srv.Close()

	get := func(path string) (int, string) {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	if code, body := get("/"); code != 200 || body != "<html>dash</html>" {
		t.Fatalf("/ = %d %q", code, body)
	}
	if code, body := get("/app.js"); code != 200 || body != "console.log(1)" {
		t.Fatalf("/app.js = %d %q", code, body)
	}
	// unknown client-side route falls back to index.html
	if code, body := get("/projects/web"); code != 200 || body != "<html>dash</html>" {
		t.Fatalf("/projects/web = %d %q", code, body)
	}
}

func TestHelpers(t *testing.T) {
	for n, want := range map[int]string{0: "0", 7: "7", 9797: "9797", 65535: "65535"} {
		if got := itoa(n); got != want {
			t.Errorf("itoa(%d) = %q", n, got)
		}
	}
	if trimLeadingSlash("/a/b") != "a/b" || trimLeadingSlash("a") != "a" || trimLeadingSlash("") != "" {
		t.Error("trimLeadingSlash")
	}

	req := httptest.NewRequest("POST", "/x?port=3000&force=true", nil)
	if o := launchOpts(req); o.Port != 3000 || !o.Force {
		t.Errorf("launchOpts = %+v", o)
	}
	req = httptest.NewRequest("POST", "/x?port=abc", nil)
	if o := launchOpts(req); o.Port != 0 || o.Force {
		t.Errorf("launchOpts (invalid) = %+v", o)
	}

	d, _ := newTestDaemon(t)
	_ = d.svc.Add(registry.Project{Name: "web", Host: "My Front"})
	if got := hostFor(d.svc, "web"); got != "my-front.localhost" {
		t.Errorf("hostFor(registered) = %q", got)
	}
	if got := hostFor(d.svc, "ghost"); got != "ghost.localhost" {
		t.Errorf("hostFor(unknown) = %q", got)
	}
}

func TestServeAndShutdown(t *testing.T) {
	t.Setenv("PM_NO_PROXY", "1") // never touch the real Caddyfile
	d, _ := newTestDaemon(t)

	ln, err := freePort(t)
	if err != nil {
		t.Skip("no free port")
	}
	d.port = ln
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Serve(ctx) }()

	url := "http://127.0.0.1:" + itoa(ln) + "/api/health"
	var res *http.Response
	for i := 0; i < 50; i++ {
		if res, err = http.Get(url); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("daemon never came up: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("health = %d", res.StatusCode)
	}
	// the embedded dashboard is served at /
	res, err = http.Get("http://127.0.0.1:" + itoa(ln) + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(strings.ToLower(string(body)), "<html") {
		t.Fatalf("/ did not serve the SPA: %.80s", body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop after cancel")
	}
}

// freePort asks the kernel for an unused loopback port.
func freePort(t *testing.T) (int, error) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
