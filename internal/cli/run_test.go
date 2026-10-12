package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/antoniojosev/pm/internal/runner"
	"github.com/antoniojosev/pm/internal/testfake"
)

// isolateWithSystemd is isolate plus the testfake systemd, in a fresh cwd.
func isolateWithSystemd(t *testing.T) (dir string, sd *testfake.Systemd) {
	t.Helper()
	isolate(t)
	sd = testfake.Install(t)
	dir = t.TempDir()
	t.Chdir(dir)
	return dir, sd
}

// The criterion of the step, through the real CLI: pm run --name x -- sleep 100
// shows up in pm ls with an owner, and pm stop x stops it.
func TestRunByNameAppearsInLsWithOwnerAndStops(t *testing.T) {
	dir, sd := isolateWithSystemd(t)

	out, _, err := run(t, "run", "--name", "x", "--", "sleep", "100")
	if err != nil {
		t.Fatalf("pm run: %v", err)
	}
	if !strings.Contains(out, "x") || !strings.Contains(out, "pid 4242") || !strings.Contains(out, "owner local") || !strings.Contains(out, "pm-x.service") {
		t.Fatalf("output = %q", out)
	}
	if !sd.Active("pm-x.service") {
		t.Fatal("unit not started")
	}
	args := sd.Calls("systemd-run")[0]
	if !contains(args, "--working-directory="+dir) || !contains(args, "--setenv=PM_PROJECT=x") || args[len(args)-3] != "--" {
		t.Fatalf("systemd-run args = %v", args)
	}

	out, _, err = run(t, "ls")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1 running") || !strings.Contains(out, "OWNER") || !strings.Contains(out, "local") || !strings.Contains(out, "ephemeral") {
		t.Fatalf("pm ls = %q", out)
	}
	line := lineWith(out, "x.localhost")
	if line == "" || !strings.Contains(line, "ephemeral") || !strings.Contains(line, "local") {
		t.Fatalf("pm ls row = %q in %q", line, out)
	}

	out, _, err = run(t, "stop", "x")
	if err != nil || !strings.Contains(out, "stopped") {
		t.Fatalf("pm stop = %q, %v", out, err)
	}
	if sd.Active("pm-x.service") {
		t.Fatal("unit still running")
	}
	out, _, _ = run(t, "ls", "-a")
	if strings.Contains(out, "x.localhost") || !strings.Contains(out, "0 running · 0 stopped") {
		t.Fatalf("pm ls -a after stop = %q", out)
	}
}

func lineWith(out, needle string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, needle) {
			return l
		}
	}
	return ""
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestRunByNameWithAllTheOptions(t *testing.T) {
	_, sd := isolateWithSystemd(t)
	logFile := filepath.Join(t.TempDir(), "x.log")

	out, _, err := run(t, "run", "--name", "api-qa", "--owner", "wt-qa", "--mem", "512M", "--cpu", "150%",
		"-e", "PORT=4100", "--env", "A=b=c", "--log", logFile, "--ports", "4100,4101", "--json", "--", "./serve", "--flag")
	if err != nil {
		t.Fatalf("pm run: %v", err)
	}
	var res runner.RunResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("--json output %q: %v", out, err)
	}
	if res.Unit != "pm-api-qa.service" || res.PID != 4242 || len(res.Ports) != 2 || res.Ports[0] != 4100 {
		t.Fatalf("result = %+v", res)
	}
	args := sd.Calls("systemd-run")[0]
	for _, want := range []string{"--setenv=PORT=4100", "--setenv=A=b=c", "MemoryMax=512M", "CPUQuota=150%", "StandardOutput=append:" + logFile} {
		if !contains(args, want) {
			t.Errorf("systemd-run lacks %q: %v", want, args)
		}
	}
	if got := strings.Join(args[len(args)-3:], " "); got != "-- ./serve --flag" {
		t.Errorf("command = %v", args)
	}

	out, _, _ = run(t, "ls")
	if !strings.Contains(out, "wt-qa") || !strings.Contains(out, ":4100") {
		t.Fatalf("pm ls = %q", out)
	}
	// the env went to systemd and nowhere else
	reg, _ := os.ReadFile(filepath.Join(os.Getenv("PM_HOME"), "registry.json"))
	if strings.Contains(string(reg), "A=b=c") || strings.Contains(string(reg), "PORT") {
		t.Fatalf("env stored in the registry: %s", reg)
	}
}

func TestRunByNameWithComposeNamesTheStack(t *testing.T) {
	_, sd := isolateWithSystemd(t)
	for _, name := range []string{"shop-a", "shop-b"} {
		if _, _, err := run(t, "run", "--name", name, "--compose-project", name+"-stack", "--compose-file", "compose.yml",
			"--", "docker", "compose", "up"); err != nil {
			t.Fatalf("pm run %s: %v", name, err)
		}
	}
	calls := sd.Calls("systemd-run")
	if len(calls) != 2 {
		t.Fatalf("systemd-run calls = %v", calls)
	}
	for i, name := range []string{"shop-a", "shop-b"} {
		args := calls[i]
		want := "docker compose -f compose.yml -p " + name + "-stack up"
		if got := strings.Join(args[len(args)-7:], " "); got != want {
			t.Errorf("%s command = %q, want %q", name, got, want)
		}
		if !contains(args, "--setenv=COMPOSE_PROJECT_NAME="+name+"-stack") {
			t.Errorf("%s lacks the project variable: %v", name, args)
		}
	}
	if !sd.Active("pm-shop-a.service") || !sd.Active("pm-shop-b.service") {
		t.Fatal("both runs must be alive")
	}
}

func TestComposeFlagsNeedNameOrAreValidated(t *testing.T) {
	_, sd := isolateWithSystemd(t)
	if _, _, err := run(t, "run", "--compose-project", "p", "--", "true"); err == nil || !strings.Contains(err.Error(), "--compose-project needs --name") {
		t.Fatalf("without --name: %v", err)
	}
	if _, _, err := run(t, "run", "--name", "x", "--compose-file", "c.yml", "--", "true"); err == nil || !strings.Contains(err.Error(), "compose.project is required") {
		t.Fatalf("file without project: %v", err)
	}
	if _, _, err := run(t, "run", "--name", "x", "--compose-project", "Bad Name", "--", "true"); err == nil || !strings.Contains(err.Error(), "invalid compose project") {
		t.Fatalf("bad project: %v", err)
	}
	if len(sd.Calls("systemd-run")) != 0 {
		t.Fatal("systemd-run ran for a refused call")
	}
}

func TestAddAndEditKeepTheComposeStack(t *testing.T) {
	isolateWithSystemd(t)
	if _, _, err := run(t, "add", "--name", "shop", "--cmd", "docker compose up", "--compose-project", "shop-wt2", "--compose-file", "c.yml"); err != nil {
		t.Fatalf("pm add: %v", err)
	}
	reg, _ := os.ReadFile(filepath.Join(os.Getenv("PM_HOME"), "registry.json"))
	for _, want := range []string{`"kind": "docker"`, `"project": "shop-wt2"`, `"file": "c.yml"`} {
		if !strings.Contains(string(reg), want) {
			t.Errorf("registry lacks %s:\n%s", want, reg)
		}
	}
	if _, _, err := run(t, "edit", "shop", "--compose-project", "shop-wt3"); err != nil {
		t.Fatalf("pm edit: %v", err)
	}
	reg, _ = os.ReadFile(filepath.Join(os.Getenv("PM_HOME"), "registry.json"))
	if !strings.Contains(string(reg), `"project": "shop-wt3"`) || !strings.Contains(string(reg), `"file": "c.yml"`) {
		t.Errorf("edit did not change the project and keep the file:\n%s", reg)
	}
	if _, _, err := run(t, "edit", "shop", "--compose-project", "", "--compose-file", ""); err != nil {
		t.Fatalf("clearing the stack: %v", err)
	}
	reg, _ = os.ReadFile(filepath.Join(os.Getenv("PM_HOME"), "registry.json"))
	if strings.Contains(string(reg), `"compose": {`) {
		t.Errorf("the stack was not cleared:\n%s", reg)
	}
}

func TestRunByNameErrors(t *testing.T) {
	_, sd := isolateWithSystemd(t)
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no command", []string{"run", "--name", "x"}, "cmd is required"},
		{"bad name", []string{"run", "--name", "a b", "--", "true"}, "invalid name"},
		{"bad env", []string{"run", "--name", "x", "-e", "NOEQUALS", "--", "true"}, "use KEY=VALUE"},
		{"bad limit", []string{"run", "--name", "x", "--mem", "lots", "--", "true"}, "invalid memory limit"},
		{"bad owner", []string{"run", "--name", "x", "--owner", "a b", "--", "true"}, "invalid owner"},
		{"relative log", []string{"run", "--name", "x", "--log", "x.log", "--", "true"}, "invalid log"},
		{"port flag with --name", []string{"run", "--name", "x", "--port", "3000", "--", "true"}, "--port applies to a registered project"},
		{"always with --name", []string{"run", "--name", "x", "--always", "--", "true"}, "--always applies"},
		{"owner without --name", []string{"run", "--owner", "o", "--", "true"}, "--owner needs --name"},
		{"ports without --name", []string{"run", "--ports", "4100", "--", "true"}, "--ports needs --name"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := run(t, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	if len(sd.Calls("systemd-run")) != 0 {
		t.Fatal("systemd-run ran for a refused command")
	}
}

func TestRunByNameRefusesALiveNameAndPorts(t *testing.T) {
	isolateWithSystemd(t)
	if _, _, err := run(t, "run", "--name", "x", "--", "sleep", "100"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, "run", "--name", "x", "--", "sleep", "100"); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("same name twice: %v", err)
	}
	// run holding a port from a lease: another owner cannot use it
	out, _, err := run(t, "lease", "--owner", "wt-qa", "--prefer", "24500", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var lease struct {
		Ports []int `json:"ports"`
	}
	_ = json.Unmarshal([]byte(out), &lease)
	if len(lease.Ports) != 1 {
		t.Fatalf("lease = %q", out)
	}
	_, _, err = run(t, "run", "--name", "y", "--owner", "wt-dev", "--ports", strconv.Itoa(lease.Ports[0]), "--", "true")
	if err == nil || !strings.Contains(err.Error(), "leased to") {
		t.Fatalf("a port leased to someone else: %v", err)
	}
	if _, _, err := run(t, "run", "--name", "y", "--owner", "wt-qa", "--ports", strconv.Itoa(lease.Ports[0]), "--", "true"); err != nil {
		t.Fatalf("a port leased to the same owner: %v", err)
	}
}

// Without --name the command keeps doing what it always did: register the cwd
// as a project, with no owner.
func TestRunWithoutNameStillRegistersTheProject(t *testing.T) {
	dir, _ := isolateWithSystemd(t)
	if _, _, err := run(t, "run", "--", "sleep", "100"); err != nil {
		t.Fatalf("pm run: %v", err)
	}
	s, _ := svc()
	p, ok := s.Get(filepath.Base(dir))
	if !ok || p.IsRun() || p.Owner != "" || strings.Join(p.Cmd, " ") != "sleep 100" {
		t.Fatalf("project = %+v, %v", p, ok)
	}
}

func TestParseEnvFlags(t *testing.T) {
	got, err := parseEnvFlags([]string{"A=1", "B=", "A=2", "C=x=y"})
	if err != nil || got["A"] != "2" || got["B"] != "" || got["C"] != "x=y" || len(got) != 3 {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, err := parseEnvFlags(nil); got != nil || err != nil {
		t.Fatalf("nil in = %v, %v", got, err)
	}
	if _, err := parseEnvFlags([]string{"=x"}); err == nil {
		t.Fatal("an empty name must be refused")
	}
}
