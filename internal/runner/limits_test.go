package runner

import (
	"os"
	"strings"
	"testing"

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
	got := limitProps(registry.Limits{Memory: "256M", CPU: "150%"})
	if strings.Join(got, " ") != "MemoryMax=256M CPUQuota=150%" {
		t.Fatalf("props = %v", got)
	}
	if got := limitProps(registry.Limits{CPU: "50%"}); strings.Join(got, " ") != "CPUQuota=50%" {
		t.Fatalf("cpu only = %v", got)
	}
}

func TestTransientLaunchPassesLimitsToSystemdRun(t *testing.T) {
	r, f := newTestRunner(t)
	_ = r.Store.Upsert(limitedProject(t.TempDir(), registry.TierEphemeral, registry.Limits{Memory: "256M", CPU: "150%"}))

	if _, err := r.Start("api", LaunchOpts{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	props := systemdRunProps(f)
	for _, want := range []string{"MemoryMax=256M", "CPUQuota=150%"} {
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
		if strings.HasPrefix(a, "MemoryMax=") && (sep == -1 || i > sep) {
			t.Fatalf("MemoryMax= landed after --: %v", args)
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
		if strings.HasPrefix(p, "MemoryMax=") || strings.HasPrefix(p, "CPUQuota=") {
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
	for _, want := range []string{"MemoryMax=512M\n", "CPUQuota=200%\n"} {
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
	if strings.Contains(string(b), "MemoryMax=") || strings.Contains(string(b), "CPUQuota=") {
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
