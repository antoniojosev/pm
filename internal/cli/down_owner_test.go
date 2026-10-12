package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/antoniojosev/pm/internal/api"
)

// The criterion of the step, through the real CLI: pm down --owner leaves pm ls
// and pm lease ls with no trace of the owner.
func TestDownOwnerLeavesNoTrace(t *testing.T) {
	_, sd := isolateWithSystemd(t)

	for _, args := range [][]string{
		{"run", "--name", "api-qa", "--owner", "wt-qa", "--", "sleep", "100"},
		{"run", "--name", "web-qa", "--owner", "wt-qa", "--", "sleep", "100"},
		{"run", "--name", "api-dev", "--owner", "wt-dev", "--", "sleep", "100"},
		{"lease", "2", "--owner", "wt-qa", "--prefer", "24600"},
		{"lease", "1", "--owner", "wt-dev", "--prefer", "24610"},
	} {
		if _, _, err := run(t, args...); err != nil {
			t.Fatalf("pm %v: %v", args, err)
		}
	}
	if out, _, _ := run(t, "ls"); !strings.Contains(out, "3 running") {
		t.Fatalf("pm ls = %q", out)
	}
	if !contains(sd.Calls("systemd-run")[0], "--setenv=PM_OWNER=wt-qa") {
		t.Fatalf("pm run did not mark the process with its owner: %v", sd.Calls("systemd-run")[0])
	}

	out, _, err := run(t, "down", "--owner", "wt-qa")
	if err != nil {
		t.Fatalf("pm down --owner: %v", err)
	}
	for _, want := range []string{"api-qa", "web-qa", "run", "1 lease released", "owner wt-qa down"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if sd.Active("pm-api-qa.service") || sd.Active("pm-web-qa.service") || !sd.Active("pm-api-dev.service") {
		t.Fatal("wrong units stopped")
	}

	out, _, _ = run(t, "ls", "-a")
	if strings.Contains(out, "wt-qa") || strings.Contains(out, "api-qa") || strings.Contains(out, "web-qa") || !strings.Contains(out, "wt-dev") {
		t.Fatalf("pm ls -a after down:\n%s", out)
	}
	out, _, _ = run(t, "lease", "ls")
	if strings.Contains(out, "wt-qa") || !strings.Contains(out, "wt-dev") {
		t.Fatalf("pm lease ls after down:\n%s", out)
	}

	// again: nothing left, not an error
	out, _, err = run(t, "down", "--owner", "wt-qa")
	if err != nil || !strings.Contains(out, "nothing running or leased for owner wt-qa") {
		t.Fatalf("second down = %q, %v", out, err)
	}
}

func TestDownOwnerKeepLeasesAndJSON(t *testing.T) {
	isolateWithSystemd(t)
	if _, _, err := run(t, "run", "--name", "x", "--owner", "wt-qa", "--", "sleep", "100"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, "lease", "--owner", "wt-qa", "--prefer", "24700"); err != nil {
		t.Fatal(err)
	}
	out, _, err := run(t, "down", "--owner", "wt-qa", "--keep-leases", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var res api.OwnerResult
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Owner != "wt-qa" || len(res.Stopped) != 1 || res.Stopped[0].Name != "x" || len(res.LeasesReleased) != 0 {
		t.Fatalf("--json = %q (%v)", out, err)
	}
	if out, _, _ := run(t, "lease", "ls"); !strings.Contains(out, "wt-qa") || !strings.Contains(out, ":24700") {
		t.Fatalf("--keep-leases dropped the lease:\n%s", out)
	}
}

func TestDownOwnerReportsAFailureAndKeepsLeases(t *testing.T) {
	_, sd := isolateWithSystemd(t)
	if _, _, err := run(t, "run", "--name", "x", "--owner", "wt-qa", "--", "sleep", "100"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, "lease", "--owner", "wt-qa", "--prefer", "24800"); err != nil {
		t.Fatal(err)
	}
	sd.FailStop(t)
	_, _, err := run(t, "down", "--owner", "wt-qa")
	if err == nil || !strings.Contains(err.Error(), "leases were kept") {
		t.Fatalf("err = %v", err)
	}
	if out, _, _ := run(t, "lease", "ls"); !strings.Contains(out, "wt-qa") {
		t.Fatalf("the lease must survive a failed down:\n%s", out)
	}
}

func TestDownArguments(t *testing.T) {
	isolate(t)
	for name, args := range map[string][]string{
		"nothing":            {"down"},
		"group and owner":    {"down", "web", "--owner", "x"},
		"keep-leases/group":  {"down", "web", "--keep-leases"},
		"bad owner":          {"down", "--owner", "a b"},
		"json on a group":    {"down", "web", "--json"},
		"unknown group left": {"down", "ghost"},
	} {
		if _, _, err := run(t, args...); err == nil {
			t.Errorf("%s: pm %v should fail", name, args)
		}
	}
}

func TestDownGroupStillWorks(t *testing.T) {
	isolate(t)
	if _, _, err := run(t, "add", "web", "--dir", t.TempDir(), "--cmd", "true"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, "group", "set", "g", "web"); err != nil {
		t.Fatal(err)
	}
	out, _, err := run(t, "down", "g")
	if err != nil || !strings.Contains(out, "group g down") {
		t.Fatalf("pm down g = %q, %v", out, err)
	}
}
