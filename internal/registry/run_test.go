package registry

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
)

func runEntry(name, owner string, ports ...int) Project {
	return Project{
		Name: name, Dir: "/srv/" + name, Cmd: []string{"sleep", "100"},
		Kind: KindNative, Tier: TierEphemeral, Owner: owner,
		Run: &RunInfo{Ports: ports},
	}
}

func alive(Project) bool { return false } // replaceable: nothing is dead
func dead(Project) bool  { return true }

func TestAddRunPersistsAndSurvivesReopen(t *testing.T) {
	paths := tempPaths(t)
	s, _ := Open(paths)
	if err := s.AddRun(runEntry("x", "wt-qa", 4100), alive); err != nil {
		t.Fatalf("AddRun: %v", err)
	}
	again, _ := Open(paths)
	p, ok := again.Get("x")
	if !ok || !p.IsRun() || p.Owner != "wt-qa" || !slices.Equal(p.Run.Ports, []int{4100}) {
		t.Fatalf("reopened = %+v, %v", p, ok)
	}
}

func TestAddRunRefusesATakenName(t *testing.T) {
	s, _ := Open(tempPaths(t))
	_ = s.Upsert(Project{Name: "web", Dir: "/srv/web", Kind: KindNative, Tier: TierEphemeral})
	err := s.AddRun(runEntry("Web", "o"), dead)
	if err == nil || !strings.Contains(err.Error(), "registered project") {
		t.Fatalf("a registered project's name (any case) must be refused: %v", err)
	}

	if err := s.AddRun(runEntry("job", "o"), alive); err != nil {
		t.Fatal(err)
	}
	err = s.AddRun(runEntry("job", "o"), alive)
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("a live run's name must be refused: %v", err)
	}

	// the record of a dead run is replaced, not duplicated
	next := runEntry("job", "other", 4200)
	if err := s.AddRun(next, dead); err != nil {
		t.Fatalf("replacing a dead run: %v", err)
	}
	got, _ := s.Get("job")
	if got.Owner != "other" || len(s.List()) != 2 {
		t.Fatalf("after replace: %+v, %d entries", got, len(s.List()))
	}
}

func TestAddRunPortsAgainstLeasesAndOtherRuns(t *testing.T) {
	s, _ := Open(tempPaths(t))
	l, _ := s.AddLease("wt-qa", func(map[int]bool) ([]int, error) { return []int{4100, 4101}, nil })

	// leased to someone else: refused, naming the owner
	err := s.AddRun(runEntry("a", "wt-dev", 4100), alive)
	if err == nil || !strings.Contains(err.Error(), "leased to \"wt-qa\"") || !strings.Contains(err.Error(), l.ID) {
		t.Fatalf("port of another owner's lease: %v", err)
	}
	if _, ok := s.Get("a"); ok {
		t.Fatal("a refused run was recorded")
	}
	// leased to the run's own owner (any case): that is what leases are for
	if err := s.AddRun(runEntry("b", "WT-QA", 4100), alive); err != nil {
		t.Fatalf("own lease: %v", err)
	}
	// declared by another run: refused even for the same owner
	err = s.AddRun(runEntry("c", "wt-qa", 4100), alive)
	if err == nil || !strings.Contains(err.Error(), "already used by \"b\"") {
		t.Fatalf("port of another run: %v", err)
	}
	// but a run replacing itself keeps its own ports
	if err := s.AddRun(runEntry("b", "wt-qa", 4100), dead); err != nil {
		t.Fatalf("a replaced run must not collide with its old self: %v", err)
	}
}

func TestRunPortsAreReservedAgainstLeasesAndLaunches(t *testing.T) {
	s, _ := Open(tempPaths(t))
	if err := s.AddRun(runEntry("x", "o", 4000, 4001), alive); err != nil {
		t.Fatal(err)
	}
	if got := s.LeasedPorts(); !got[4000] || !got[4001] {
		t.Fatalf("LeasedPorts = %v, must include the run's ports", got)
	}
	l, err := s.AddLease("b", next(2))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(l.Ports, []int{4002, 4003}) {
		t.Fatalf("a lease picked %v: the run's ports must be skipped", l.Ports)
	}
	// stopping the run (Remove) frees them
	if err := s.Remove("x"); err != nil {
		t.Fatal(err)
	}
	if s.LeasedPorts()[4000] {
		t.Fatal("ports of a removed run are still reserved")
	}
}

// Many callers racing for one name and one port: exactly one wins.
func TestSimultaneousRunsNeverShareANameOrPort(t *testing.T) {
	paths := tempPaths(t)
	const racers = 30
	var wg sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	wonName, wonPort := 0, 0
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, _ := Open(paths) // each racer is its own process, in effect
			<-start
			if err := s.AddRun(runEntry("same", "o"), alive); err == nil {
				mu.Lock()
				wonName++
				mu.Unlock()
			}
			// distinct names, one port
			if err := s.AddRun(runEntry(fmt.Sprintf("p%d", i), "o", 5000), alive); err == nil {
				mu.Lock()
				wonPort++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if wonName != 1 || wonPort != 1 {
		t.Fatalf("name won %d times, port won %d times; want 1 and 1", wonName, wonPort)
	}
}

func TestAddRunValidates(t *testing.T) {
	s, _ := Open(tempPaths(t))
	if err := s.AddRun(Project{Name: "x"}, alive); err == nil {
		t.Fatal("an entry without Run must be refused")
	}
	bad := runEntry("x", "o")
	bad.Limits = Limits{Memory: "lots"}
	if err := s.AddRun(bad, alive); err == nil || !strings.Contains(err.Error(), "invalid memory limit") {
		t.Fatalf("limits are validated: %v", err)
	}
	if err := s.AddRun(runEntry("x", "has space"), alive); err == nil || !strings.Contains(err.Error(), "invalid owner") {
		t.Fatalf("owner is validated: %v", err)
	}
}

func TestRunEntriesAreNotCatalogEntries(t *testing.T) {
	s, _ := Open(tempPaths(t))
	if err := s.AddRun(runEntry("x", "o"), alive); err != nil {
		t.Fatal(err)
	}
	// Upsert cannot create one, nor overwrite one
	if err := s.Upsert(runEntry("y", "o")); err == nil {
		t.Fatal("Upsert created a run entry")
	}
	err := s.Upsert(Project{Name: "x", Dir: "/elsewhere", Kind: KindNative, Tier: TierEphemeral})
	if err == nil || !strings.Contains(err.Error(), "pm run") {
		t.Fatalf("Upsert over a run: %v", err)
	}
	// and its dir does not make it "the project" of that directory
	if p, ok := s.FindByDir("/srv/x"); ok {
		t.Fatalf("FindByDir matched the run %q", p.Name)
	}
}

func TestValidateRunName(t *testing.T) {
	for _, ok := range []string{"x", "api-qa", "a.b_c", "7up"} {
		if err := ValidateRunName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-x", "a b", "a/b", "a\nb", strings.Repeat("a", 65)} {
		if err := ValidateRunName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestProjectValidatesItsOwner(t *testing.T) {
	if err := (Project{Name: "x", Owner: "wt qa"}).Validate(); err == nil {
		t.Fatal("bad owner accepted")
	}
	if err := (Project{Name: "x"}).Validate(); err != nil {
		t.Fatalf("no owner is fine: %v", err)
	}
}
