package client_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/antoniojosev/pm/client"
	"github.com/antoniojosev/pm/internal/api"
	"github.com/antoniojosev/pm/internal/config"
	"github.com/antoniojosev/pm/internal/daemon"
	"github.com/antoniojosev/pm/internal/detector"
	"github.com/antoniojosev/pm/internal/registry"
	"github.com/antoniojosev/pm/internal/runner"
	"github.com/antoniojosev/pm/internal/testfake"
	"github.com/antoniojosev/pm/internal/version"
)

// contractEnv is the real daemon (its full handler, over httptest) on a
// throwaway PM_HOME, with fake systemctl and systemd-run on PATH: nothing real
// is ever started, nothing leaves the process.
func contractEnv(t *testing.T) (*client.Client, *testfake.Systemd) {
	t.Helper()
	sd := testfake.Install(t) // PATH holds only the fakes
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
	svc := api.New(paths, store, nil)
	svc.Run.Snap = func() ([]detector.Instance, error) { return nil, nil }
	svc.PortFree = func(int) bool { return true }
	srv := httptest.NewServer(daemon.New(svc, 0).Handler())
	t.Cleanup(srv.Close)
	return client.New(srv.URL), sd
}

// TestClientAgainstTheDaemon drives the whole client surface through the real
// daemon: what the client sends is what the daemon accepts, and what the
// daemon answers is what the client decodes.
func TestClientAgainstTheDaemon(t *testing.T) {
	c, sd := contractEnv(t)
	ctx := context.Background()

	v, err := c.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v.API != client.APIVersion || v.Version != version.Version {
		t.Fatalf("version = %+v", v)
	}

	// Leases: two ports for wt-qa, one for wt-dev.
	lease, err := c.Leases.Create(ctx, client.LeaseRequest{Owner: "wt-qa", Count: 2, Prefer: 4500})
	if err != nil {
		t.Fatal(err)
	}
	if lease.ID == "" || lease.Owner != "wt-qa" || len(lease.Ports) != 2 || lease.Ports[0] < 4500 || lease.CreatedAt.IsZero() {
		t.Fatalf("lease = %+v", lease)
	}
	other, err := c.Leases.Create(ctx, client.LeaseRequest{Owner: "wt-dev"})
	if err != nil || len(other.Ports) != 1 || slices.Contains(lease.Ports, other.Ports[0]) {
		t.Fatalf("second lease = %+v, %v", other, err)
	}
	all, err := c.Leases.List(ctx, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("List all = %+v, %v", all, err)
	}
	mine, err := c.Leases.List(ctx, "wt-qa")
	if err != nil || len(mine) != 1 || mine[0].ID != lease.ID || !slices.Equal(mine[0].Ports, lease.Ports) {
		t.Fatalf("List wt-qa = %+v, %v", mine, err)
	}
	if none, err := c.Leases.List(ctx, "nobody"); err != nil || none == nil || len(none) != 0 {
		t.Fatalf("List nobody = %#v, %v", none, err)
	}

	// Run on the leased ports, with env, limits and a log of our own.
	dir, logFile := t.TempDir(), filepath.Join(t.TempDir(), "api.log")
	if err := os.WriteFile(logFile, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := c.Run(ctx, client.RunSpec{
		Name: "api-qa", Dir: dir, Cmd: []string{"sleep", "100"},
		Env:    map[string]string{"PORT": "4500"},
		Limits: client.Limits{Memory: "256M", CPU: "50%"},
		Log:    logFile, Owner: "wt-qa", Ports: lease.Ports,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Unit != "pm-api-qa.service" || res.PID != testfake.MainPID || !slices.Equal(res.Ports, lease.Ports) {
		t.Fatalf("run = %+v", res)
	}
	if !sd.Active(res.Unit) {
		t.Fatal("the unit was not started")
	}
	args := strings.Join(sd.Calls("systemd-run")[0], " ")
	for _, want := range []string{"MemoryMax=256M", "CPUQuota=50%", "PORT=4500"} {
		if !strings.Contains(args, want) {
			t.Fatalf("systemd-run did not get %q: %s", want, args)
		}
	}

	// Logs: all of it, and the tail.
	if out, err := c.Logs(ctx, "api-qa", 0); err != nil || !strings.Contains(out, "one") || !strings.Contains(out, "three") {
		t.Fatalf("Logs = %q, %v", out, err)
	}
	if out, err := c.Logs(ctx, "api-qa", 2); err != nil || strings.Contains(out, "one") || !strings.Contains(out, "three") {
		t.Fatalf("Logs tail = %q, %v", out, err)
	}

	// Stop by name; the run is forgotten.
	if err := c.Stop(ctx, "api-qa"); err != nil {
		t.Fatal(err)
	}
	if sd.Active(res.Unit) {
		t.Fatal("Stop left the unit running")
	}

	// Owners: Stop keeps leases, Delete releases them; others are untouched.
	for _, name := range []string{"web-qa", "worker-qa"} {
		if _, err := c.Run(ctx, client.RunSpec{Name: name, Dir: dir, Cmd: []string{"sleep", "100"}, Owner: "wt-qa"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Run(ctx, client.RunSpec{Name: "web-dev", Dir: dir, Cmd: []string{"sleep", "100"}, Owner: "wt-dev"}); err != nil {
		t.Fatal(err)
	}
	stopped, err := c.Owners.Stop(ctx, "wt-qa")
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Owner != "wt-qa" || len(stopped.Stopped) != 2 || stopped.LeasesReleased == nil || len(stopped.LeasesReleased) != 0 {
		t.Fatalf("Owners.Stop = %+v", stopped)
	}
	if got, _ := c.Leases.List(ctx, "wt-qa"); len(got) != 1 {
		t.Fatalf("Owners.Stop must keep the leases: %+v", got)
	}
	if sd.Active("pm-web-qa.service") || !sd.Active("pm-web-dev.service") {
		t.Fatal("Owners.Stop stopped the wrong units")
	}
	deleted, err := c.Owners.Delete(ctx, "wt-qa")
	if err != nil || deleted.Owner != "wt-qa" || !slices.Equal(deleted.LeasesReleased, []string{lease.ID}) {
		t.Fatalf("Owners.Delete = %+v, %v", deleted, err)
	}
	if got, _ := c.Leases.List(ctx, "wt-qa"); len(got) != 0 {
		t.Fatalf("leases left after Owners.Delete: %+v", got)
	}

	// Leases.Delete by id.
	if err := c.Leases.Delete(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Leases.List(ctx, ""); len(got) != 0 {
		t.Fatalf("leases left after Leases.Delete: %+v", got)
	}
}

// TestDaemonRefusalsReachTheCaller checks that what the daemon rejects comes
// back as a *client.Error carrying pm's own message.
func TestDaemonRefusalsReachTheCaller(t *testing.T) {
	c, sd := contractEnv(t)
	ctx := context.Background()

	var e *client.Error
	_, err := c.Run(ctx, client.RunSpec{Name: "x", Dir: "rel", Cmd: []string{"true"}})
	if !errors.As(err, &e) || e.Status != 400 || !strings.Contains(e.Message, "absolute") {
		t.Fatalf("relative dir: %v", err)
	}
	if len(sd.Calls("systemd-run")) != 0 {
		t.Fatal("systemd-run ran for a refused request")
	}
	if _, err := c.Leases.Create(ctx, client.LeaseRequest{Owner: "a", Count: 500}); !errors.As(err, &e) || !strings.Contains(e.Message, "invalid count") {
		t.Fatalf("count: %v", err)
	}
	if err := c.Leases.Delete(ctx, "nope"); !errors.As(err, &e) || e.Status != 400 {
		t.Fatalf("unknown lease: %v", err)
	}
	if err := c.Stop(ctx, "ghost"); !errors.As(err, &e) {
		t.Fatalf("unknown process: %v", err)
	}
	if _, err := c.Owners.Stop(ctx, "bad owner!"); !errors.As(err, &e) {
		t.Fatalf("invalid owner: %v", err)
	}

	// A stop that fails midway still tells what it did.
	dir := t.TempDir()
	if _, err := c.Run(ctx, client.RunSpec{Name: "stuck", Dir: dir, Cmd: []string{"sleep", "100"}, Owner: "wt-x"}); err != nil {
		t.Fatal(err)
	}
	sd.FailStop(t)
	res, err := c.Owners.Stop(ctx, "wt-x")
	if !errors.As(err, &e) || res.Owner != "wt-x" || len(res.Errors) == 0 {
		t.Fatalf("a failing stop = %+v, %v", res, err)
	}
}

// TestTypesMatchTheDaemons guards against drift: every client type must have
// exactly the JSON fields of the daemon type it stands for. The client cannot
// import them (standard library only), so this test, which can, does.
func TestTypesMatchTheDaemons(t *testing.T) {
	pairs := []struct{ client, daemon any }{
		{client.VersionInfo{}, api.VersionInfo{}},
		{client.LeaseRequest{}, api.LeaseRequest{}},
		{client.Lease{}, registry.Lease{}},
		{client.Limits{}, registry.Limits{}},
		{client.Compose{}, registry.Compose{}},
		{client.RunSpec{}, runner.RunSpec{}},
		{client.RunResult{}, runner.RunResult{}},
		{client.Stopped{}, runner.Stopped{}},
		{client.OwnerResult{}, api.OwnerResult{}},
	}
	for _, p := range pairs {
		ct, dt := reflect.TypeOf(p.client), reflect.TypeOf(p.daemon)
		t.Run(ct.Name(), func(t *testing.T) {
			got, want := jsonFields(ct), jsonFields(dt)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("client %s has fields %v, daemon %s has %v", ct.Name(), got, dt, want)
			}
		})
	}
}

// jsonFields maps each JSON key of a struct to its Go type kind and whether it
// is omitted when empty, flattening embedded structs like encoding/json does.
func jsonFields(rt reflect.Type) map[string]string {
	out := map[string]string{}
	for f := range rt.Fields() {
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			for k, v := range jsonFields(f.Type) {
				out[k] = v
			}
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" || !f.IsExported() {
			continue
		}
		kind := f.Type.Kind().String()
		if f.Type.Kind() == reflect.Slice || f.Type.Kind() == reflect.Pointer {
			kind += "-" + f.Type.Elem().Kind().String()
		}
		out[name] = kind + "," + opts
	}
	return out
}
