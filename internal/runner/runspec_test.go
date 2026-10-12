package runner

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/antoniojosev/pm/internal/registry"
)

func specFor(t *testing.T) RunSpec {
	t.Helper()
	return RunSpec{Name: "x", Dir: t.TempDir(), Cmd: []string{"sleep", "100"}}
}

func TestLaunchSpecStartsATransientUnitAndRecordsTheRun(t *testing.T) {
	r, f := newTestRunner(t)
	spec := specFor(t)
	spec.Env = map[string]string{"PORT": "4100", "A": "1"}
	spec.Limits = registry.Limits{Memory: "256M", CPU: "150%"}
	spec.Owner = "wt-qa"
	spec.Ports = []int{4100}
	f.stdout["systemctl --user show pm-x.service -p MainPID --value"] = "777\n"

	res, err := r.LaunchSpec(spec)
	if err != nil {
		t.Fatalf("LaunchSpec: %v", err)
	}
	if res.Unit != "pm-x.service" || res.PID != 777 || !slices.Equal(res.Ports, []int{4100}) {
		t.Fatalf("result = %+v", res)
	}

	args := f.argsOf("systemd-run")
	for _, want := range []string{
		"--user", "--unit=pm-x.service", "--working-directory=" + spec.Dir, "--setenv=PM_PROJECT=x",
		"--setenv=A=1", "--setenv=PORT=4100", "StandardOutput=append:" + r.Paths.LogFile("x"),
		"MemoryMax=256M", "MemorySwapMax=0", "CPUQuota=150%",
	} {
		if !contains(args, want) {
			t.Errorf("systemd-run lacks %q: %v", want, args)
		}
	}
	if got := strings.Join(args[len(args)-3:], " "); got != "-- sleep 100" {
		t.Errorf("command not last: %v", args)
	}
	if args[len(args)-3] != "--" {
		t.Errorf("-- must separate the properties from the command: %v", args)
	}

	// recorded as a run with its owner, and with no trace of the environment
	p, ok := r.Store.Get("x")
	if !ok || !p.IsRun() || p.Owner != "wt-qa" || p.Tier != registry.TierEphemeral || p.Kind != registry.KindNative {
		t.Fatalf("stored = %+v, %v", p, ok)
	}
	if !slices.Equal(p.Run.Ports, []int{4100}) || p.Limits.Memory != "256M" || p.Dir != spec.Dir {
		t.Fatalf("stored = %+v", p)
	}
}

func TestLaunchSpecDefaultsOwnerAndReportsNoPortsAsEmpty(t *testing.T) {
	r, _ := newTestRunner(t)
	res, err := r.LaunchSpec(specFor(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.Ports == nil || len(res.Ports) != 0 {
		t.Fatalf("Ports must be an empty slice (JSON []), got %#v", res.Ports)
	}
	if p, _ := r.Store.Get("x"); p.Owner != registry.DefaultOwner {
		t.Fatalf("owner = %q, want the default %q", p.Owner, registry.DefaultOwner)
	}
}

func TestLaunchSpecNeverStoresTheEnvironment(t *testing.T) {
	r, _ := newTestRunner(t)
	spec := specFor(t)
	spec.Env = map[string]string{"DB_PASSWORD": "hunter2-very-secret"}
	if _, err := r.LaunchSpec(spec); err != nil {
		t.Fatal(err)
	}
	raw, err := readFile(r.Paths.Registry)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "hunter2") || strings.Contains(raw, "DB_PASSWORD") {
		t.Fatalf("the environment reached registry.json:\n%s", raw)
	}
}

func TestLaunchSpecUsesACustomLog(t *testing.T) {
	r, f := newTestRunner(t)
	spec := specFor(t)
	spec.Log = filepath.Join(t.TempDir(), "deep", "dir", "x.log")
	if _, err := r.LaunchSpec(spec); err != nil {
		t.Fatal(err)
	}
	args := f.argsOf("systemd-run")
	if !contains(args, "StandardOutput=append:"+spec.Log) || !contains(args, "StandardError=append:"+spec.Log) {
		t.Fatalf("custom log not used: %v", args)
	}
	if got := r.LogPath("x"); got != spec.Log {
		t.Fatalf("LogPath = %q, want %q", got, spec.Log)
	}
	if got := r.LogPath("someone-else"); got != r.Paths.LogFile("someone-else") {
		t.Fatalf("default LogPath = %q", got)
	}
	if !fileExists(filepath.Dir(spec.Log)) {
		t.Fatal("the log's directory was not created")
	}
}

func TestLaunchSpecRollsBackWhenSystemdRefuses(t *testing.T) {
	r, f := newTestRunner(t)
	f.fail["systemd-run"] = true
	_, err := r.LaunchSpec(specFor(t))
	if err == nil || !strings.Contains(err.Error(), "systemd-run failed") {
		t.Fatalf("err = %v", err)
	}
	if _, ok := r.Store.Get("x"); ok {
		t.Fatal("a run that never started was left in the registry")
	}
}

func TestLaunchSpecNameRules(t *testing.T) {
	r, f := newTestRunner(t)
	_ = r.Store.Upsert(registry.Project{Name: "web", Dir: t.TempDir(), Kind: registry.KindNative, Tier: registry.TierEphemeral})
	spec := specFor(t)
	spec.Name = "web"
	if _, err := r.LaunchSpec(spec); err == nil || !strings.Contains(err.Error(), "registered project") {
		t.Fatalf("a registered name: %v", err)
	}
	if f.called("systemd-run") {
		t.Fatal("systemd-run ran for a refused name")
	}

	// a run that is still alive keeps its name; once it is dead it can be replaced
	spec.Name = "job"
	if _, err := r.LaunchSpec(spec); err != nil {
		t.Fatal(err)
	}
	f.stdout["systemctl --user is-active pm-job.service"] = "active"
	if _, err := r.LaunchSpec(spec); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("a live run's name: %v", err)
	}
	f.stdout["systemctl --user is-active pm-job.service"] = "failed"
	if _, err := r.LaunchSpec(spec); err != nil {
		t.Fatalf("replacing a dead run: %v", err)
	}
	if !f.called("systemctl", "reset-failed", "pm-job.service") {
		t.Fatal("a failed unit must be reset before its name is reused")
	}
}

func TestRunSpecNormalize(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "afile")
	if err := writeFileBytes(file, "x"); err != nil {
		t.Fatal(err)
	}
	good := RunSpec{Name: "x", Dir: dir, Cmd: []string{"true"}}
	tests := []struct {
		name string
		mut  func(*RunSpec)
		want string // "" = valid
	}{
		{"valid", func(*RunSpec) {}, ""},
		{"no name", func(s *RunSpec) { s.Name = "" }, "name is required"},
		{"bad name", func(s *RunSpec) { s.Name = "a b" }, "invalid name"},
		{"no dir", func(s *RunSpec) { s.Dir = "" }, "dir is required"},
		{"relative dir", func(s *RunSpec) { s.Dir = "rel/dir" }, "absolute"},
		{"missing dir", func(s *RunSpec) { s.Dir = filepath.Join(dir, "nope") }, "not an existing directory"},
		{"dir is a file", func(s *RunSpec) { s.Dir = file }, "not an existing directory"},
		{"no cmd", func(s *RunSpec) { s.Cmd = nil }, "cmd is required"},
		{"empty program", func(s *RunSpec) { s.Cmd = []string{""} }, "cmd is required"},
		{"nul in cmd", func(s *RunSpec) { s.Cmd = []string{"a\x00b"} }, "invalid cmd"},
		{"bad env name", func(s *RunSpec) { s.Env = map[string]string{"1A": "x"} }, "invalid env name"},
		{"env name with =", func(s *RunSpec) { s.Env = map[string]string{"A=B": "x"} }, "invalid env name"},
		{"reserved env", func(s *RunSpec) { s.Env = map[string]string{"PM_PROJECT": "x"} }, "reserved"},
		{"nul in env value", func(s *RunSpec) { s.Env = map[string]string{"A": "x\x00"} }, "invalid value"},
		{"env value with newline is fine", func(s *RunSpec) { s.Env = map[string]string{"KEY": "a\nb"} }, ""},
		{"bad memory", func(s *RunSpec) { s.Limits.Memory = "1G\nExecStartPre=/evil" }, "invalid memory limit"},
		{"bad cpu", func(s *RunSpec) { s.Limits.CPU = "0%" }, "invalid cpu limit"},
		{"relative log", func(s *RunSpec) { s.Log = "x.log" }, "invalid log"},
		{"log with newline", func(s *RunSpec) { s.Log = "/tmp/a\nb.log" }, "invalid log"},
		{"log is a directory", func(s *RunSpec) { s.Log = dir }, "is a directory"},
		{"bad owner", func(s *RunSpec) { s.Owner = "a b" }, "invalid owner"},
		{"port 0", func(s *RunSpec) { s.Ports = []int{0} }, "invalid port"},
		{"port too big", func(s *RunSpec) { s.Ports = []int{65536} }, "invalid port"},
		{"duplicate port", func(s *RunSpec) { s.Ports = []int{4000, 4000} }, "listed twice"},
		{"too many ports", func(s *RunSpec) {
			for p := 1; p <= MaxRunPorts+1; p++ {
				s.Ports = append(s.Ports, p)
			}
		}, "too many ports"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := good
			tc.mut(&s)
			_, err := s.Normalize()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestInvalidSpecTouchesNothing(t *testing.T) {
	r, f := newTestRunner(t)
	spec := specFor(t)
	spec.Limits.Memory = "lots"
	if _, err := r.LaunchSpec(spec); err == nil {
		t.Fatal("expected an error")
	}
	if len(f.calls) != 0 {
		t.Fatalf("external programs were invoked: %v", f.calls)
	}
	if len(r.Store.List()) != 0 {
		t.Fatal("an invalid spec was recorded")
	}
}

// pm stop forgets a run, and only a run.
func TestStopForgetsARun(t *testing.T) {
	r, f := newTestRunner(t)
	if _, err := r.LaunchSpec(specFor(t)); err != nil {
		t.Fatal(err)
	}
	unitLoaded(f, "x")
	if err := r.Stop("x"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !f.called("systemctl", "stop", "pm-x.service") {
		t.Fatal("the unit was not stopped")
	}
	if _, ok := r.Store.Get("x"); ok {
		t.Fatal("a stopped run is still in the registry")
	}
}

func TestStopOfARunWhoseProcessAlreadyExitedOnlyForgetsIt(t *testing.T) {
	r, f := newTestRunner(t)
	if _, err := r.LaunchSpec(specFor(t)); err != nil {
		t.Fatal(err)
	}
	// no unit is loaded any more, and nothing listens: an unmanaged stop would fail
	if err := r.Stop("x"); err != nil {
		t.Fatalf("Stop of a dead run: %v", err)
	}
	if f.called("systemctl", "stop", "pm-x.service") {
		t.Fatal("stopped a unit that is not loaded")
	}
	if _, ok := r.Store.Get("x"); ok {
		t.Fatal("record not forgotten")
	}
}

func TestStopKeepsARegisteredProject(t *testing.T) {
	r, f := newTestRunner(t)
	_ = r.Store.Upsert(registry.Project{Name: "web", Dir: t.TempDir(), Kind: registry.KindNative, Tier: registry.TierEphemeral, Cmd: []string{"true"}})
	unitLoaded(f, "web")
	if err := r.Stop("web"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Store.Get("web"); !ok {
		t.Fatal("stopping a registered project must not unregister it")
	}
}

func TestARunCannotBeStartedRestartedOrPromoted(t *testing.T) {
	r, f := newTestRunner(t)
	if _, err := r.LaunchSpec(specFor(t)); err != nil {
		t.Fatal(err)
	}
	before := len(f.calls)
	if _, err := r.Start("x", LaunchOpts{}); err == nil || !strings.Contains(err.Error(), "pm run") {
		t.Fatalf("Start: %v", err)
	}
	if _, err := r.Restart("x", LaunchOpts{}); err == nil || !strings.Contains(err.Error(), "pm run") {
		t.Fatalf("Restart: %v", err)
	}
	if err := r.Promote("x"); err == nil || !strings.Contains(err.Error(), "pm run") {
		t.Fatalf("Promote: %v", err)
	}
	if err := r.Demote("x"); err == nil || !strings.Contains(err.Error(), "pm run") {
		t.Fatalf("Demote: %v", err)
	}
	if len(f.calls) != before {
		t.Fatalf("a refused verb still reached systemd: %v", f.calls[before:])
	}
	// Restart must not have stopped (and so forgotten) the run
	if _, ok := r.Store.Get("x"); !ok {
		t.Fatal("a refused restart dropped the run")
	}
}

func TestUnitStatesParsesActiveFailedAndPID(t *testing.T) {
	r, f := newTestRunner(t)
	f.stdout["systemctl --user show pm-*.service -p Id -p ActiveState -p Result -p MainPID"] = "" +
		"MainPID=0\nResult=oom-kill\nId=pm-api.service\nActiveState=failed\n\n" +
		"MainPID=4321\nResult=success\nId=pm-web.service\nActiveState=active\n"
	got := r.UnitStates()
	if got["pm-api.service"] != (UnitState{Active: "failed", Result: "oom-kill"}) {
		t.Fatalf("api = %+v", got["pm-api.service"])
	}
	if got["pm-web.service"] != (UnitState{Active: "active", Result: "success", PID: 4321}) {
		t.Fatalf("web = %+v", got["pm-web.service"])
	}
}

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}

func writeFileBytes(path, content string) error { return os.WriteFile(path, []byte(content), 0o644) }

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
