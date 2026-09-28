package runner

import (
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// fakeExec replaces execCommand so systemctl / systemd-run / docker are never
// really invoked. Every call is recorded; stdout and failures are scripted
// per exact command line (or per binary name for failures).
type fakeExec struct {
	mu     sync.Mutex
	calls  [][]string
	stdout map[string]string // "systemctl --user is-active pm-web.service" → output
	fail   map[string]bool   // exact command line, or bare binary name
}

func installFakeExec(t *testing.T) *fakeExec {
	t.Helper()
	f := &fakeExec{stdout: map[string]string{}, fail: map[string]bool{}}
	prev := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		full := append([]string{name}, args...)
		key := strings.Join(full, " ")
		f.mu.Lock()
		f.calls = append(f.calls, full)
		out := f.stdout[key]
		failed := f.fail[key] || f.fail[name]
		f.mu.Unlock()

		// A tiny shell stands in for the real binary: it echoes the scripted
		// stdout and exits with the scripted status.
		cmd := exec.Command("sh", "-c", `printf '%s' "$PM_TEST_STDOUT"; exit "$PM_TEST_EXIT"`)
		exit := "0"
		if failed {
			exit = "1"
		}
		cmd.Env = append(os.Environ(), "PM_TEST_STDOUT="+out, "PM_TEST_EXIT="+exit)
		return cmd
	}
	t.Cleanup(func() { execCommand = prev })
	return f
}

// called reports whether some recorded invocation starts with name and
// contains every fragment as a whole argument.
func (f *fakeExec) called(name string, fragments ...string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c[0] != name {
			continue
		}
		ok := true
		for _, frag := range fragments {
			if !contains(c[1:], frag) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// argsOf returns the arguments of the first invocation of name.
func (f *fakeExec) argsOf(name string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c[0] == name {
			return c[1:]
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
