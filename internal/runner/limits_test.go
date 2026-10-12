package runner

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/pm/internal/detector"
	"github.com/antoniojosev/pm/internal/registry"
)

// systemdRunProps returns every "-p" value passed to the first systemd-run call.
func systemdRunProps(f *fakeExec) []string {
	args := f.argsOf("systemd-run")
	var props []string
	for i, a := range args {
		if a == "-p" && i+1 < len(args) {
			props = append(props, args[i+1])
		}
	}
	return props
}

func hasProp(props []string, want string) bool { return contains(props, want) }

func limitedProject(dir string, tier registry.Tier, l registry.Limits) registry.Project {
	return registry.Project{
		Name: "api", Dir: dir, Kind: registry.KindNative, Tier: tier,
		Cmd: []string{"./serve"}, Limits: l,
	}
}

func TestLimitProps(t *testing.T) {
	if got := limitProps(registry.Limits{}); len(got) != 0 {
		t.Fatalf("no limits must yield no properties, got %v", got)
	}
	// A memory cap also forbids swap: without MemorySwapMax=0 a process over
	// MemoryMax= is paged out on a host with swap (WSL2) and never killed.
	got := limitProps(registry.Limits{Memory: "256M", CPU: "150%"})
	if strings.Join(got, " ") != "MemoryMax=256M MemorySwapMax=0 CPUQuota=150%" {
		t.Fatalf("props = %v", got)
	}
	if got := limitProps(registry.Limits{CPU: "50%"}); strings.Join(got, " ") != "CPUQuota=50%" {
		t.Fatalf("cpu only must not touch swap: %v", got)
	}
	if got := limitProps(registry.Limits{Memory: "50%"}); strings.Join(got, " ") != "MemoryMax=50% MemorySwapMax=0" {
		t.Fatalf("percentage = %v", got)
	}
	if got := limitProps(registry.Limits{Memory: "infinity"}); strings.Join(got, " ") != "MemoryMax=infinity" {
		t.Fatalf("infinity means no cap, so no swap restriction either: %v", got)
	}
}

func TestTransientLaunchPassesLimitsToSystemdRun(t *testing.T) {
	r, f := newTestRunner(t)
	_ = r.Store.Upsert(limitedProject(t.TempDir(), registry.TierEphemeral, registry.Limits{Memory: "256M", CPU: "150%"}))

	if _, err := r.Start("api", LaunchOpts{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	props := systemdRunProps(f)
	for _, want := range []string{"MemoryMax=256M", "MemorySwapMax=0", "CPUQuota=150%"} {
		if !hasProp(props, want) {
			t.Errorf("systemd-run lacks -p %s; props = %v", want, props)
		}
	}
	// Limits are properties, so they must come before the command separator.
	args := f.argsOf("systemd-run")
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
		}
	}
	for i, a := range args {
		if (strings.HasPrefix(a, "MemoryMax=") || strings.HasPrefix(a, "MemorySwapMax=")) && (sep == -1 || i > sep) {
			t.Fatalf("%s landed after --: %v", a, args)
		}
	}
}

func TestTransientLaunchWithoutLimitsAddsNoResourceProperties(t *testing.T) {
	r, f := newTestRunner(t)
	_ = r.Store.Upsert(limitedProject(t.TempDir(), registry.TierEphemeral, registry.Limits{}))

	if _, err := r.Start("api", LaunchOpts{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, p := range systemdRunProps(f) {
		if strings.HasPrefix(p, "MemoryMax=") || strings.HasPrefix(p, "MemorySwapMax=") || strings.HasPrefix(p, "CPUQuota=") {
			t.Fatalf("unexpected resource property %q", p)
		}
	}
}

func TestLaunchOptsLimitsOverrideProjectLimitsPerField(t *testing.T) {
	r, f := newTestRunner(t)
	_ = r.Store.Upsert(limitedProject(t.TempDir(), registry.TierEphemeral, registry.Limits{Memory: "256M", CPU: "50%"}))

	if _, err := r.Start("api", LaunchOpts{Limits: registry.Limits{Memory: "2G"}}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	props := systemdRunProps(f)
	if !hasProp(props, "MemoryMax=2G") || hasProp(props, "MemoryMax=256M") {
		t.Errorf("memory override not applied: %v", props)
	}
	if !hasProp(props, "CPUQuota=50%") {
		t.Errorf("cpu from the project should be kept: %v", props)
	}
	// A start override is for that start only.
	p, _ := r.Store.Get("api")
	if p.Limits.Memory != "256M" {
		t.Fatalf("a start override must not be persisted, stored %+v", p.Limits)
	}
}

func TestServiceTierUnitCarriesLimits(t *testing.T) {
	r, f := newTestRunner(t)
	_ = r.Store.Upsert(limitedProject(t.TempDir(), registry.TierService, registry.Limits{Memory: "512M", CPU: "200%"}))

	if _, err := r.Start("api", LaunchOpts{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	b, err := os.ReadFile(r.unitPath("api"))
	if err != nil {
		t.Fatalf("unit not written: %v", err)
	}
	unit := string(b)
	svc := unit[strings.Index(unit, "[Service]"):strings.Index(unit, "[Install]")]
	for _, want := range []string{"MemoryMax=512M\n", "MemorySwapMax=0\n", "CPUQuota=200%\n"} {
		if !strings.Contains(svc, want) {
			t.Errorf("[Service] lacks %q:\n%s", want, unit)
		}
	}
	if f.called("systemd-run") {
		t.Fatal("service tier must not use systemd-run")
	}
}

func TestUnitWithoutLimitsHasNoResourceDirectives(t *testing.T) {
	r, _ := newTestRunner(t)
	if err := r.writeUnit(registry.Project{Name: "web", Dir: "/srv/web"}, []string{"true"}, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(r.unitPath("web"))
	if strings.Contains(string(b), "MemoryMax=") || strings.Contains(string(b), "MemorySwapMax=") || strings.Contains(string(b), "CPUQuota=") {
		t.Fatalf("unit has resource directives without limits:\n%s", b)
	}
}

func TestLaunchRefusesInvalidLimitsWithoutTouchingSystemd(t *testing.T) {
	r, f := newTestRunner(t)
	_ = r.Store.Upsert(limitedProject(t.TempDir(), registry.TierEphemeral, registry.Limits{}))

	_, err := r.Start("api", LaunchOpts{Limits: registry.Limits{Memory: "1G\nExecStartPre=/bin/evil"}})
	if err == nil || !strings.Contains(err.Error(), "invalid memory limit") {
		t.Fatalf("err = %v", err)
	}
	if f.called("systemd-run") || f.called("systemctl") {
		t.Fatalf("systemd was invoked despite invalid limits: %v", f.calls)
	}
}

func TestRunAdhocRemembersLimits(t *testing.T) {
	r, f := newTestRunner(t)
	dir := t.TempDir()

	if _, err := r.RunAdhoc(dir, []string{"./serve"}, AdhocOpts{
		Name:       "adhoc",
		LaunchOpts: LaunchOpts{Limits: registry.Limits{Memory: "128M"}},
	}); err != nil {
		t.Fatalf("RunAdhoc: %v", err)
	}
	if !hasProp(systemdRunProps(f), "MemoryMax=128M") {
		t.Fatalf("limit not passed on run: %v", systemdRunProps(f))
	}
	p, _ := r.Store.Get("adhoc")
	if p.Limits.Memory != "128M" {
		t.Fatalf("run did not remember the limit: %+v", p.Limits)
	}
}

func TestRunAdhocRejectsInvalidLimitsBeforeRegistering(t *testing.T) {
	r, f := newTestRunner(t)
	_, err := r.RunAdhoc(t.TempDir(), []string{"./serve"}, AdhocOpts{
		Name:       "bad",
		LaunchOpts: LaunchOpts{Limits: registry.Limits{CPU: "lots"}},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid cpu limit") {
		t.Fatalf("err = %v", err)
	}
	if _, ok := r.Store.Get("bad"); ok {
		t.Fatal("project registered despite invalid limits")
	}
	if f.called("systemd-run") {
		t.Fatal("systemd-run was invoked")
	}
}

// callIndex returns the position of the first recorded call to name that
// contains every fragment as a whole argument, or -1.
func (f *fakeExec) callIndex(name string, fragments ...string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.calls {
		if c[0] != name {
			continue
		}
		ok := true
		for _, frag := range fragments {
			if !contains(c[1:], frag) {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

func TestTransientLaunchClearsAFailedUnitFirst(t *testing.T) {
	// After the kernel OOM-kills a unit, systemd keeps it loaded as failed and
	// systemd-run refuses to reuse the name until reset-failed runs.
	r, f := newTestRunner(t)
	_ = r.Store.Upsert(limitedProject(t.TempDir(), registry.TierEphemeral, registry.Limits{Memory: "64M"}))

	if _, err := r.Start("api", LaunchOpts{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	reset := f.callIndex("systemctl", "reset-failed", "pm-api.service")
	run := f.callIndex("systemd-run")
	if reset == -1 || run == -1 || reset > run {
		t.Fatalf("reset-failed (call %d) must precede systemd-run (call %d): %v", reset, run, f.calls)
	}
}

func TestTransientLaunchSurvivesResetFailedErrors(t *testing.T) {
	// Nothing failed to reset is the usual case; systemctl may well error.
	r, f := newTestRunner(t)
	_ = r.Store.Upsert(limitedProject(t.TempDir(), registry.TierEphemeral, registry.Limits{}))
	f.fail["systemctl --user reset-failed pm-api.service"] = true

	if _, err := r.Start("api", LaunchOpts{}); err != nil {
		t.Fatalf("a failing reset-failed must not block the launch: %v", err)
	}
	if !f.called("systemd-run") {
		t.Fatal("systemd-run was not invoked")
	}
}

func TestFailedUnitsReportsOnlyFailedPmUnits(t *testing.T) {
	r, f := newTestRunner(t)
	f.stdout["systemctl --user show pm-*.service -p Id -p ActiveState -p Result"] = "" +
		"Result=oom-kill\nId=pm-api.service\nActiveState=failed\n\n" +
		"Result=success\nId=pm-web.service\nActiveState=active\n\n" +
		"Result=exit-code\nId=pm-job.service\nActiveState=failed\n"

	got := r.FailedUnits()
	if len(got) != 2 || got["pm-api.service"] != "oom-kill" || got["pm-job.service"] != "exit-code" {
		t.Fatalf("FailedUnits = %v", got)
	}
	if _, ok := got["pm-web.service"]; ok {
		t.Fatal("an active unit is not failed")
	}
}

func TestFailedUnitsEmptyWhenSystemctlFails(t *testing.T) {
	r, f := newTestRunner(t)
	f.fail["systemctl"] = true
	if got := r.FailedUnits(); len(got) != 0 {
		t.Fatalf("FailedUnits = %v", got)
	}
}

func TestStartRefusesLimitOverrideOnServiceTier(t *testing.T) {
	// The service-tier unit is a file enabled at boot: an override written into
	// it would survive every restart, against what --mem promises on start.
	r, f := newTestRunner(t)
	_ = r.Store.Upsert(limitedProject(t.TempDir(), registry.TierService, registry.Limits{Memory: "512M"}))

	_, err := r.Start("api", LaunchOpts{Limits: registry.Limits{Memory: "2G"}})
	if err == nil || !strings.Contains(err.Error(), "pm edit") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(r.unitPath("api")); statErr == nil {
		t.Fatal("a unit file was written for a refused start")
	}
	if f.called("systemctl") || f.called("systemd-run") {
		t.Fatalf("systemd was touched: %v", f.calls)
	}
}

func TestServiceTierStartKeepsProjectLimitsInUnit(t *testing.T) {
	r, _ := newTestRunner(t)
	_ = r.Store.Upsert(limitedProject(t.TempDir(), registry.TierService, registry.Limits{Memory: "512M", CPU: "200%"}))

	// Start twice, as a restart would: the unit must keep the stored limits.
	for i := 0; i < 2; i++ {
		if _, err := r.Start("api", LaunchOpts{}); err != nil {
			t.Fatalf("Start: %v", err)
		}
	}
	b, _ := os.ReadFile(r.unitPath("api"))
	for _, want := range []string{"MemoryMax=512M\n", "CPUQuota=200%\n"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("unit lacks %q:\n%s", want, b)
		}
	}
}

func TestRunAdhocAlwaysWithLimitsIsNotAnOverride(t *testing.T) {
	// `pm run --always --mem 1G` stores the limit and launches the service
	// tier; it must not trip the refusal meant for `pm start --mem`.
	r, _ := newTestRunner(t)
	if _, err := r.RunAdhoc(t.TempDir(), []string{"./serve"}, AdhocOpts{
		Name: "svc", Always: true,
		LaunchOpts: LaunchOpts{Limits: registry.Limits{Memory: "1G"}},
	}); err != nil {
		t.Fatalf("RunAdhoc: %v", err)
	}
	b, _ := os.ReadFile(r.unitPath("svc"))
	if !strings.Contains(string(b), "MemoryMax=1G\n") {
		t.Fatalf("unit lacks the limit:\n%s", b)
	}
}

func TestDockerProjectsRefuseLimits(t *testing.T) {
	r, f := newTestRunner(t)
	dir := t.TempDir()
	// The registry refuses to store the combination...
	err := r.Store.Upsert(registry.Project{
		Name: "db", Dir: dir, Kind: registry.KindDocker, Cmd: []string{"docker", "compose", "up"},
		Limits: registry.Limits{Memory: "1G"},
	})
	if err == nil || !strings.Contains(err.Error(), "docker") {
		t.Fatalf("Upsert err = %v", err)
	}
	// ...and a start-time override on a stored docker project is refused too.
	_ = r.Store.Upsert(registry.Project{Name: "db", Dir: dir, Kind: registry.KindDocker, Cmd: []string{"docker", "compose", "up"}})
	_, err = r.Start("db", LaunchOpts{Limits: registry.Limits{CPU: "50%"}})
	if err == nil || !strings.Contains(err.Error(), "docker") {
		t.Fatalf("Start err = %v", err)
	}
	if f.called("systemd-run") {
		t.Fatal("systemd-run was invoked for a refused start")
	}
}

func TestRestartInstanceKeepsRegisteredLimits(t *testing.T) {
	r, f := newTestRunner(t)
	child := spawnSleep(t)
	pid := child.Process.Pid
	for i := 0; i < 20 && len(procArgv(pid)) == 0; i++ {
		time.Sleep(25 * time.Millisecond)
	}
	cwd := t.TempDir()
	_ = r.Store.Upsert(registry.Project{
		Name: "api", Dir: cwd, Kind: registry.KindNative, Cmd: []string{"sleep", "30"},
		Limits: registry.Limits{Memory: "256M", CPU: "50%"},
	})
	r.Snap = func() ([]detector.Instance, error) {
		return []detector.Instance{{Project: "api", Port: 4321, PID: pid, Cwd: cwd}}, nil
	}

	if _, err := r.RestartInstance(4321); err != nil {
		t.Fatalf("RestartInstance: %v", err)
	}
	props := systemdRunProps(f)
	for _, want := range []string{"MemoryMax=256M", "MemorySwapMax=0", "CPUQuota=50%"} {
		if !hasProp(props, want) {
			t.Errorf("relaunch lacks -p %s: %v", want, props)
		}
	}
	_ = child.Wait()
}
