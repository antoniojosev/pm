package cli

import (
	"encoding/json"
	"net"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/pm/internal/registry"
)

var leaseIDRe = regexp.MustCompile(`l-[0-9a-f]{12}`)

func TestLeaseCommandReservesAndListsAndReleases(t *testing.T) {
	isolate(t)

	out, _, err := run(t, "lease", "2", "--owner", "wt-qa", "--prefer", "24100")
	if err != nil {
		t.Fatalf("pm lease: %v", err)
	}
	id := leaseIDRe.FindString(out)
	if id == "" || !strings.Contains(out, "wt-qa") || !strings.Contains(out, ":24100") || !strings.Contains(out, ":24101") {
		t.Fatalf("output = %q", out)
	}

	// the lease is in the registry file, so a second invocation sees it
	out, _, err = run(t, "lease", "ls")
	if err != nil || !strings.Contains(out, id) || !strings.Contains(out, "wt-qa") || !strings.Contains(out, ":24100 :24101") {
		t.Fatalf("ls = %q, %v", out, err)
	}
	out, _, _ = run(t, "lease", "ls", "--owner", "someone-else")
	if !strings.Contains(out, "no leases") {
		t.Fatalf("ls by other owner = %q", out)
	}

	// a second lease never gets the first one's ports
	out, _, err = run(t, "lease", "2", "--owner", "wt-dev", "--prefer", "24100")
	if err != nil || strings.Contains(out, ":24100") || strings.Contains(out, ":24101") || !strings.Contains(out, ":24102") {
		t.Fatalf("second lease = %q, %v", out, err)
	}

	out, _, err = run(t, "lease", "release", id)
	if err != nil || !strings.Contains(out, id) || !strings.Contains(out, "released") {
		t.Fatalf("release = %q, %v", out, err)
	}
	if _, _, err := run(t, "lease", "release", id); err == nil {
		t.Fatal("releasing twice must fail")
	}
	// the freed ports can be leased again
	out, _, _ = run(t, "lease", "--owner", "wt-qa", "--prefer", "24100")
	if !strings.Contains(out, ":24100") {
		t.Fatalf("freed port not reused: %q", out)
	}
}

func TestLeaseCommandDefaultsToOnePort(t *testing.T) {
	isolate(t)
	out, _, err := run(t, "lease", "--owner", "x", "--prefer", "24300")
	if err != nil || !strings.Contains(out, ":24300") || strings.Contains(out, ":24301") {
		t.Fatalf("output = %q, %v", out, err)
	}
}

func TestLeaseCommandJSON(t *testing.T) {
	isolate(t)
	out, _, err := run(t, "lease", "1", "--owner", "x", "--prefer", "24400", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var l registry.Lease
	if err := json.Unmarshal([]byte(out), &l); err != nil || l.Owner != "x" || len(l.Ports) != 1 || l.Ports[0] != 24400 {
		t.Fatalf("json = %q (%v)", out, err)
	}
	out, _, err = run(t, "lease", "ls", "--json")
	var all []registry.Lease
	if err != nil || json.Unmarshal([]byte(out), &all) != nil || len(all) != 1 || all[0].ID != l.ID {
		t.Fatalf("ls --json = %q, %v", out, err)
	}
	out, _, _ = run(t, "lease", "ls", "--owner", "nobody", "--json")
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty ls --json = %q, want []", out)
	}
}

func TestLeaseCommandErrors(t *testing.T) {
	isolate(t)
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"owner is required", []string{"lease", "2"}, "owner"},
		{"count must be a number", []string{"lease", "many", "--owner", "x"}, "invalid count"},
		{"count is capped", []string{"lease", "101", "--owner", "x"}, "invalid count"},
		{"owner grammar", []string{"lease", "--owner", "a b"}, "invalid owner"},
		{"unknown id", []string{"lease", "release", "l-000000000000"}, "does not exist"},
		{"release needs an id", []string{"lease", "release"}, "argument"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, err := run(t, tc.args...)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error()+stderr, tc.want) {
				t.Fatalf("err = %v, stderr = %q, want %q", err, stderr, tc.want)
			}
		})
	}
}

// The bind test is real here: a port some process listens on is never leased,
// even though `ss` is not even installed in the sandbox.
func TestLeaseCommandSkipsABoundPort(t *testing.T) {
	isolate(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	taken := l.Addr().(*net.TCPAddr).Port

	out, _, err := run(t, "lease", "--owner", "x", "--prefer", strconv.Itoa(taken))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, ":"+strconv.Itoa(taken)+"\n") || strings.Contains(out, ":"+strconv.Itoa(taken)+" ") {
		t.Fatalf("leased a port that is in use: %q", out)
	}
}

func TestAgeToken(t *testing.T) {
	if age(timeAgo(5)) != "5s" || age(timeAgo(120)) != "2m" || age(timeAgo(3*3600)) != "3h" || age(timeAgo(72*3600)) != "3d" {
		t.Errorf("age: %s %s %s %s", age(timeAgo(5)), age(timeAgo(120)), age(timeAgo(3*3600)), age(timeAgo(72*3600)))
	}
}

func timeAgo(seconds int) time.Time { return time.Now().Add(-time.Duration(seconds) * time.Second) }
