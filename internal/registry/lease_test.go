package registry

import (
	"slices"
	"strings"
	"sync"
	"testing"
)

// next hands out consecutive ports starting after the highest one leased.
func next(n int) func(map[int]bool) ([]int, error) {
	return func(leased map[int]bool) ([]int, error) {
		var out []int
		for p := 4000; len(out) < n; p++ {
			if !leased[p] {
				out = append(out, p)
			}
		}
		return out, nil
	}
}

func TestAddLeasePersistsAndListsCopies(t *testing.T) {
	paths := tempPaths(t)
	s, _ := Open(paths)

	l, err := s.AddLease("wt-qa", next(2))
	if err != nil {
		t.Fatalf("AddLease: %v", err)
	}
	if !strings.HasPrefix(l.ID, "l-") || len(l.ID) != 14 || l.Owner != "wt-qa" || !slices.Equal(l.Ports, []int{4000, 4001}) || l.CreatedAt.IsZero() {
		t.Fatalf("lease = %+v", l)
	}

	// a fresh process sees it
	again, _ := Open(paths)
	got, ok := again.GetLease(l.ID)
	if !ok || !slices.Equal(got.Ports, l.Ports) || !got.CreatedAt.Equal(l.CreatedAt) {
		t.Fatalf("reopened lease = %+v, %v", got, ok)
	}
	if !again.LeasedPorts()[4001] {
		t.Fatal("LeasedPorts misses a leased port")
	}

	// callers get copies: scribbling on them does not touch the store
	list := s.Leases()
	list[0].Ports[0] = 1
	if s.Leases()[0].Ports[0] != 4000 {
		t.Fatal("Leases returned the store's own slice")
	}
	l.Ports[0] = 2
	if s.Leases()[0].Ports[0] != 4000 {
		t.Fatal("AddLease returned the store's own slice")
	}
}

func TestAddLeasePickSeesExistingLeases(t *testing.T) {
	s, _ := Open(tempPaths(t))
	a, _ := s.AddLease("a", next(2))
	b, err := s.AddLease("b", next(2))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Equal(a.Ports, b.Ports) || !slices.Equal(b.Ports, []int{4002, 4003}) {
		t.Fatalf("a=%v b=%v", a.Ports, b.Ports)
	}
}

func TestAddLeaseRejectsBadInput(t *testing.T) {
	s, _ := Open(tempPaths(t))
	for _, owner := range []string{"", " ", "a b", "-x", "a/b", "a\nb", `a"b`, strings.Repeat("x", MaxOwnerLen+1)} {
		if _, err := s.AddLease(owner, next(1)); err == nil {
			t.Errorf("owner %q accepted", owner)
		}
	}
	for _, owner := range []string{"wt-qa", "A.b_c-9", strings.Repeat("x", MaxOwnerLen)} {
		if err := ValidateOwner(owner); err != nil {
			t.Errorf("owner %q rejected: %v", owner, err)
		}
	}
	if _, err := s.AddLease("a", func(map[int]bool) ([]int, error) { return nil, nil }); err == nil {
		t.Error("an empty pick must not store a lease")
	}
	if len(s.Leases()) != 0 {
		t.Fatalf("failed adds left leases behind: %+v", s.Leases())
	}
}

func TestAddLeasePickErrorStoresNothing(t *testing.T) {
	paths := tempPaths(t)
	s, _ := Open(paths)
	_, err := s.AddLease("a", func(map[int]bool) ([]int, error) { return nil, errTest })
	if err != errTest {
		t.Fatalf("err = %v", err)
	}
	if reopened, _ := Open(paths); len(reopened.Leases()) != 0 {
		t.Fatal("a failed pick stored a lease")
	}
}

type testErr string

func (e testErr) Error() string { return string(e) }

const errTest = testErr("no ports")

func TestReleaseLease(t *testing.T) {
	paths := tempPaths(t)
	s, _ := Open(paths)
	a, _ := s.AddLease("a", next(1))
	b, _ := s.AddLease("b", next(1))

	got, err := s.ReleaseLease(a.ID)
	if err != nil || got.ID != a.ID || !slices.Equal(got.Ports, a.Ports) {
		t.Fatalf("ReleaseLease = %+v, %v", got, err)
	}
	if _, err := s.ReleaseLease(a.ID); err == nil {
		t.Fatal("releasing twice must fail")
	}
	reopened, _ := Open(paths)
	list := reopened.Leases()
	if len(list) != 1 || list[0].ID != b.ID {
		t.Fatalf("leases after release = %+v", list)
	}
	// the freed port is offered again
	c, _ := s.AddLease("c", next(1))
	if !slices.Equal(c.Ports, a.Ports) {
		t.Fatalf("freed port not reused: %v", c.Ports)
	}
}

// Two stores on one file stand for the CLI and the daemon. The writes must not
// erase each other: this is what the reload-under-lock in update() is for.
func TestLeasesSurviveAnotherProcessesWrites(t *testing.T) {
	paths := tempPaths(t)
	daemon, _ := Open(paths)
	cli, _ := Open(paths)

	l, err := cli.AddLease("wt-qa", next(1))
	if err != nil {
		t.Fatal(err)
	}
	// the daemon never reloaded, yet its own writes keep the lease
	if err := daemon.Upsert(Project{Name: "web"}); err != nil {
		t.Fatal(err)
	}
	if err := daemon.UpsertGroup(Group{Name: "g"}); err != nil {
		t.Fatal(err)
	}
	reopened, _ := Open(paths)
	if _, ok := reopened.GetLease(l.ID); !ok {
		t.Fatal("a stale process erased the lease")
	}
	// and the other way round: the daemon's project survives the CLI's lease
	if _, err := cli.AddLease("wt-qa", next(1)); err != nil {
		t.Fatal(err)
	}
	reopened, _ = Open(paths)
	if _, ok := reopened.Get("web"); !ok || len(reopened.Leases()) != 2 {
		t.Fatalf("state after interleaved writes: %d leases, web present %v", len(reopened.Leases()), ok)
	}
	// Remove from a stale view works on the file's state, not the stale copy
	if err := daemon.Remove("web"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if reopened, _ = Open(paths); len(reopened.Leases()) != 2 {
		t.Fatal("Remove from a stale store dropped leases")
	}
}

func TestFailedMutationLeavesStoreOnDisk(t *testing.T) {
	paths := tempPaths(t)
	s, _ := Open(paths)
	_ = s.Upsert(Project{Name: "keep"})
	if err := s.Remove("ghost"); err == nil {
		t.Fatal("removing an unknown project must fail")
	}
	if _, ok := s.Get("keep"); !ok {
		t.Fatal("a failed Remove lost a project")
	}
}

// Many writers through several stores (as many processes): no port is ever
// handed out twice and every lease is stored.
func TestConcurrentLeasesNeverShareAPort(t *testing.T) {
	paths := tempPaths(t)
	const stores, perStore, count = 4, 10, 3
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[int]string{}
	for i := 0; i < stores; i++ {
		s, _ := Open(paths)
		for j := 0; j < perStore; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				l, err := s.AddLease("o", next(count))
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				for _, p := range l.Ports {
					if other, dup := seen[p]; dup {
						t.Errorf("port %d given to %s and %s", p, other, l.ID)
					}
					seen[p] = l.ID
				}
			}()
		}
	}
	wg.Wait()
	final, _ := Open(paths)
	if got := len(final.Leases()); got != stores*perStore {
		t.Fatalf("stored %d leases, want %d", got, stores*perStore)
	}
	if len(seen) != stores*perStore*count {
		t.Fatalf("%d distinct ports, want %d", len(seen), stores*perStore*count)
	}
}

func TestReleaseOwnerLeasesTakesOnlyThatOwnersAndFreesItsPorts(t *testing.T) {
	paths := tempPaths(t)
	s, _ := Open(paths)
	a1, _ := s.AddLease("wt-qa", next(2))
	other, _ := s.AddLease("wt-dev", next(1))
	a2, _ := s.AddLease("WT-QA", next(1)) // owners compare case-insensitively

	released, err := s.ReleaseOwnerLeases("wt-qa")
	if err != nil {
		t.Fatal(err)
	}
	if len(released) != 2 || released[0].ID != a1.ID || released[1].ID != a2.ID || !slices.Equal(released[0].Ports, a1.Ports) {
		t.Fatalf("released = %+v, want the two leases of wt-qa, oldest first", released)
	}
	left := s.Leases()
	if len(left) != 1 || left[0].ID != other.ID {
		t.Fatalf("left = %+v", left)
	}
	// persisted, and the ports are free again
	again, _ := Open(paths)
	if got := again.Leases(); len(got) != 1 || got[0].ID != other.ID {
		t.Fatalf("reopened = %+v", got)
	}
	if again.LeasedPorts()[a1.Ports[0]] {
		t.Fatal("a released port is still reserved")
	}

	// an owner with no leases is not an error
	if released, err := s.ReleaseOwnerLeases("nobody"); err != nil || len(released) != 0 {
		t.Fatalf("nobody = %+v, %v", released, err)
	}
}
