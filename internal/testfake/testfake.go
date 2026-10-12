// Package testfake puts stand-ins for systemctl and systemd-run on PATH, for
// tests above the runner that cannot swap the runner's exec seam: the API
// service, the daemon, the MCP server and the CLI. Nothing real is ever
// started. The stand-ins keep a little state, so a unit started by systemd-run
// is loaded and active until systemctl stops it, which is enough to follow a
// process from `pm run` through `pm ps` to `pm stop`. Import it from tests only.
package testfake

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Systemd is the installed pair of fakes.
type Systemd struct {
	state string
}

// MainPID is the pid every fake unit reports.
const MainPID = 4242

const systemdRunScript = `#!/bin/sh
state="$PM_FAKE_STATE"
unit=""
for a in "$@"; do
  case "$a" in --unit=*) unit="${a#--unit=}" ;; esac
done
{ echo "systemd-run"; for a in "$@"; do echo "$a"; done; echo "--end--"; } >> "$state/calls"
if [ -e "$state/fail-systemd-run" ]; then echo "fake systemd-run: refused" >&2; exit 1; fi
echo __PID__ > "$state/$unit.pid"
echo "Running as unit: $unit."
`

const systemctlScript = `#!/bin/sh
state="$PM_FAKE_STATE"
[ "$1" = "--user" ] && shift
verb="$1"; shift
{ echo "systemctl"; echo "$verb"; for a in "$@"; do echo "$a"; done; echo "--end--"; } >> "$state/calls"
case "$verb" in
  show)
    unit="$1"
    case "$unit" in
      pm-\*.service)
        for f in "$state"/pm-*.pid; do
          [ -e "$f" ] || continue
          u="$(basename "$f" .pid)"
          echo "Id=$u"; echo "ActiveState=active"; echo "Result=success"; echo "MainPID=$(cat "$f")"; echo
        done ;;
      *)
        case " $* " in
          *" LoadState "*) if [ -e "$state/$unit.pid" ]; then echo loaded; else echo not-found; fi ;;
          *" MainPID "*) if [ -e "$state/$unit.pid" ]; then cat "$state/$unit.pid"; else echo 0; fi ;;
        esac ;;
    esac ;;
  is-active)
    if [ -e "$state/$1.pid" ]; then echo active; else echo inactive; exit 3; fi ;;
  stop)
    rm -f "$state/$1.pid" ;;
esac
exit 0
`

// Install makes the fake systemctl and systemd-run the only programs on PATH.
func Install(t *testing.T) *Systemd {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{"systemd-run": systemdRunScript, "systemctl": systemctlScript} {
		script = strings.ReplaceAll(script, "__PID__", strconv.Itoa(MainPID))
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The scripts use cat, basename and rm. They are linked in, so that PATH is
	// this directory alone: no real systemctl, ss or docker can answer.
	for _, tool := range []string{"cat", "basename", "rm"} {
		real, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("testfake needs %s: %v", tool, err)
		}
		if err := os.Symlink(real, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PM_FAKE_STATE", state)
	t.Setenv("PATH", bin)
	return &Systemd{state: state}
}

// Calls returns the argv of every recorded call to name ("systemd-run" or
// "systemctl"; for systemctl the verb is the first element).
func (s *Systemd) Calls(name string) [][]string {
	raw, err := os.ReadFile(filepath.Join(s.state, "calls"))
	if err != nil {
		return nil
	}
	var out [][]string
	var cur []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "--end--" {
			if len(cur) > 0 && cur[0] == name {
				out = append(out, cur[1:])
			}
			cur = nil
			continue
		}
		if line != "" || cur != nil {
			cur = append(cur, line)
		}
	}
	return out
}

// Active reports whether the fake unit is loaded and running.
func (s *Systemd) Active(unit string) bool {
	_, err := os.Stat(filepath.Join(s.state, unit+".pid"))
	return err == nil
}

// Kill makes the unit vanish as if its process had exited.
func (s *Systemd) Kill(unit string) { _ = os.Remove(filepath.Join(s.state, unit+".pid")) }

// FailSystemdRun makes every later systemd-run exit with an error.
func (s *Systemd) FailSystemdRun(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.state, "fail-systemd-run"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}
