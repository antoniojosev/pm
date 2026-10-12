package api

import (
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antoniojosev/pm/internal/detector"
	"github.com/antoniojosev/pm/internal/registry"
)

// allFree makes every port pass the bind test, so a test controls the outcome
// with live listeners and other leases only.
func allFree(svc *Service) { svc.PortFree = func(int) bool { return true } }

func TestCreateLeaseDefaults(t *testing.T) {
	svc := newTestService(t)
	allFree(svc)

	l, err := svc.CreateLease(LeaseRequest{Owner: "wt-qa"})
	if err != nil {
		t.Fatal(err)
	}
	if l.Owner != "wt-qa" || !slices.Equal(l.Ports, []int{DefaultLeaseBase}) || l.ID == "" {
		t.Fatalf("lease = %+v", l)
	}
	l, err = svc.CreateLease(LeaseRequest{Owner: "wt-qa", Count: 3, Prefer: 4100})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(l.Ports, []int{4100, 4101, 4102}) {
		t.Fatalf("ports = %v", l.Ports)
	}
}

func TestCreateLeaseSkipsLiveBoundAndLeasedPorts(t *testing.T) {
	svc := newTestService(t)
	live(svc, detector.Instance{Project: "x", Port: 4101, PID: 1})
	svc.PortFree = func(p int) bool { return p != 4102 } // ss cannot see 4102, a bind can

	first, err := svc.CreateLease(LeaseRequest{Owner: "a", Count: 2, Prefer: 4100})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(first.Ports, []int{4100, 4103}) {
		t.Fatalf("ports = %v, want 4101 (listening) and 4102 (not bindable) skipped", first.Ports)
	}
	second, err := svc.CreateLease(LeaseRequest{Owner: "b", Count: 2, Prefer: 4100})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(second.Ports, []int{4104, 4105}) {
		t.Fatalf("ports = %v, want the first lease's ports skipped", second.Ports)
	}
}

func TestCreateLeaseUsesARealBindTest(t *testing.T) {
	svc := newTestService(t) // PortFree stays ports.Free
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	taken := l.Addr().(*net.TCPAddr).Port

	// ss is blind here (Snap reports nothing), only the bind test sees it
	got, err := svc.CreateLease(LeaseRequest{Owner: "a", Count: 2, Prefer: taken})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(got.Ports, taken) {
		t.Fatalf("leased %d, which a process is listening on", taken)
	}
}

func TestCreateLeaseValidation(t *testing.T) {
	svc := newTestService(t)
	allFree(svc)
	tests := []struct {
		name string
		req  LeaseRequest
		want string
	}{
		{"no owner", LeaseRequest{Count: 1}, "owner is required"},
		{"bad owner", LeaseRequest{Owner: "a b"}, "invalid owner"},
		{"negative count", LeaseRequest{Owner: "a", Count: -1}, "invalid count"},
		{"too many", LeaseRequest{Owner: "a", Count: MaxLeaseCount + 1}, "invalid count"},
		{"negative prefer", LeaseRequest{Owner: "a", Prefer: -5}, "invalid prefer"},
		{"prefer out of range", LeaseRequest{Owner: "a", Prefer: 70000}, "invalid prefer"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.CreateLease(tc.req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	if n := len(svc.Leases("")); n != 0 {
		t.Fatalf("rejected requests stored %d leases", n)
	}
}

func TestCreateLeaseRunsOutOfPorts(t *testing.T) {
	svc := newTestService(t)
	svc.PortFree = func(p int) bool { return p == 4100 } // only one port is bindable
	if _, err := svc.CreateLease(LeaseRequest{Owner: "a", Count: 2, Prefer: 4100}); err == nil || !strings.Contains(err.Error(), "could not find 2 free ports") {
		t.Fatalf("err = %v", err)
	}
	// nothing half-leased: the one free port is still available
	l, err := svc.CreateLease(LeaseRequest{Owner: "a", Count: 1, Prefer: 4100})
	if err != nil || !slices.Equal(l.Ports, []int{4100}) {
		t.Fatalf("after failure: %+v, %v", l, err)
	}
	// the top of the range ends the scan instead of overflowing it
	svc.PortFree = func(int) bool { return true }
	if _, err := svc.CreateLease(LeaseRequest{Owner: "a", Count: 3, Prefer: 65535}); err == nil {
		t.Fatal("a lease cannot go past port 65535")
	}
}

func TestLeasesFilterAndRelease(t *testing.T) {
	svc := newTestService(t)
	allFree(svc)
	a, _ := svc.CreateLease(LeaseRequest{Owner: "wt-qa"})
	b, _ := svc.CreateLease(LeaseRequest{Owner: "wt-dev"})

	if got := svc.Leases(""); len(got) != 2 {
		t.Fatalf("all = %+v", got)
	}
	if got := svc.Leases("WT-QA"); len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("by owner = %+v", got)
	}
	if got := svc.Leases("nobody"); got == nil || len(got) != 0 {
		t.Fatalf("unknown owner = %#v, want an empty non-nil slice (JSON [])", got)
	}
	if _, err := svc.ReleaseLease(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReleaseLease(a.ID); err == nil {
		t.Fatal("releasing twice must fail")
	}
	if got := svc.Leases(""); len(got) != 1 || got[0].ID != b.ID {
		t.Fatalf("after release = %+v", got)
	}
}

// Leases made by another process (the CLI) appear without restarting anything.
func TestLeasesSeeOtherProcessesChanges(t *testing.T) {
	svc := newTestService(t)
	other, err := registry.Open(svc.Paths)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := other.AddLease("cli", func(map[int]bool) ([]int, error) { return []int{4100}, nil })
	if got := svc.Leases(""); len(got) != 1 || got[0].ID != l.ID {
		t.Fatalf("leases = %+v", got)
	}
}

// The core guarantee: simultaneous requests never receive the same port, even
// though every one of them scans the same candidates from the same start.
func TestConcurrentLeasesNeverShareAPort(t *testing.T) {
	svc := newTestService(t)
	allFree(svc)
	const requests, count = 40, 3

	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan []int, requests)
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			l, err := svc.CreateLease(LeaseRequest{Owner: "racer", Count: count, Prefer: 4100})
			if err != nil {
				t.Error(err)
				return
			}
			results <- l.Ports
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	seen := map[int]bool{}
	for ports := range results {
		for _, p := range ports {
			if seen[p] {
				t.Fatalf("port %d was leased twice", p)
			}
			seen[p] = true
		}
	}
	if len(seen) != requests*count {
		t.Fatalf("%d distinct ports, want %d", len(seen), requests*count)
	}
}

func TestLeaseChangedWakesOnCreateAndRelease(t *testing.T) {
	svc := newTestService(t)
	allFree(svc)

	wake := svc.LeaseChanged()
	select {
	case <-wake:
		t.Fatal("woken with nothing changed")
	default:
	}
	l, err := svc.CreateLease(LeaseRequest{Owner: "a"})
	if err != nil {
		t.Fatal(err)
	}
	waitClosed(t, wake, "create")

	wake = svc.LeaseChanged()
	if _, err := svc.ReleaseLease(l.ID); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, wake, "release")

	// a failed request is not a change
	wake = svc.LeaseChanged()
	_, _ = svc.CreateLease(LeaseRequest{})
	_, _ = svc.ReleaseLease("l-nope")
	select {
	case <-wake:
		t.Fatal("woken by a failed request")
	default:
	}
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("not woken after %s", what)
	}
}
