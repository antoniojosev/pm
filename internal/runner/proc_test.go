package runner

import (
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

// spawnSleep starts a throwaway child process owned by the test so /proc
// helpers can be exercised against a real, isolated pid.
func spawnSleep(t *testing.T) *exec.Cmd {
	t.Helper()
	if _, err := os.Stat("/proc/self/status"); err != nil {
		t.Skip("procfs not available")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

func TestProcHelpersOnChild(t *testing.T) {
	child := spawnSleep(t)
	pid := child.Process.Pid

	if got := parentPID(pid); got != os.Getpid() {
		t.Fatalf("parentPID(%d) = %d, want %d", pid, got, os.Getpid())
	}
	// /proc/<pid>/cmdline can read empty for a moment right after exec.
	var argv []string
	for i := 0; i < 20 && len(argv) == 0; i++ {
		argv = procArgv(pid)
		time.Sleep(25 * time.Millisecond)
	}
	if !reflect.DeepEqual(argv, []string{"sleep", "30"}) {
		t.Fatalf("procArgv = %v", argv)
	}
	found := false
	for _, d := range descendants(os.Getpid()) {
		if d == pid {
			found = true
		}
	}
	if !found {
		t.Fatalf("descendants(%d) does not include child %d", os.Getpid(), pid)
	}
}

func TestProcHelpersOnMissingPid(t *testing.T) {
	const bogus = 1 << 30
	if parentPID(bogus) != 0 {
		t.Fatal("parentPID of a missing pid should be 0")
	}
	if procArgv(bogus) != nil {
		t.Fatal("procArgv of a missing pid should be nil")
	}
}

func TestKillTreeTerminatesChild(t *testing.T) {
	child := spawnSleep(t)
	killTree(child.Process.Pid)

	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("child still alive after killTree")
	}
}
