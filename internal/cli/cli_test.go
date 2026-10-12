package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/antoniojosev/pm/internal/api"
	"github.com/antoniojosev/pm/internal/registry"
	"github.com/antoniojosev/pm/internal/version"
	"github.com/spf13/cobra"
)

// isolate points every path the CLI touches at throwaway directories and
// hides external binaries, so commands never reach the real ~/.pm or systemd.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PM_HOME", filepath.Join(home, ".pm"))
	t.Setenv("PM_ROOTS", "")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("NO_COLOR", "1")
	return home
}

// capture runs fn with stdout and stderr redirected and returns what was written.
func capture(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	outF, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	errF, err := os.CreateTemp(t.TempDir(), "err")
	if err != nil {
		t.Fatal(err)
	}
	prevOut, prevErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outF, errF
	defer func() { os.Stdout, os.Stderr = prevOut, prevErr }()
	fn()
	outB, _ := os.ReadFile(outF.Name())
	errB, _ := os.ReadFile(errF.Name())
	return string(outB), string(errB)
}

// run executes the CLI with the given argv (as if typed after `pm`).
func run(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	prev := os.Args
	os.Args = append([]string{"pm"}, args...)
	defer func() { os.Args = prev }()
	stdout, stderr = capture(t, func() { err = Execute() })
	return
}

func TestRoots(t *testing.T) {
	home := isolate(t)
	if got := roots(); len(got) != 1 || got[0] != filepath.Join(home, "projects") {
		t.Fatalf("default roots = %v", got)
	}
	t.Setenv("PM_ROOTS", "/a:/b")
	if got := roots(); strings.Join(got, ",") != "/a,/b" {
		t.Fatalf("PM_ROOTS roots = %v", got)
	}
}

func TestTextHelpers(t *testing.T) {
	if got := truncName(strings.Repeat("a", 30)); len([]rune(got)) != 26 || !strings.HasSuffix(got, "…") {
		t.Errorf("truncName long = %q", got)
	}
	if got := truncName("short"); got != "short" {
		t.Errorf("truncName short = %q", got)
	}
	if portToken(0) != "—" || portToken(3000) != ":3000" {
		t.Error("portToken")
	}
	if urlToken(api.Row{Status: "up", URL: "http://web.localhost"}) != "web.localhost" || urlToken(api.Row{Status: "unclaimed", URL: "x"}) != "" {
		t.Error("urlToken")
	}
	if pidToken(api.Row{}) != "—" || pidToken(api.Row{PID: 42}) != "42" {
		t.Error("pidToken")
	}
	if processName(api.Row{Cmd: "/usr/bin/node server.js"}) != "node" {
		t.Error("processName from cmd")
	}
	if processName(api.Row{Docker: true, Name: "shop"}) != "shop" {
		t.Error("processName docker")
	}
	if !strings.Contains(processName(api.Row{}), "(system)") {
		t.Error("processName system")
	}
	if pad("ab", 4) != "ab  " || pad("abcd", 2) != "abcd" {
		t.Error("pad")
	}
	if maxi(1, 2) != 2 || maxi(3, 2) != 3 {
		t.Error("maxi")
	}
	for in, want := range map[string]string{"/srv/web/": "web", "/srv/web": "web", "web": "web", "": ""} {
		if got := baseName(in); got != want {
			t.Errorf("baseName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderPS(t *testing.T) {
	isolate(t)
	rows := []api.Row{
		{Name: "web", Status: "up", Port: 5173, URL: "http://web.localhost", Tier: "ephemeral", Group: "front"},
		{Name: "api", Status: "down", Port: 8000, URL: "http://api.localhost", Tier: "service"},
		{Name: "?", Status: "unclaimed", Port: 53},
	}
	failedRows := []api.Row{
		rows[0],
		{Name: "job", Status: "failed", Failure: "oom-kill", URL: "http://job.localhost", Tier: "ephemeral"},
		rows[1],
	}
	render := func(rows []api.Row, opts psOpts) string {
		out, _ := capture(t, func() { renderPS(os.Stdout, rows, opts) })
		return out
	}

	tests := []struct {
		name string
		rows []api.Row
		opts psOpts
		want []string
		not  []string
	}{
		{"default shows running plus hidden counts", rows, psOpts{},
			[]string{"1 running", "web", ":5173", "web.localhost", "1 stopped", "1 unmanaged", "pm ps -a", "pm listen"},
			[]string{"api.localhost"}},
		{"all lists running then stopped", rows, psOpts{all: true},
			[]string{"1 running · 1 stopped", "NAME", "PORT", "URL", "TIER", "web", "api", "service"}, nil},
		{"stopped only", rows, psOpts{stopped: true},
			[]string{"1 stopped", "api"}, []string{"web.localhost"}},
		{"group filter", rows, psOpts{all: true, group: "FRONT"},
			[]string{"1 running · 0 stopped", "web"}, []string{"api"}},
		{"nothing running hints", rows[1:2], psOpts{},
			[]string{"nothing running", "pm ps -a to see the 1 registered", "pm run -- <cmd>"}, nil},
		{"nothing stopped", rows[:1], psOpts{stopped: true},
			[]string{"nothing stopped"}, nil},
		{"failed shows in the default view with its reason", failedRows, psOpts{},
			[]string{"1 running · 1 failed", "web", "job", "failed: oom-kill", "1 stopped"},
			[]string{"api.localhost"}},
		{"failed alone is not nothing running", failedRows[1:2], psOpts{},
			[]string{"nothing running · 1 failed", "job", "failed: oom-kill"}, nil},
		{"all lists failed between running and stopped", failedRows, psOpts{all: true},
			[]string{"1 running · 1 stopped · 1 failed", "job", "failed: oom-kill", "api"}, nil},
		{"stopped view includes failed", failedRows, psOpts{stopped: true},
			[]string{"1 stopped · 1 failed", "job", "api"}, []string{"web.localhost"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := render(tc.rows, tc.opts)
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("missing %q in:\n%s", w, out)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(out, n) {
					t.Errorf("unexpected %q in:\n%s", n, out)
				}
			}
		})
	}
}

func TestRenderListen(t *testing.T) {
	isolate(t)
	out, _ := capture(t, func() { renderListen(os.Stdout, nil) })
	if !strings.Contains(out, "no unmanaged ports") {
		t.Fatalf("empty listen:\n%s", out)
	}
	rows := []api.Row{
		{Name: "?", Status: "unclaimed", Port: 9000, PID: 30, Cmd: "node x"},
		{Name: "?", Status: "unclaimed", Port: 53, System: true},
		{Name: "web", Status: "up", Port: 5173},
	}
	out, _ = capture(t, func() { renderListen(os.Stdout, rows) })
	for _, w := range []string{"2 unmanaged ports", "PORT", "PID", "PROCESS", ":53", ":9000", "30", "node", "(system)"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
	if strings.Contains(out, ":5173") {
		t.Fatalf("managed rows must not appear in listen:\n%s", out)
	}
	if strings.Index(out, ":53") > strings.Index(out, ":9000") {
		t.Fatalf("listen must sort by port:\n%s", out)
	}
}

func TestProjectFromFlags(t *testing.T) {
	c := &cobra.Command{Use: "x"}
	addProjectFlags(c)
	if err := c.ParseFlags([]string{"--name", "web", "--dir", "/srv/web", "--port", "3000", "--cmd", "npm run dev", "--host", "front", "--group", "g"}); err != nil {
		t.Fatal(err)
	}
	p := projectFromFlags(c)
	want := registry.Project{Name: "web", Dir: "/srv/web", PreferPort: 3000, Cmd: []string{"npm", "run", "dev"}, Host: "front", Group: "g", Tier: registry.TierEphemeral, Kind: registry.KindNative}
	if strings.Join(p.Cmd, " ") != "npm run dev" {
		t.Fatalf("cmd = %v", p.Cmd)
	}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("projectFromFlags = %+v, want %+v", p, want)
	}
}

func TestAddRmAndGroupCommands(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"devDependencies":{"vite":"5"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	out, _, err := run(t, "add", dir, "--name", "front")
	if err != nil || !strings.Contains(out, "front registered (vite)") || !strings.Contains(out, "front.localhost") {
		t.Fatalf("add: %v\n%s", err, out)
	}
	s, err := svc()
	if err != nil {
		t.Fatal(err)
	}
	p, ok := s.Get("front")
	if !ok || p.Dir != dir || p.PreferPort != 5173 || strings.Join(p.Cmd, " ") != "npm run dev" {
		t.Fatalf("registered = %+v", p)
	}

	// name defaults to the directory's base name
	out, _, err = run(t, "add", dir)
	if err != nil || !strings.Contains(out, filepath.Base(dir)+" registered") {
		t.Fatalf("add without name: %v\n%s", err, out)
	}

	out, _, err = run(t, "group", "set", "stack", "front")
	if err != nil || !strings.Contains(out, "group stack = front") {
		t.Fatalf("group set: %v\n%s", err, out)
	}
	out, _, err = run(t, "group", "ls")
	if err != nil || !strings.Contains(out, "stack: front") {
		t.Fatalf("group ls: %v\n%s", err, out)
	}
	if _, _, err = run(t, "group", "rm", "stack"); err != nil {
		t.Fatalf("group rm: %v", err)
	}
	if _, _, err = run(t, "group", "rm", "stack"); err == nil {
		t.Fatal("removing a missing group should fail")
	}

	out, _, err = run(t, "rm", "front")
	if err != nil || !strings.Contains(out, "front removed") {
		t.Fatalf("rm: %v\n%s", err, out)
	}
	_ = s.Store.Reload() // each command opens its own store; re-read from disk
	if _, ok := s.Get("front"); ok {
		t.Fatal("project still registered after rm")
	}
	if _, _, err = run(t, "rm", "front"); err == nil {
		t.Fatal("removing a missing project should fail")
	}
}

func TestCLIErrorsAreFriendly(t *testing.T) {
	isolate(t)
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"rm"}, "invalid number of arguments"},
		{[]string{"nope"}, "unknown command"},
		{[]string{"ps", "--bogus"}, "unknown flag"},
		{[]string{"ps", "-Z"}, "unknown flag"},
		{[]string{"start", "ghost"}, "not registered"},
	}
	for _, tc := range tests {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			_, stderr, err := run(t, tc.args...)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(stderr, tc.want) {
				t.Fatalf("stderr = %q, want %q", stderr, tc.want)
			}
		})
	}
}

func TestVersionFlag(t *testing.T) {
	isolate(t)
	out, _, err := run(t, "--version")
	if err != nil || !strings.Contains(out, version.Version) {
		t.Fatalf("--version: %v %q", err, out)
	}
}

func TestDoctorShowsDaemonVersion(t *testing.T) {
	isolate(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/version" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"version":"9.9.9","api":1,"caddy":true,"docker":false,"systemd":true}`))
	}))
	defer srv.Close()
	t.Setenv("PM_DAEMON_URL", srv.URL)

	out, _, err := run(t, "doctor")
	if err != nil {
		t.Fatal(err)
	}
	want := "pm 9.9.9 · api 1 · caddy yes · docker no · systemd yes"
	if !strings.Contains(out, want) {
		t.Fatalf("doctor output lacks %q:\n%s", want, out)
	}
}

func TestAPICheck(t *testing.T) {
	isolate(t)
	serve := func(body string, code int) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}

	if d, ok := apiCheck(serve(`{"version":"1","api":1}`, 200)); !ok || !strings.Contains(d, "api 1") {
		t.Errorf("matching api = %q, %v", d, ok)
	}
	if d, ok := apiCheck(serve(`{"version":"2","api":2}`, 200)); ok || !strings.Contains(d, "expects api 1") {
		t.Errorf("other api generation = %q, %v", d, ok)
	}
	if d, ok := apiCheck(serve(`not json`, 200)); ok || !strings.Contains(d, "bad response") {
		t.Errorf("garbage body = %q, %v", d, ok)
	}
	if d, ok := apiCheck(serve(`nope`, 404)); ok || !strings.Contains(d, "HTTP 404") {
		t.Errorf("old daemon without /version = %q, %v", d, ok)
	}
	if d, ok := apiCheck("http://127.0.0.1:1"); ok || !strings.Contains(d, "not reachable") {
		t.Errorf("no daemon = %q, %v", d, ok)
	}
}

func TestEditScanAndLogsCommands(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "svc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "svc", "go.mod"), []byte("module svc"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PM_ROOTS", root)

	out, _, err := run(t, "scan")
	if err != nil || !strings.Contains(out, "svc (go)") {
		t.Fatalf("scan: %v\n%s", err, out)
	}
	out, _, err = run(t, "scan")
	if err != nil || !strings.Contains(out, "no new projects") {
		t.Fatalf("second scan: %v\n%s", err, out)
	}

	out, _, err = run(t, "edit", "svc", "--port", "9090", "--host", "backend", "--group", "core", "--cmd", "go run ./cmd")
	if err != nil || !strings.Contains(out, "svc updated") {
		t.Fatalf("edit: %v\n%s", err, out)
	}
	s, err := svc()
	if err != nil {
		t.Fatal(err)
	}
	p, _ := s.Get("svc")
	if p.PreferPort != 9090 || p.Host != "backend" || p.Group != "core" || strings.Join(p.Cmd, " ") != "go run ./cmd" || p.Draft {
		t.Fatalf("edited = %+v", p)
	}
	if _, _, err = run(t, "edit", "ghost"); err == nil {
		t.Fatal("editing an unknown project should fail")
	}

	if err := os.WriteFile(s.LogPath("svc"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, err = run(t, "logs", "svc", "-n", "2")
	if err != nil || !strings.Contains(out, "two") {
		t.Fatalf("logs: %v\n%s", err, out)
	}
}

func TestLimitFlagsExist(t *testing.T) {
	for name, c := range map[string]*cobra.Command{
		"add": addCmd(), "edit": editCmd(), "start": startCmd(), "run": runCmd(),
	} {
		for _, f := range []string{"mem", "cpu"} {
			if c.Flags().Lookup(f) == nil {
				t.Errorf("pm %s has no --%s flag", name, f)
			}
		}
	}
}

func TestProjectFromFlagsReadsLimits(t *testing.T) {
	c := &cobra.Command{Use: "x"}
	addProjectFlags(c)
	if err := c.ParseFlags([]string{"--name", "web", "--mem", "256M", "--cpu", "150%"}); err != nil {
		t.Fatal(err)
	}
	if got := projectFromFlags(c).Limits; got != (registry.Limits{Memory: "256M", CPU: "150%"}) {
		t.Fatalf("limits = %+v", got)
	}
}

func TestAddAndEditLimits(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module svc"), 0o644); err != nil {
		t.Fatal(err)
	}
	stored := func() registry.Limits {
		t.Helper()
		s, err := svc()
		if err != nil {
			t.Fatal(err)
		}
		p, ok := s.Get("svc")
		if !ok {
			t.Fatal("svc not registered")
		}
		return p.Limits
	}

	if _, _, err := run(t, "add", dir, "--name", "svc", "--mem", "256M", "--cpu", "50%"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got := stored(); got != (registry.Limits{Memory: "256M", CPU: "50%"}) {
		t.Fatalf("after add: %+v", got)
	}

	// Editing one limit leaves the other alone.
	if _, _, err := run(t, "edit", "svc", "--mem", "1G"); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if got := stored(); got != (registry.Limits{Memory: "1G", CPU: "50%"}) {
		t.Fatalf("after editing memory: %+v", got)
	}

	// An unrelated edit keeps both.
	if _, _, err := run(t, "edit", "svc", "--group", "core"); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if got := stored(); got != (registry.Limits{Memory: "1G", CPU: "50%"}) {
		t.Fatalf("an edit that does not mention limits changed them: %+v", got)
	}

	// Passing an empty value clears that limit.
	if _, _, err := run(t, "edit", "svc", "--cpu", ""); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if got := stored(); got != (registry.Limits{Memory: "1G"}) {
		t.Fatalf("after clearing cpu: %+v", got)
	}

	// Invalid values are refused and nothing changes.
	if _, _, err := run(t, "edit", "svc", "--mem", "lots"); err == nil || !strings.Contains(err.Error(), "invalid memory limit") {
		t.Fatalf("invalid limit accepted: %v", err)
	}
	if got := stored(); got != (registry.Limits{Memory: "1G"}) {
		t.Fatalf("a rejected edit changed the project: %+v", got)
	}
}

func TestCgroupCheck(t *testing.T) {
	write := func(content string) string {
		f := filepath.Join(t.TempDir(), "cgroup.controllers")
		if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return f
	}
	missing := filepath.Join(t.TempDir(), "missing")
	root := write("cpuset cpu io memory pids")

	if detail, ok := cgroupCheck(root, write("cpu memory pids")); !ok || detail != "unified" {
		t.Fatalf("v2 with both controllers delegated: %q, %v", detail, ok)
	}

	detail, ok := cgroupCheck(missing, write("cpu memory"))
	if ok {
		t.Fatal("a missing cgroup v2 must fail the check")
	}
	for _, want := range []string{"--mem", "--cpu", "not be enforced"} {
		if !strings.Contains(detail, want) {
			t.Errorf("warning %q lacks %q", detail, want)
		}
	}

	// v2 is mounted but the user manager was not delegated the controllers:
	// systemd would accept MemoryMax=/CPUQuota= and silently ignore them.
	for _, tc := range []struct {
		user string
		want string
	}{
		{"pids", "memory and cpu not delegated"},
		{"cpu pids", "memory not delegated"},
		{"memory pids", "cpu not delegated"},
	} {
		detail, ok := cgroupCheck(root, write(tc.user))
		if ok || !strings.Contains(detail, tc.want) || !strings.Contains(detail, "not be enforced") {
			t.Errorf("user controllers %q: %q, %v; want failure mentioning %q", tc.user, detail, ok, tc.want)
		}
	}

	// An unreadable user cgroup cannot be verified: warn rather than pass.
	if detail, ok := cgroupCheck(root, missing); ok || !strings.Contains(detail, "may not be enforced") {
		t.Errorf("unreadable user controllers: %q, %v", detail, ok)
	}
}
