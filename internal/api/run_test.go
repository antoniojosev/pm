package api

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/antoniojosev/pm/internal/detector"
	"github.com/antoniojosev/pm/internal/registry"
	"github.com/antoniojosev/pm/internal/runner"
	"github.com/antoniojosev/pm/internal/testfake"
)

// runService is a Service whose systemd is the testfake pair.
func runService(t *testing.T) (*Service, *testfake.Systemd) {
	t.Helper()
	svc := newTestService(t)
	allFree(svc)
	return svc, testfake.Install(t)
}

func runSpec(t *testing.T, name string, ports ...int) runner.RunSpec {
	t.Helper()
	return runner.RunSpec{Name: name, Dir: t.TempDir(), Cmd: []string{"sleep", "100"}, Ports: ports}
}

func rowByName(t *testing.T, svc *Service, name string) Row {
	t.Helper()
	rows, err := svc.PS()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no row %q in %+v", name, rows)
	return Row{}
}

// The criterion of the step: run, see it in pm ls with its owner, stop it.
func TestRunAppearsInPSWithItsOwnerAndStopsWithStop(t *testing.T) {
	svc, sd := runService(t)
	spec := runSpec(t, "x", 4100)
	spec.Owner = "wt-qa"

	res, err := svc.StartRun(spec)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if res.Unit != "pm-x.service" || res.PID != testfake.MainPID || !slices.Equal(res.Ports, []int{4100}) {
		t.Fatalf("result = %+v", res)
	}
	if !sd.Active("pm-x.service") {
		t.Fatal("systemd-run did not start the unit")
	}

	// nothing listens (a `sleep`), and the row is still up: the unit says so
	r := rowByName(t, svc, "x")
	if r.Status != "up" || r.Owner != "wt-qa" || r.Tier != "ephemeral" || r.PID != testfake.MainPID ||
		r.Unit != "pm-x.service" || r.Port != 4100 || r.Cmd != "sleep 100" {
		t.Fatalf("row = %+v", r)
	}

	if err := svc.Stop("x"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if sd.Active("pm-x.service") {
		t.Fatal("the unit is still running")
	}
	rows, _ := svc.PS()
	for _, r := range rows {
		if r.Name == "x" {
			t.Fatalf("a stopped run is still listed: %+v", r)
		}
	}
}

func TestRunWithoutPortsAndOwnerUsesDefaults(t *testing.T) {
	svc, _ := runService(t)
	res, err := svc.StartRun(runSpec(t, "x"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Ports == nil || len(res.Ports) != 0 {
		t.Fatalf("ports = %#v", res.Ports)
	}
	r := rowByName(t, svc, "x")
	if r.Owner != registry.DefaultOwner || r.Port != 0 || r.LocalURL != "" {
		t.Fatalf("row = %+v", r)
	}
}

func TestRunWhoseProcessDiedShowsAsDownUntilStopped(t *testing.T) {
	svc, sd := runService(t)
	if _, err := svc.StartRun(runSpec(t, "x")); err != nil {
		t.Fatal(err)
	}
	sd.Kill("pm-x.service") // exited on its own
	if r := rowByName(t, svc, "x"); r.Status != "down" || r.Owner == "" {
		t.Fatalf("row = %+v", r)
	}
	if err := svc.Stop("x"); err != nil {
		t.Fatalf("stopping a run that already exited: %v", err)
	}
	rows, _ := svc.PS()
	if len(rows) != 0 {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestRunAdoptsAListenerOnItsPort(t *testing.T) {
	svc, _ := runService(t)
	if _, err := svc.StartRun(runSpec(t, "x", 4100)); err != nil {
		t.Fatal(err)
	}
	// the process binds a different port than declared; attribution by marker wins
	live(svc, detector.Instance{Project: "x", Port: 4555, PID: 99, Source: detector.SourceMarker})
	r := rowByName(t, svc, "x")
	if r.Status != "up" || r.Port != 4555 || r.PID != 99 || r.Owner == "" {
		t.Fatalf("row = %+v", r)
	}
}

func TestRunRefusesPortsInUse(t *testing.T) {
	svc, sd := runService(t)
	live(svc, detector.Instance{Project: "other", Port: 4100, PID: 7})
	_, err := svc.StartRun(runSpec(t, "a", 4101, 4100))
	if err == nil || !strings.Contains(err.Error(), "port 4100 is in use") {
		t.Fatalf("a port with a listener: %v", err)
	}

	svc.PortFree = func(p int) bool { return p != 4200 } // ss cannot see it, a bind can
	_, err = svc.StartRun(runSpec(t, "b", 4200))
	if err == nil || !strings.Contains(err.Error(), "port 4200 is not free") {
		t.Fatalf("a port that cannot be bound: %v", err)
	}
	if len(sd.Calls("systemd-run")) != 0 {
		t.Fatal("systemd-run ran for a refused run")
	}
	if len(svc.Store.List()) != 0 {
		t.Fatal("a refused run was recorded")
	}
}

func TestRunUsesARealBindTest(t *testing.T) {
	svc := newTestService(t) // PortFree stays ports.Free
	testfake.Install(t)
	port := listenOn(t)
	_, err := svc.StartRun(runSpec(t, "x", port)) // ss is blind (Snap reports nothing)
	if err == nil || !strings.Contains(err.Error(), "is not free") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunPortsAgainstLeases(t *testing.T) {
	svc, _ := runService(t)
	lease, err := svc.CreateLease(LeaseRequest{Owner: "wt-qa", Count: 2, Prefer: 4100})
	if err != nil {
		t.Fatal(err)
	}

	// someone else's lease: refused
	other := runSpec(t, "a", lease.Ports[0])
	other.Owner = "wt-dev"
	if _, err := svc.StartRun(other); err == nil || !strings.Contains(err.Error(), "leased to") {
		t.Fatalf("another owner's lease: %v", err)
	}
	// its own lease: that is the intended flow
	mine := runSpec(t, "b", lease.Ports...)
	mine.Owner = "wt-qa"
	res, err := svc.StartRun(mine)
	if err != nil {
		t.Fatalf("own lease: %v", err)
	}
	if !slices.Equal(res.Ports, lease.Ports) {
		t.Fatalf("ports = %v", res.Ports)
	}

	// releasing the lease does not free the ports while the run holds them
	if _, err := svc.ReleaseLease(lease.ID); err != nil {
		t.Fatal(err)
	}
	next, err := svc.CreateLease(LeaseRequest{Owner: "x", Count: 2, Prefer: 4100})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(next.Ports, 4100) || slices.Contains(next.Ports, 4101) {
		t.Fatalf("a lease received %v, ports a run holds", next.Ports)
	}
	// once the run is stopped they come back
	if err := svc.Stop("b"); err != nil {
		t.Fatal(err)
	}
	again, _ := svc.CreateLease(LeaseRequest{Owner: "y", Count: 2, Prefer: 4100})
	if !slices.Equal(again.Ports, []int{4100, 4101}) {
		t.Fatalf("after stop: %v", again.Ports)
	}
}

func TestRunNameErrorsComeBeforePortErrors(t *testing.T) {
	svc, _ := runService(t)
	if _, err := svc.StartRun(runSpec(t, "x", 4100)); err != nil {
		t.Fatal(err)
	}
	live(svc, detector.Instance{Project: "x", Port: 4100, PID: 7, Source: detector.SourceMarker})
	_, err := svc.StartRun(runSpec(t, "x", 4100))
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("err = %v: the name is the problem, not the port its owner holds", err)
	}

	_ = svc.Add(registry.Project{Name: "web", Dir: "/srv/web"})
	if _, err := svc.StartRun(runSpec(t, "web")); err == nil || !strings.Contains(err.Error(), "registered project") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunRefusesInvalidSpecsBeforeTouchingAnything(t *testing.T) {
	svc, sd := runService(t)
	for name, mut := range map[string]func(*runner.RunSpec){
		"no cmd":    func(s *runner.RunSpec) { s.Cmd = nil },
		"bad owner": func(s *runner.RunSpec) { s.Owner = "no good" },
		"bad limit": func(s *runner.RunSpec) { s.Limits.CPU = "x" },
		"bad env":   func(s *runner.RunSpec) { s.Env = map[string]string{"PM_PROJECT": "other"} },
	} {
		spec := runSpec(t, "x")
		mut(&spec)
		if _, err := svc.StartRun(spec); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(sd.Calls("systemd-run")) != 0 || len(svc.Store.List()) != 0 {
		t.Fatal("an invalid spec had effects")
	}
}

func TestRunLogsComeFromTheRunsLogFile(t *testing.T) {
	svc, _ := runService(t)
	spec := runSpec(t, "x")
	spec.Log = filepath.Join(t.TempDir(), "x.log")
	if _, err := svc.StartRun(spec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spec.Log, []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := svc.Logs("x", 10)
	if err != nil || !strings.Contains(out, "two") {
		t.Fatalf("Logs = %q, %v", out, err)
	}
	if svc.LogPath("x") != spec.Log {
		t.Fatalf("LogPath = %q", svc.LogPath("x"))
	}
}

func TestRemoveOfARunStopsAndForgetsIt(t *testing.T) {
	svc, sd := runService(t)
	if _, err := svc.StartRun(runSpec(t, "x")); err != nil {
		t.Fatal(err)
	}
	if err := svc.Remove("x"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if sd.Active("pm-x.service") || len(svc.Store.List()) != 0 {
		t.Fatal("run survived Remove")
	}
	if err := svc.Remove("x"); err == nil {
		t.Fatal("removing something that does not exist should still fail")
	}
}

func TestRunNameCannotBeTakenByRegisteringWhileItLives(t *testing.T) {
	svc, _ := runService(t)
	if _, err := svc.StartRun(runSpec(t, "x")); err != nil {
		t.Fatal(err)
	}
	if err := svc.Add(registry.Project{Name: "x", Dir: "/srv/x"}); err == nil || !strings.Contains(err.Error(), "pm run") {
		t.Fatalf("Add over a run: %v", err)
	}
}

// listenOn binds a free port on all interfaces for the length of the test and
// returns it.
func listenOn(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l.Addr().(*net.TCPAddr).Port
}

// Simultaneous runs asking for the same port: only one gets it.
func TestSimultaneousRunsNeverShareAPort(t *testing.T) {
	svc, _ := runService(t)
	const racers = 12
	dirs := make([]string, racers)
	for i := range dirs {
		dirs[i] = t.TempDir()
	}
	results := make(chan error, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		go func() {
			<-start
			_, err := svc.StartRun(runner.RunSpec{
				Name: "r" + string(rune('a'+i)), Dir: dirs[i], Cmd: []string{"sleep", "1"}, Ports: []int{4100},
			})
			results <- err
		}()
	}
	close(start)
	won := 0
	for i := 0; i < racers; i++ {
		if err := <-results; err == nil {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d runs got port 4100, want exactly 1", won)
	}
}
