package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antoniojosev/pm/internal/api"
	"github.com/antoniojosev/pm/internal/registry"
)

func decodeLease(t *testing.T, raw string) registry.Lease {
	t.Helper()
	var l registry.Lease
	if err := json.Unmarshal([]byte(raw), &l); err != nil {
		t.Fatalf("not a lease: %s (%v)", raw, err)
	}
	return l
}

func TestLeaseLifecycleOverHTTP(t *testing.T) {
	d, srv := newTestDaemon(t)
	d.svc.PortFree = func(int) bool { return true }

	code, _, raw := do(t, srv, "POST", "/api/v1/leases", `{"owner":"wt-qa","count":2,"prefer":4100}`)
	l := decodeLease(t, raw)
	if code != 200 || l.Owner != "wt-qa" || !slices.Equal(l.Ports, []int{4100, 4101}) || l.ID == "" {
		t.Fatalf("create = %d %s", code, raw)
	}
	assertConforms(t, "POST", "/leases", raw)

	code, _, raw = do(t, srv, "POST", "/api/v1/leases", `{"owner":"wt-dev"}`)
	other := decodeLease(t, raw)
	if code != 200 || !slices.Equal(other.Ports, []int{20000}) {
		t.Fatalf("second create = %d %s", code, raw)
	}

	code, _, raw = do(t, srv, "GET", "/api/v1/leases", "")
	var all []registry.Lease
	if err := json.Unmarshal([]byte(raw), &all); err != nil || code != 200 || len(all) != 2 {
		t.Fatalf("list = %d %s (%v)", code, raw, err)
	}
	assertConforms(t, "GET", "/leases", raw)

	_, _, raw = do(t, srv, "GET", "/api/v1/leases?owner=wt-qa", "")
	var mine []registry.Lease
	_ = json.Unmarshal([]byte(raw), &mine)
	if len(mine) != 1 || mine[0].ID != l.ID {
		t.Fatalf("filtered list = %s", raw)
	}
	_, _, raw = do(t, srv, "GET", "/api/v1/leases?owner=nobody", "")
	if strings.TrimSpace(raw) != "[]" {
		t.Fatalf("no leases must be [], got %s", raw)
	}

	code, obj, _ := do(t, srv, "DELETE", "/api/v1/leases/"+l.ID, "")
	if code != 200 || obj["ok"] != true {
		t.Fatalf("delete = %d %v", code, obj)
	}
	code, obj, _ = do(t, srv, "DELETE", "/api/v1/leases/"+l.ID, "")
	if code != 400 || !strings.Contains(obj["error"].(string), "does not exist") {
		t.Fatalf("delete twice = %d %v", code, obj)
	}
	// the freed ports are handed out again
	_, _, raw = do(t, srv, "POST", "/api/v1/leases", `{"owner":"wt-qa","count":2,"prefer":4100}`)
	if again := decodeLease(t, raw); !slices.Equal(again.Ports, []int{4100, 4101}) {
		t.Fatalf("after release: %s", raw)
	}

	// the deprecated alias serves the same routes
	code, _, _ = do(t, srv, "GET", "/api/leases", "")
	if code != 200 {
		t.Fatalf("GET /api/leases = %d", code)
	}
}

func TestLeaseRequestErrorsAreReported(t *testing.T) {
	_, srv := newTestDaemon(t)
	tests := []struct{ name, body, want string }{
		{"bad json", `{nope`, ""},
		{"no owner", `{"count":1}`, "owner is required"},
		{"bad owner", `{"owner":"a b"}`, "invalid owner"},
		{"bad count", `{"owner":"a","count":-2}`, "invalid count"},
		{"bad prefer", `{"owner":"a","prefer":99999}`, "invalid prefer"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, obj, raw := do(t, srv, "POST", "/api/v1/leases", tc.body)
			msg, _ := obj["error"].(string)
			if code != 400 || msg == "" || !strings.Contains(msg, tc.want) {
				t.Fatalf("= %d %s", code, raw)
			}
		})
	}
}

// Two (here forty) requests at the same instant, over real HTTP, never get the
// same port.
func TestSimultaneousLeaseRequestsNeverShareAPort(t *testing.T) {
	d, srv := newTestDaemon(t)
	d.svc.PortFree = func(int) bool { return true }

	const requests, count = 40, 2
	var wg sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	seen := map[int]string{}
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := srv.Client().Post(srv.URL+"/api/v1/leases", "application/json",
				strings.NewReader(`{"owner":"racer","count":2,"prefer":4100}`))
			if err != nil {
				t.Error(err)
				return
			}
			defer res.Body.Close()
			var l registry.Lease
			if err := json.NewDecoder(res.Body).Decode(&l); err != nil || res.StatusCode != 200 {
				t.Errorf("status %d, %v", res.StatusCode, err)
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
	close(start)
	wg.Wait()
	if len(seen) != requests*count {
		t.Fatalf("%d distinct ports, want %d", len(seen), requests*count)
	}
}

// readEvent reads one SSE frame: the optional `event:` name and the data line.
func readEvent(t *testing.T, r *bufio.Reader) (name, data string) {
	t.Helper()
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("reading event: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "":
			if data != "" {
				return name, data
			}
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
}

// nextLeaseEvent skips the unnamed ps frames until a lease.* frame arrives.
func nextLeaseEvent(t *testing.T, r *bufio.Reader) (string, registry.Lease) {
	t.Helper()
	for {
		name, data := readEvent(t, r)
		if strings.HasPrefix(name, "lease.") {
			return name, decodeLease(t, data)
		}
	}
}

func TestEventsReportLeases(t *testing.T) {
	d, srv := newTestDaemon(t)
	d.svc.PortFree = func(int) bool { return true }

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/events", nil)
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	r := bufio.NewReader(res.Body)

	// the first frame is the unnamed ps table; the client is now connected
	if name, _ := readEvent(t, r); name != "" {
		t.Fatalf("first frame is %q, want the unnamed ps frame", name)
	}

	// made through the daemon: arrives at once, well before the 2s tick
	t0 := time.Now()
	_, _, raw := do(t, srv, "POST", "/api/v1/leases", `{"owner":"wt-qa","count":2,"prefer":4100}`)
	created := decodeLease(t, raw)
	name, got := nextLeaseEvent(t, r)
	if name != "lease.created" || got.ID != created.ID || !slices.Equal(got.Ports, created.Ports) {
		t.Fatalf("event = %s %+v", name, got)
	}
	if d := time.Since(t0); d > 1500*time.Millisecond {
		t.Errorf("lease.created took %v: it should not wait for the poll", d)
	}

	do(t, srv, "DELETE", "/api/v1/leases/"+created.ID, "")
	name, got = nextLeaseEvent(t, r)
	if name != "lease.released" || got.ID != created.ID {
		t.Fatalf("event = %s %+v", name, got)
	}

	// made by another process (the CLI): noticed by the poll
	other, err := registry.Open(d.svc.Paths)
	if err != nil {
		t.Fatal(err)
	}
	cliLease, err := other.AddLease("cli", func(map[int]bool) ([]int, error) { return []int{4500}, nil })
	if err != nil {
		t.Fatal(err)
	}
	name, got = nextLeaseEvent(t, r)
	if name != "lease.created" || got.ID != cliLease.ID {
		t.Fatalf("event for a CLI lease = %s %+v", name, got)
	}
	if _, err := other.ReleaseLease(cliLease.ID); err != nil {
		t.Fatal(err)
	}
	name, got = nextLeaseEvent(t, r)
	if name != "lease.released" || got.ID != cliLease.ID {
		t.Fatalf("event for a CLI release = %s %+v", name, got)
	}
}

// A lease that already existed when the client connected is not replayed.
func TestEventsDoNotReplayExistingLeases(t *testing.T) {
	d, srv := newTestDaemon(t)
	d.svc.PortFree = func(int) bool { return true }
	if _, err := d.svc.CreateLease(api.LeaseRequest{Owner: "old"}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/events", nil)
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	r := bufio.NewReader(res.Body)
	readEvent(t, r) // ps

	fresh, err := d.svc.CreateLease(api.LeaseRequest{Owner: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if name, got := nextLeaseEvent(t, r); name != "lease.created" || got.ID != fresh.ID {
		t.Fatalf("first lease event = %s %+v, want only the new lease", name, got)
	}
}

func TestDiffLeases(t *testing.T) {
	a := registry.Lease{ID: "l-a", Owner: "x", Ports: []int{1}}
	b := registry.Lease{ID: "l-b", Owner: "x", Ports: []int{2}}
	c := registry.Lease{ID: "l-c", Owner: "y", Ports: []int{3}}

	evs := diffLeases(leasesByID([]registry.Lease{a, b}), leasesByID([]registry.Lease{b, c}))
	if len(evs) != 2 || evs[0].Name != "lease.created" || evs[0].Lease.ID != "l-c" ||
		evs[1].Name != "lease.released" || evs[1].Lease.ID != "l-a" {
		t.Fatalf("events = %+v", evs)
	}
	if evs := diffLeases(leasesByID([]registry.Lease{a}), leasesByID([]registry.Lease{a})); len(evs) != 0 {
		t.Fatalf("no change produced %+v", evs)
	}
}
