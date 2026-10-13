package runner

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/antoniojosev/pm/internal/detector"
	"github.com/antoniojosev/pm/internal/registry"
)

// ownedRun stores a run record for owner, the way LaunchSpec leaves it.
func ownedRun(t *testing.T, r *Runner, name, owner string) {
	t.Helper()
	p := RunSpec{Name: name, Dir: t.TempDir(), Cmd: []string{"sleep", "100"}, Owner: owner}.project()
	if err := r.Store.AddRun(p, func(registry.Project) bool { return true }); err != nil {
		t.Fatal(err)
	}
}

func TestLaunchInjectsThePMOwnerMarker(t *testing.T) {
	r, f := newTestRunner(t)
	spec := specFor(t)
	spec.Owner = "wt-qa"
	if _, err := r.LaunchSpec(spec); err != nil {
		t.Fatal(err)
	}
	args := f.argsOf("systemd-run")
	if !contains(args, "--setenv=PM_OWNER=wt-qa") || !contains(args, "--setenv=PM_PROJECT=x") {
		t.Fatalf("systemd-run lacks the markers: %v", args)
	}

	// a registered project without an owner carries no PM_OWNER at all
	r2, f2 := newTestRunner(t)
	_ = r2.Store.Upsert(registry.Project{Name: "web", Dir: t.TempDir(), Kind: registry.KindNative, Tier: registry.TierEphemeral, Cmd: []string{"true"}})
	if _, err := r2.Start("web", LaunchOpts{}); err != nil {
		t.Fatal(err)
	}
	for _, a := range f2.argsOf("systemd-run") {
		if strings.Contains(a, "PM_OWNER") {
			t.Fatalf("an ownerless project got %q", a)
		}
	}

	// and one that has an owner gets it too
	r3, f3 := newTestRunner(t)
	_ = r3.Store.Upsert(registry.Project{Name: "web", Dir: t.TempDir(), Kind: registry.KindNative, Tier: registry.TierEphemeral, Cmd: []string{"true"}, Owner: "wt-dev"})
	if _, err := r3.Start("web", LaunchOpts{}); err != nil {
		t.Fatal(err)
	}
	if !contains(f3.argsOf("systemd-run"), "--setenv=PM_OWNER=wt-dev") {
		t.Fatalf("registered project of an owner lacks the marker: %v", f3.argsOf("systemd-run"))
	}
}

func TestPersistentUnitCarriesThePMOwnerMarker(t *testing.T) {
	r, _ := newTestRunner(t)
	if err := r.writeUnit(registry.Project{Name: "web", Dir: "/srv/web", Owner: "wt-qa"}, []string{"true"}, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := readFile(r.unitPath("web"))
	if err != nil || !strings.Contains(raw, "Environment=PM_OWNER=wt-qa\n") {
		t.Fatalf("unit = %q, %v", raw, err)
	}
	if err := r.writeUnit(registry.Project{Name: "web", Dir: "/srv/web"}, []string{"true"}, nil); err != nil {
		t.Fatal(err)
	}
	if raw, _ := readFile(r.unitPath("web")); strings.Contains(raw, "PM_OWNER") {
		t.Fatalf("ownerless unit mentions PM_OWNER:\n%s", raw)
	}
}

func TestRunSpecReservesPMOwner(t *testing.T) {
	spec := specFor(t)
	spec.Env = map[string]string{"PM_OWNER": "someone-else"}
	if _, err := spec.Normalize(); err == nil || !strings.Contains(err.Error(), "PM_OWNER is reserved") {
		t.Fatalf("err = %v", err)
	}
}

// waitForArgv blocks until /proc/<pid>/cmdline of pid is readable.
func waitForArgv(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for len(procArgv(pid)) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("/proc/%d/cmdline stayed empty", pid)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRestartInstanceKeepsTheOwnerMarker(t *testing.T) {
	r, f := newTestRunner(t)
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	// Right after Start the kernel may still be swapping the process image, and
	// /proc/<pid>/cmdline reads empty for a moment: wait until it has an argv
	// instead of racing it.
	waitForArgv(t, cmd.Process.Pid)
	cwd, _ := os.Getwd()
	r.Snap = func() ([]detector.Instance, error) {
		return []detector.Instance{{Port: 4000, PID: cmd.Process.Pid, Cwd: cwd, Owner: "wt-qa"}}, nil
	}
	if _, err := r.RestartInstance(4000); err != nil {
		t.Fatal(err)
	}
	if !contains(f.argsOf("systemd-run"), "--setenv=PM_OWNER=wt-qa") {
		t.Fatalf("relaunch lost the owner: %v", f.argsOf("systemd-run"))
	}
}

func TestStopOwnerStopsProjectsRunsAndMarkedProcesses(t *testing.T) {
	r, f := newTestRunner(t)

	// a running registered project of the owner
	_ = r.Store.Upsert(registry.Project{Name: "web", Dir: t.TempDir(), Kind: registry.KindNative, Tier: registry.TierEphemeral, Cmd: []string{"true"}, Owner: "wt-qa"})
	f.stdout["systemctl --user is-active pm-web.service"] = "active"
	unitLoaded(f, "web")
	// a registered project of the owner that is not running: left alone
	_ = r.Store.Upsert(registry.Project{Name: "idle", Dir: t.TempDir(), Kind: registry.KindNative, Tier: registry.TierEphemeral, Cmd: []string{"true"}, Owner: "WT-QA"})
	// a run of the owner (its unit is long gone: stopping still forgets it)
	ownedRun(t, r, "api", "wt-qa")
	// someone else's, which must survive
	ownedRun(t, r, "other-run", "wt-dev")
	_ = r.Store.Upsert(registry.Project{Name: "mine", Dir: t.TempDir(), Kind: registry.KindNative, Tier: registry.TierEphemeral, Cmd: []string{"true"}})

	// a process a script left running with the marker, on two ports; and one of
	// another owner
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _, _ = child.Process.Wait() })
	r.Snap = func() ([]detector.Instance, error) {
		return []detector.Instance{
			{Project: "web", Port: 3000, PID: 11, Owner: "wt-qa"}, // the project's own listener: not stopped twice
			{Port: 4000, PID: child.Process.Pid, Owner: "wt-qa"},
			{Port: 4001, PID: child.Process.Pid, Owner: "wt-qa"},
			{Port: 5000, PID: bogusPID, Owner: "wt-dev"},
			{Port: 5001, PID: bogusPID},
			{Port: 6000, Docker: true, Container: "abc123", Owner: "wt-qa"},
		}, nil
	}

	stopped, errs := r.StopOwner("wt-qa")
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	got := map[string]bool{}
	for _, s := range stopped {
		got[s.Kind+":"+s.Name] = true
	}
	for _, want := range []string{"project:web", "run:api", "container:abc123"} {
		if !got[want] {
			t.Errorf("%s not reported stopped: %+v", want, stopped)
		}
	}
	pids := 0
	for _, s := range stopped {
		if s.Kind == StoppedProcess {
			pids++
			if s.PID != child.Process.Pid {
				t.Errorf("process %d stopped, want %d", s.PID, child.Process.Pid)
			}
		}
	}
	if pids != 1 || len(stopped) != 4 {
		t.Fatalf("stopped = %+v, want web, api, the process once, the container", stopped)
	}
	if !f.called("systemctl", "stop", "pm-web.service") || !f.called("docker", "stop", "abc123") {
		t.Fatalf("calls = %v", f.calls)
	}
	if f.called("systemctl", "stop", "pm-idle.service") {
		t.Fatal("a project that is not running was stopped")
	}
	// the process really died
	done := make(chan struct{})
	go func() { _, _ = child.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the marked process is still alive")
	}

	// no trace of the run; the others stay
	names := []string{}
	for _, p := range r.Store.List() {
		names = append(names, p.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"idle", "mine", "other-run", "web"}) {
		t.Fatalf("registry = %v: the run must be forgotten, the catalog and other owners kept", names)
	}
}

func TestStopOwnerWithNothingIsEmpty(t *testing.T) {
	r, _ := newTestRunner(t)
	stopped, errs := r.StopOwner("nobody")
	if len(stopped) != 0 || len(errs) != 0 {
		t.Fatalf("stopped = %+v, errs = %v", stopped, errs)
	}
}

func TestStopOwnerKeepsGoingAfterAFailure(t *testing.T) {
	r, f := newTestRunner(t)
	ownedRun(t, r, "a", "wt-qa")
	ownedRun(t, r, "b", "wt-qa")
	unitLoaded(f, "a")
	unitLoaded(f, "b")
	f.fail["systemctl --user stop pm-a.service"] = true

	stopped, errs := r.StopOwner("wt-qa")
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "stop a") {
		t.Fatalf("errs = %v", errs)
	}
	if len(stopped) != 1 || stopped[0].Name != "b" {
		t.Fatalf("stopped = %+v, want b only", stopped)
	}
	if _, ok := r.Store.Get("a"); !ok {
		t.Fatal("the run that could not be stopped must stay recorded")
	}
	if _, ok := r.Store.Get("b"); ok {
		t.Fatal("the run that was stopped must be forgotten")
	}
}

func TestStopOwnerNeverKillsPm(t *testing.T) {
	r, _ := newTestRunner(t)
	r.Snap = func() ([]detector.Instance, error) {
		return []detector.Instance{{Port: 9797, PID: os.Getpid(), Owner: "wt-qa"}}, nil
	}
	stopped, errs := r.StopOwner("wt-qa")
	if len(stopped) != 0 || len(errs) != 0 {
		t.Fatalf("stopped = %+v, %v", stopped, errs)
	}
	if err := syscall.Kill(os.Getpid(), 0); err != nil {
		t.Fatal(err)
	}
}
