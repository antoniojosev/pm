package api

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/pm/internal/detector"
	"github.com/antoniojosev/pm/internal/registry"
	"github.com/antoniojosev/pm/internal/runner"
)

// startOwned starts two runs of owner, holding the ports of a lease, plus a
// run of someone else with a lease of its own.
func startOwned(t *testing.T, svc *Service, owner string) registry.Lease {
	t.Helper()
	lease, err := svc.CreateLease(LeaseRequest{Owner: owner, Count: 2, Prefer: 4100})
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"api-" + owner, "web-" + owner} {
		spec := runSpec(t, name, lease.Ports[i])
		spec.Owner = owner
		if _, err := svc.StartRun(spec); err != nil {
			t.Fatal(err)
		}
	}
	return lease
}

// The criterion of the step: turning an owner off leaves pm ps and pm lease ls
// with no trace of it, and leaves other owners alone.
func TestDeleteOwnerLeavesNoTraceAndKeepsOthers(t *testing.T) {
	svc, sd := runService(t)
	startOwned(t, svc, "wt-qa")
	keep := startOwned(t, svc, "wt-dev")

	res, err := svc.DeleteOwner("wt-qa")
	if err != nil {
		t.Fatalf("DeleteOwner: %v", err)
	}
	if res.Owner != "wt-qa" || len(res.Stopped) != 2 || len(res.LeasesReleased) != 1 || len(res.Errors) != 0 {
		t.Fatalf("result = %+v", res)
	}
	for _, s := range res.Stopped {
		if s.Kind != runner.StoppedRun {
			t.Errorf("stopped %+v, want runs", s)
		}
	}
	if sd.Active("pm-api-wt-qa.service") || sd.Active("pm-web-wt-qa.service") {
		t.Fatal("the units are still running")
	}
	if !sd.Active("pm-api-wt-dev.service") || !sd.Active("pm-web-wt-dev.service") {
		t.Fatal("another owner's units were stopped")
	}

	rows, _ := svc.PS()
	for _, r := range rows {
		if r.Owner == "wt-qa" || strings.Contains(r.Name, "wt-qa") {
			t.Fatalf("pm ps still shows %+v", r)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want the two of wt-dev", rows)
	}
	leases := svc.Leases("")
	if len(leases) != 1 || leases[0].ID != keep.ID {
		t.Fatalf("leases = %+v, want only wt-dev's", leases)
	}
	// the freed ports are handed out again
	// (wt-qa leased 4100-4101 first, wt-dev 4102-4103)
	again, err := svc.CreateLease(LeaseRequest{Owner: "next", Count: 2, Prefer: 4100})
	if err != nil || !slices.Equal(again.Ports, []int{4100, 4101}) {
		t.Fatalf("lease after delete = %+v, %v; keep = %v", again, err, keep.Ports)
	}
}

func TestStopOwnerKeepsItsLeases(t *testing.T) {
	svc, sd := runService(t)
	lease := startOwned(t, svc, "wt-qa")

	res, err := svc.StopOwner("wt-qa")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stopped) != 2 || len(res.LeasesReleased) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if sd.Active("pm-api-wt-qa.service") {
		t.Fatal("still running")
	}
	if got := svc.Leases("wt-qa"); len(got) != 1 || got[0].ID != lease.ID {
		t.Fatalf("leases = %+v, want the lease kept", got)
	}
	rows, _ := svc.PS()
	if len(rows) != 0 {
		t.Fatalf("rows = %+v", rows)
	}
	// stopping again, with nothing left, is not an error
	res, err = svc.StopOwner("wt-qa")
	if err != nil || len(res.Stopped) != 0 || res.Stopped == nil {
		t.Fatalf("second stop = %+v, %v", res, err)
	}
}

func TestDeleteOwnerWithNothingIsNotAnError(t *testing.T) {
	svc, _ := runService(t)
	res, err := svc.DeleteOwner("ghost")
	if err != nil || res.Owner != "ghost" || res.Stopped == nil || res.LeasesReleased == nil || len(res.Stopped)+len(res.LeasesReleased) != 0 {
		t.Fatalf("= %+v, %v", res, err)
	}
}

func TestOwnerValidation(t *testing.T) {
	svc, _ := runService(t)
	for _, owner := range []string{"", "a b", "../x", "-x"} {
		if _, err := svc.StopOwner(owner); err == nil || !strings.Contains(err.Error(), "owner") {
			t.Errorf("StopOwner(%q) = %v", owner, err)
		}
		if _, err := svc.DeleteOwner(owner); err == nil {
			t.Errorf("DeleteOwner(%q) succeeded", owner)
		}
	}
}

// A run that cannot be stopped keeps its owner's leases: the process may still
// hold their ports. The rest is stopped and reported, and a retry finishes.
func TestDeleteOwnerKeepsLeasesWhenSomethingWontStop(t *testing.T) {
	svc, sd := runService(t)
	startOwned(t, svc, "wt-qa")
	sd.FailStop(t)

	res, err := svc.DeleteOwner("wt-qa")
	if err == nil || !strings.Contains(err.Error(), "leases were kept") {
		t.Fatalf("err = %v", err)
	}
	if len(res.Errors) != 2 || len(res.LeasesReleased) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if got := svc.Leases("wt-qa"); len(got) != 1 {
		t.Fatalf("leases = %+v, want kept", got)
	}
}

// Adoption by marker: a process nobody registered that carries PM_OWNER shows
// in pm ps with its owner, and is stopped with the rest of the owner.
func TestMarkedUnclaimedProcessShowsItsOwnerAndStopsWithIt(t *testing.T) {
	svc, _ := runService(t)
	startOwned(t, svc, "wt-qa")
	child := startSleeper(t)
	live(svc,
		detector.Instance{Port: 4500, PID: child.Process.Pid, Owner: "wt-qa", Cmd: "sleep 30", Source: detector.SourceUnknown},
		detector.Instance{Port: 4501, PID: 1 << 30, Owner: "wt-dev"},
	)
	rows, _ := svc.PS()
	var marked, other *Row
	for i := range rows {
		switch rows[i].Port {
		case 4500:
			marked = &rows[i]
		case 4501:
			other = &rows[i]
		}
	}
	if marked == nil || marked.Status != "unclaimed" || marked.Owner != "wt-qa" || other == nil || other.Owner != "wt-dev" {
		t.Fatalf("rows = %+v", rows)
	}

	res, err := svc.DeleteOwner("wt-qa")
	if err != nil {
		t.Fatal(err)
	}
	var proc []int
	for _, s := range res.Stopped {
		if s.Kind == runner.StoppedProcess {
			proc = append(proc, s.PID)
		}
	}
	if !slices.Equal(proc, []int{child.Process.Pid}) || len(res.Stopped) != 3 {
		t.Fatalf("stopped = %+v", res.Stopped)
	}
	waitDead(t, child)
}

// Claiming such a process keeps its owner, so pm still groups it.
func TestClaimKeepsTheOwnerMarker(t *testing.T) {
	svc, _ := runService(t)
	live(svc, detector.Instance{Port: 4500, PID: 1 << 30, Owner: "wt-qa", Cwd: t.TempDir()})
	p, err := svc.Claim(4500, "adopted")
	if err != nil || p.Owner != "wt-qa" {
		t.Fatalf("claim = %+v, %v", p, err)
	}
	if got, _ := svc.Store.Get("adopted"); got.Owner != "wt-qa" {
		t.Fatalf("stored = %+v", got)
	}
}

func TestOwnerEventsArePublished(t *testing.T) {
	svc, _ := runService(t)
	events, cancel := svc.SubscribeOwnerEvents()
	defer cancel()
	startOwned(t, svc, "wt-qa")

	next := func() OwnerEvent {
		t.Helper()
		select {
		case ev := <-events:
			return ev
		case <-time.After(2 * time.Second):
			t.Fatal("no owner event")
			return OwnerEvent{}
		}
	}
	if _, err := svc.StopOwner("wt-qa"); err != nil {
		t.Fatal(err)
	}
	if ev := next(); ev.Name != EventOwnerStopped || ev.Result.Owner != "wt-qa" || len(ev.Result.Stopped) != 2 {
		t.Fatalf("event = %+v", ev)
	}
	if _, err := svc.DeleteOwner("wt-qa"); err != nil {
		t.Fatal(err)
	}
	if ev := next(); ev.Name != EventOwnerDeleted || len(ev.Result.LeasesReleased) != 1 || len(ev.Result.Stopped) != 0 {
		t.Fatalf("event = %+v", ev)
	}
	// nothing to do publishes nothing
	if _, err := svc.DeleteOwner("wt-qa"); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-events:
		t.Fatalf("unexpected event %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}

	// after cancel nobody is told, and nothing blocks
	cancel()
	startOwned(t, svc, "wt-qa")
	if _, err := svc.StopOwner("wt-qa"); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-events:
		t.Fatalf("event after cancel: %+v", ev)
	default:
	}
}

func startSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn sleep: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	return cmd
}

func waitDead(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the process is still alive")
	}
}
