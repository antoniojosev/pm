package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/pm/internal/detector"
	"github.com/antoniojosev/pm/internal/registry"
)

// bogusPID cannot exist on Linux (pid_max tops out at 4194304), so killTree
// against it is a guaranteed no-op.
const bogusPID = 1 << 30

// unitLoaded scripts `systemctl show` so Stop takes the managed-unit path.
func unitLoaded(f *fakeExec, name string) {
	f.stdout["systemctl --user show "+UnitName(name)+" -p LoadState --value"] = "loaded"
}

// liveAfterLaunch returns a Snap that reports `before` until the launch
// command has been invoked, then `after` — mimicking a unit binding its port.
func liveAfterLaunch(f *fakeExec, launcher string, before, after []detector.Instance) func() ([]detector.Instance, error) {
	return func() ([]detector.Instance, error) {
		if f.called(launcher) {
			return after, nil
		}
		return before, nil
	}
}

func newTestRunner(t *testing.T) (*Runner, *fakeExec) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	paths := tempPaths(t)
	store, err := registry.Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	r := New(paths, store)
	r.PortWait = 200 * time.Millisecond
	r.Snap = func() ([]detector.Instance, error) { return nil, nil }
	return r, installFakeExec(t)
}

func TestLaunchEphemeralBumpsPortOnCollision(t *testing.T) {
	r, f := newTestRunner(t)
	dir := t.TempDir()
	_ = r.Store.Upsert(registry.Project{
		Name: "web", Dir: dir, Kind: registry.KindNative, Tier: registry.TierEphemeral,
		Cmd: []string{"npm", "run", "dev"}, PreferPort: 3000,
		PortStrategy: registry.PortStrategy{Flag: "--port"},
	})
	other := detector.Instance{Project: "other", Port: 3000, PID: 42}
	r.Snap = liveAfterLaunch(f, "systemd-run",
		[]detector.Instance{other},
		[]detector.Instance{other, {Project: "web", Port: 3001, Source: detector.SourceMarker}},
	)

	res, err := r.Start("web", LaunchOpts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Port != 3001 || res.Note != "port 3000 taken → using 3001" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.URL != "http://web.localhost (:3001)" || res.Unit != "pm-web.service" {
		t.Fatalf("unexpected result: %+v", res)
	}

	args := f.argsOf("systemd-run")
	if args == nil {
		t.Fatal("systemd-run was not invoked")
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--user",
		"--unit=pm-web.service",
		"--working-directory=" + dir,
		"--setenv=PM_PROJECT=web",
		"StandardOutput=append:" + r.Paths.LogFile("web"),
		"-- npm run dev -- --port 3001",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("systemd-run args missing %q:\n%s", want, joined)
		}
	}
	if _, err := os.Stat(r.Paths.LogsDir); err != nil {
		t.Fatal("launch should create the runtime dirs")
	}
	if f.called("systemctl") {
		t.Fatal("ephemeral launch must not touch persistent units")
	}
}

func TestLaunchFallsBackToPreferredURLWhenPortNotSeen(t *testing.T) {
	r, _ := newTestRunner(t)
	_ = r.Store.Upsert(registry.Project{Name: "svc", Dir: t.TempDir(), Kind: registry.KindNative, Cmd: []string{"./run"}})

	res, err := r.Start("svc", LaunchOpts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Port != 0 || res.URL != "http://svc.localhost" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestLaunchSurfacesSystemdRunFailure(t *testing.T) {
	r, f := newTestRunner(t)
	f.fail["systemd-run"] = true
	_ = r.Store.Upsert(registry.Project{Name: "svc", Dir: t.TempDir(), Kind: registry.KindNative, Cmd: []string{"./run"}})

	if _, err := r.Start("svc", LaunchOpts{}); err == nil || !strings.Contains(err.Error(), "systemd-run failed") {
		t.Fatalf("expected systemd-run failure, got %v", err)
	}
}

func TestLaunchServiceTierWritesUnitAndEnables(t *testing.T) {
	r, f := newTestRunner(t)
	_ = r.Store.Upsert(registry.Project{
		Name: "api", Dir: t.TempDir(), Kind: registry.KindNative, Tier: registry.TierService,
		Cmd: []string{"go", "run", "."}, PreferPort: 8080,
		PortStrategy: registry.PortStrategy{Env: "PORT"},
	})
	r.Snap = liveAfterLaunch(f, "systemctl", nil, []detector.Instance{{Project: "api", Port: 8080}})

	res, err := r.Start("api", LaunchOpts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Port != 8080 {
		t.Fatalf("port = %d", res.Port)
	}
	unit, err := os.ReadFile(r.unitPath("api"))
	if err != nil {
		t.Fatalf("unit file not written: %v", err)
	}
	if !strings.Contains(string(unit), "Environment=PORT=8080") {
		t.Fatalf("unit lacks injected env:\n%s", unit)
	}
	if !f.called("systemctl", "daemon-reload") || !f.called("systemctl", "enable", "--now", "pm-api.service") {
		t.Fatalf("expected daemon-reload + enable, got %v", f.calls)
	}
	if f.called("systemd-run") {
		t.Fatal("service tier must not use systemd-run")
	}

	t.Run("enable failure is reported", func(t *testing.T) {
		f.fail["systemctl --user enable --now pm-api.service"] = true
		if _, err := r.Start("api", LaunchOpts{}); err == nil || !strings.Contains(err.Error(), "systemctl enable") {
			t.Fatalf("expected enable failure, got %v", err)
		}
	})
}

func TestStopManagedUnit(t *testing.T) {
	r, f := newTestRunner(t)
	unitLoaded(f, "web")

	if err := r.Stop("web"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !f.called("systemctl", "stop", "pm-web.service") || !f.called("systemctl", "reset-failed", "pm-web.service") {
		t.Fatalf("expected stop + reset-failed, got %v", f.calls)
	}
	if f.called("docker") {
		t.Fatal("managed stop must not touch docker")
	}

	f.fail["systemctl --user stop pm-web.service"] = true
	if err := r.Stop("web"); err == nil {
		t.Fatal("expected systemctl stop failure to surface")
	}
}

func TestStopExternal(t *testing.T) {
	tests := []struct {
		name    string
		live    []detector.Instance
		wantErr string
		docker  string
	}{
		{
			name:   "docker container is stopped through docker",
			live:   []detector.Instance{{Project: "db", Port: 5432, Docker: true, Container: "abc123"}},
			docker: "abc123",
		},
		{
			name: "native pid gets its tree killed",
			live: []detector.Instance{{Project: "db", Port: 5432, PID: bogusPID}},
		},
		{
			name:    "nothing detected",
			live:    nil,
			wantErr: "is not running under pm",
		},
		{
			name:    "other projects are ignored",
			live:    []detector.Instance{{Project: "web", Port: 3000, PID: bogusPID}},
			wantErr: "is not running under pm",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, f := newTestRunner(t)
			r.Snap = func() ([]detector.Instance, error) { return tc.live, nil }

			err := r.Stop("db")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Stop: %v", err)
			}
			if tc.docker != "" && !f.called("docker", "stop", tc.docker) {
				t.Fatalf("expected docker stop %s, got %v", tc.docker, f.calls)
			}
			if f.called("systemctl", "stop") {
				t.Fatal("external stop must not call systemctl stop")
			}
		})
	}
}

func TestStopInstance(t *testing.T) {
	tests := []struct {
		name    string
		live    []detector.Instance
		port    int
		wantErr string
		docker  string
	}{
		{"docker", []detector.Instance{{Port: 5432, Docker: true, Container: "c1"}}, 5432, "", "c1"},
		{"native", []detector.Instance{{Port: 3000, PID: bogusPID}}, 3000, "", ""},
		{"no pid visible", []detector.Instance{{Port: 3000}}, 3000, "no visible PID", ""},
		{"nothing listening", nil, 9999, "nothing listening", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, f := newTestRunner(t)
			r.Snap = func() ([]detector.Instance, error) { return tc.live, nil }
			err := r.StopInstance(tc.port)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("StopInstance: %v", err)
			}
			if tc.docker != "" && !f.called("docker", "stop", tc.docker) {
				t.Fatalf("expected docker stop, got %v", f.calls)
			}
		})
	}

	t.Run("docker failure surfaces", func(t *testing.T) {
		r, f := newTestRunner(t)
		f.fail["docker"] = true
		r.Snap = func() ([]detector.Instance, error) {
			return []detector.Instance{{Port: 5432, Docker: true, Container: "c1"}}, nil
		}
		if err := r.StopInstance(5432); err == nil {
			t.Fatal("expected docker stop failure")
		}
	})
}

func TestRestartInstance(t *testing.T) {
	t.Run("docker restart", func(t *testing.T) {
		r, f := newTestRunner(t)
		r.Snap = func() ([]detector.Instance, error) {
			return []detector.Instance{{Project: "db", Port: 5432, Docker: true, Container: "c1"}}, nil
		}
		res, err := r.RestartInstance(5432)
		if err != nil {
			t.Fatalf("RestartInstance: %v", err)
		}
		if res.Note != "docker restart" || res.Project != "db" || !f.called("docker", "restart", "c1") {
			t.Fatalf("unexpected: %+v / %v", res, f.calls)
		}
	})

	t.Run("native relaunches recovered argv in cwd", func(t *testing.T) {
		r, f := newTestRunner(t)
		child := spawnSleep(t)
		pid := child.Process.Pid
		for i := 0; i < 20 && len(procArgv(pid)) == 0; i++ {
			time.Sleep(25 * time.Millisecond)
		}
		cwd := t.TempDir()
		r.Snap = func() ([]detector.Instance, error) {
			return []detector.Instance{{Port: 4321, PID: pid, Cwd: cwd}}, nil
		}

		res, err := r.RestartInstance(4321)
		if err != nil {
			t.Fatalf("RestartInstance: %v", err)
		}
		if res.Note != "relaunched" || res.Project != filepath.Base(cwd) || res.Port != 4321 {
			t.Fatalf("unexpected result: %+v", res)
		}
		args := strings.Join(f.argsOf("systemd-run"), " ")
		if !strings.Contains(args, "--unit="+UnitName(filepath.Base(cwd))) || !strings.HasSuffix(args, "-- sleep 30") {
			t.Fatalf("relaunch args: %s", args)
		}
		done := make(chan struct{})
		go func() { _ = child.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("original process was not terminated")
		}
	})

	t.Run("unrecoverable native", func(t *testing.T) {
		r, _ := newTestRunner(t)
		r.Snap = func() ([]detector.Instance, error) {
			return []detector.Instance{{Port: 4321, PID: bogusPID, Cwd: "/x"}}, nil
		}
		if _, err := r.RestartInstance(4321); err == nil || !strings.Contains(err.Error(), "could not recover") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("no pid", func(t *testing.T) {
		r, _ := newTestRunner(t)
		r.Snap = func() ([]detector.Instance, error) { return []detector.Instance{{Port: 4321}}, nil }
		if _, err := r.RestartInstance(4321); err == nil || !strings.Contains(err.Error(), "no visible PID") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("nothing listening", func(t *testing.T) {
		r, _ := newTestRunner(t)
		if _, err := r.RestartInstance(1); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestIsActive(t *testing.T) {
	r, f := newTestRunner(t)
	if r.IsActive("web") {
		t.Fatal("unit without output must not be active")
	}
	f.stdout["systemctl --user is-active pm-web.service"] = "active\n"
	if !r.IsActive("web") {
		t.Fatal("expected active")
	}
}

func TestPromoteAndDemote(t *testing.T) {
	r, f := newTestRunner(t)
	_ = r.Store.Upsert(registry.Project{Name: "web", Dir: t.TempDir(), Kind: registry.KindNative, Cmd: []string{"./run"}, PreferPort: 3000})

	if err := r.Promote("web"); err != nil {
		t.Fatalf("Promote (stopped): %v", err)
	}
	if p, _ := r.Store.Get("web"); p.Tier != registry.TierService {
		t.Fatalf("tier = %q, want service", p.Tier)
	}
	if f.called("systemctl", "enable") {
		t.Fatal("a stopped project must not be started on promote")
	}

	// running → promote relaunches as a persistent unit
	f.stdout["systemctl --user is-active pm-web.service"] = "active"
	unitLoaded(f, "web")
	if err := r.Promote("web"); err != nil {
		t.Fatalf("Promote (running): %v", err)
	}
	if !f.called("systemctl", "stop", "pm-web.service") || !f.called("systemctl", "enable", "--now", "pm-web.service") {
		t.Fatalf("expected stop + enable, got %v", f.calls)
	}
	if _, err := os.Stat(r.unitPath("web")); err != nil {
		t.Fatal("persistent unit not written")
	}

	if err := r.Demote("web"); err != nil {
		t.Fatalf("Demote: %v", err)
	}
	if p, _ := r.Store.Get("web"); p.Tier != registry.TierEphemeral {
		t.Fatalf("tier = %q, want ephemeral", p.Tier)
	}
	if !f.called("systemctl", "disable", "--now", "pm-web.service") {
		t.Fatalf("expected disable, got %v", f.calls)
	}
	if _, err := os.Stat(r.unitPath("web")); !os.IsNotExist(err) {
		t.Fatal("unit file should be removed on demote")
	}

	if err := r.Promote("ghost"); err == nil {
		t.Fatal("expected error promoting unknown project")
	}
	if err := r.Demote("ghost"); err == nil {
		t.Fatal("expected error demoting unknown project")
	}
}

func TestRestartStopsThenStarts(t *testing.T) {
	r, f := newTestRunner(t)
	_ = r.Store.Upsert(registry.Project{Name: "web", Dir: t.TempDir(), Kind: registry.KindNative, Cmd: []string{"./run"}})
	unitLoaded(f, "web")

	if _, err := r.Restart("web", LaunchOpts{}); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if !f.called("systemctl", "stop", "pm-web.service") || !f.called("systemd-run") {
		t.Fatalf("expected stop then systemd-run, got %v", f.calls)
	}
}

func TestDownStopsEveryMember(t *testing.T) {
	r, f := newTestRunner(t)
	_ = r.Store.UpsertGroup(registry.Group{Name: "stack", Projects: []string{"web", "api"}})
	unitLoaded(f, "web")
	unitLoaded(f, "api")

	if err := r.Down("stack"); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if !f.called("systemctl", "stop", "pm-web.service") || !f.called("systemctl", "stop", "pm-api.service") {
		t.Fatalf("expected both units stopped, got %v", f.calls)
	}
	if err := r.Down("nope"); err == nil {
		t.Fatal("expected error for unknown group")
	}
}

func TestRunAdhoc(t *testing.T) {
	writeFile := func(t *testing.T, dir, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("registers, detects the stack and launches", func(t *testing.T) {
		r, f := newTestRunner(t)
		dir := t.TempDir()
		writeFile(t, dir, "package.json", `{"scripts":{"dev":"vite"},"devDependencies":{"vite":"5"}}`)
		r.Snap = liveAfterLaunch(f, "systemd-run", nil, []detector.Instance{{Project: filepath.Base(dir), Port: 5173}})

		res, err := r.RunAdhoc(dir, []string{"npm", "run", "dev"}, AdhocOpts{})
		if err != nil {
			t.Fatalf("RunAdhoc: %v", err)
		}
		if res.Port != 5173 || res.Project != filepath.Base(dir) {
			t.Fatalf("unexpected result: %+v", res)
		}
		p, ok := r.Store.Get(filepath.Base(dir))
		if !ok {
			t.Fatal("project was not registered")
		}
		if p.Stack != "vite" || p.PreferPort != 5173 || p.Draft || p.Tier != registry.TierEphemeral || p.Dir != dir {
			t.Fatalf("registered project: %+v", p)
		}
		if !strings.HasSuffix(strings.Join(f.argsOf("systemd-run"), " "), "-- npm run dev -- --port 5173") {
			t.Fatalf("port not injected: %v", f.argsOf("systemd-run"))
		}
	})

	t.Run("name override and --always", func(t *testing.T) {
		r, f := newTestRunner(t)
		dir := t.TempDir()
		res, err := r.RunAdhoc(dir, []string{"./serve"}, AdhocOpts{Name: "custom", Always: true})
		if err != nil {
			t.Fatalf("RunAdhoc: %v", err)
		}
		if res.Project != "custom" {
			t.Fatalf("project = %q", res.Project)
		}
		p, _ := r.Store.Get("custom")
		if p.Tier != registry.TierService || p.Kind != registry.KindNative {
			t.Fatalf("registered project: %+v", p)
		}
		if !f.called("systemctl", "enable", "--now", "pm-custom.service") {
			t.Fatalf("expected persistent unit, got %v", f.calls)
		}
	})

	t.Run("known dir reuses the entry and remembers the new command", func(t *testing.T) {
		r, f := newTestRunner(t)
		dir := t.TempDir()
		_ = r.Store.Upsert(registry.Project{Name: "web", Dir: dir, Kind: registry.KindNative, Cmd: []string{"old"}})

		if _, err := r.RunAdhoc(dir, []string{"new", "cmd"}, AdhocOpts{}); err != nil {
			t.Fatalf("RunAdhoc: %v", err)
		}
		p, _ := r.Store.Get("web")
		if strings.Join(p.Cmd, " ") != "new cmd" {
			t.Fatalf("cmd = %v", p.Cmd)
		}
		if len(r.Store.List()) != 1 {
			t.Fatal("RunAdhoc duplicated the project")
		}
		if !f.called("systemd-run", "--unit=pm-web.service") {
			t.Fatalf("expected launch of web, got %v", f.calls)
		}
	})

	t.Run("without a command and nothing remembered it refuses", func(t *testing.T) {
		// Note: the detected stack's default command is intentionally not
		// used here; `pm run` requires an explicit command the first time.
		r, f := newTestRunner(t)
		dir := t.TempDir()
		writeFile(t, dir, "package.json", `{"devDependencies":{"vite":"5"}}`)

		if _, err := r.RunAdhoc(dir, nil, AdhocOpts{}); err == nil || !strings.Contains(err.Error(), "no command") {
			t.Fatalf("err = %v", err)
		}
		if len(r.Store.List()) != 0 {
			t.Fatal("a failed adhoc run must not register anything")
		}
		if f.called("systemd-run") {
			t.Fatal("nothing should be launched")
		}
	})
}
