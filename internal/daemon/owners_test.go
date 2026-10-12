package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/pm/internal/api"
	"github.com/antoniojosev/pm/internal/testfake"
)

// startOwnerRuns starts two runs of owner over HTTP and leases two ports for it.
func startOwnerRuns(t *testing.T, srvDo func(method, path, body string) (int, map[string]any, string), owner string) {
	t.Helper()
	for _, name := range []string{"api", "web"} {
		b, _ := json.Marshal(t.TempDir())
		body := `{"name":"` + name + `-` + owner + `","dir":` + string(b) + `,"cmd":["sleep","100"],"owner":"` + owner + `"}`
		if code, _, raw := srvDo("POST", "/api/v1/run", body); code != 200 {
			t.Fatalf("run %s: %d %s", name, code, raw)
		}
	}
	if code, _, raw := srvDo("POST", "/api/v1/leases", `{"owner":"`+owner+`","count":2,"prefer":4100}`); code != 200 {
		t.Fatalf("lease: %d %s", code, raw)
	}
}

func TestOwnerStopAndDeleteOverHTTP(t *testing.T) {
	d, srv := newTestDaemon(t)
	d.svc.PortFree = func(int) bool { return true }
	sd := testfake.Install(t)
	call := func(method, path, body string) (int, map[string]any, string) {
		return do(t, srv, method, path, body)
	}
	startOwnerRuns(t, call, "wt-qa")
	startOwnerRuns(t, call, "wt-dev")

	// stop: processes go, leases stay
	code, obj, raw := call("POST", "/api/v1/owners/wt-qa/stop", "")
	if code != 200 || obj["owner"] != "wt-qa" {
		t.Fatalf("stop = %d %s", code, raw)
	}
	assertConforms(t, "POST", "/owners/{owner}/stop", raw)
	if st, _ := obj["stopped"].([]any); len(st) != 2 {
		t.Fatalf("stopped = %s", raw)
	}
	if rel, ok := obj["leases_released"].([]any); !ok || len(rel) != 0 {
		t.Fatalf("a stop must not release leases, and must say [] : %s", raw)
	}
	if sd.Active("pm-api-wt-qa.service") || !sd.Active("pm-api-wt-dev.service") {
		t.Fatal("wrong units stopped")
	}
	_, _, raw = call("GET", "/api/v1/leases?owner=wt-qa", "")
	if !strings.Contains(raw, "wt-qa") {
		t.Fatalf("the lease must survive a stop: %s", raw)
	}

	// delete: the lease goes too, and wt-dev is untouched
	code, obj, raw = call("DELETE", "/api/v1/owners/wt-qa", "")
	if code != 200 {
		t.Fatalf("delete = %d %s", code, raw)
	}
	assertConforms(t, "DELETE", "/owners/{owner}", raw)
	if rel, _ := obj["leases_released"].([]any); len(rel) != 1 {
		t.Fatalf("delete = %s", raw)
	}
	_, _, raw = call("GET", "/api/v1/leases?owner=wt-qa", "")
	if strings.TrimSpace(raw) != "[]" {
		t.Fatalf("leases of wt-qa after delete = %s", raw)
	}
	_, _, raw = call("GET", "/api/v1/ps", "")
	if strings.Contains(raw, "wt-qa") || !strings.Contains(raw, "wt-dev") {
		t.Fatalf("ps after delete = %s", raw)
	}

	// idempotent, and an owner with nothing is not an error
	code, obj, raw = call("DELETE", "/api/v1/owners/wt-qa", "")
	if code != 200 || len(obj["stopped"].([]any)) != 0 {
		t.Fatalf("second delete = %d %s", code, raw)
	}

	// the deprecated alias serves the same routes
	code, _, _ = call("POST", "/api/owners/wt-dev/stop", "")
	if code != 200 {
		t.Fatalf("POST /api/owners/{owner}/stop = %d", code)
	}
}

func TestOwnerRoutesReportErrors(t *testing.T) {
	d, srv := newTestDaemon(t)
	d.svc.PortFree = func(int) bool { return true }
	sd := testfake.Install(t)
	call := func(method, path, body string) (int, map[string]any, string) {
		return do(t, srv, method, path, body)
	}

	for _, path := range []string{"/api/v1/owners/a%20b", "/api/v1/owners/-x"} {
		code, obj, raw := call("DELETE", path, "")
		if code != 400 || !strings.Contains(obj["error"].(string), "invalid owner") {
			t.Fatalf("DELETE %s = %d %s", path, code, raw)
		}
	}
	if code, _, raw := call("POST", "/api/v1/owners/a%20b/stop", ""); code != 400 {
		t.Fatalf("stop with a bad owner = %d %s", code, raw)
	}

	// a stop that fails reports what it could not do, keeps the leases, and
	// can be retried
	startOwnerRuns(t, call, "wt-qa")
	sd.FailStop(t)
	code, obj, raw := call("DELETE", "/api/v1/owners/wt-qa", "")
	if code != 400 || !strings.Contains(obj["error"].(string), "leases were kept") {
		t.Fatalf("delete with a failing stop = %d %s", code, raw)
	}
	if errs, _ := obj["errors"].([]any); len(errs) != 2 || obj["owner"] != "wt-qa" {
		t.Fatalf("body = %s", raw)
	}
	if _, _, raw = call("GET", "/api/v1/leases?owner=wt-qa", ""); !strings.Contains(raw, "wt-qa") {
		t.Fatalf("leases must be kept: %s", raw)
	}
}

// nextOwnerEvent skips ps and lease frames until an owner.* frame arrives.
func nextOwnerEvent(t *testing.T, r *bufio.Reader) (string, api.OwnerResult) {
	t.Helper()
	for {
		name, data := readEvent(t, r)
		if strings.HasPrefix(name, "owner.") {
			var res api.OwnerResult
			if err := json.Unmarshal([]byte(data), &res); err != nil {
				t.Fatalf("not an owner result: %s (%v)", data, err)
			}
			return name, res
		}
	}
}

func TestEventsReportOwners(t *testing.T) {
	d, srv := newTestDaemon(t)
	d.svc.PortFree = func(int) bool { return true }
	testfake.Install(t)
	call := func(method, path, body string) (int, map[string]any, string) {
		return do(t, srv, method, path, body)
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
	readEvent(t, r) // the ps frame: the client is subscribed

	startOwnerRuns(t, call, "wt-qa")
	t0 := time.Now()
	call("POST", "/api/v1/owners/wt-qa/stop", "")
	name, got := nextOwnerEvent(t, r)
	if name != "owner.stopped" || got.Owner != "wt-qa" || len(got.Stopped) != 2 || len(got.LeasesReleased) != 0 {
		t.Fatalf("event = %s %+v", name, got)
	}
	if d := time.Since(t0); d > 1500*time.Millisecond {
		t.Errorf("owner.stopped took %v: it should not wait for the poll", d)
	}

	call("DELETE", "/api/v1/owners/wt-qa", "")
	name, got = nextOwnerEvent(t, r)
	if name != "owner.deleted" || got.Owner != "wt-qa" || len(got.LeasesReleased) != 1 {
		t.Fatalf("event = %s %+v", name, got)
	}
}
