package runner

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// unitExists reports whether a systemd --user unit is currently loaded
// (running or otherwise). A not-found unit returns false, so Stop can fall
// back to killing an externally-launched process.
func unitExists(unit string) bool {
	out, _ := execCommand("systemctl", "--user", "show", unit, "-p", "LoadState", "--value").Output()
	return strings.TrimSpace(string(out)) == "loaded"
}

// killTree sends SIGTERM to a pid and all its descendants (one process at a
// time, walking /proc), avoiding process-group kills that could hit the shell.
func killTree(pid int) {
	targets := append([]int{pid}, descendants(pid)...)
	for _, p := range targets {
		_ = syscall.Kill(p, syscall.SIGTERM)
	}
}

// descendants returns every pid whose ancestry leads back to root.
func descendants(root int) []int {
	children := map[int][]int{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if ppid := parentPID(pid); ppid > 0 {
			children[ppid] = append(children[ppid], pid)
		}
	}
	var out []int
	var walk func(int)
	walk = func(p int) {
		for _, c := range children[p] {
			out = append(out, c)
			walk(c)
		}
	}
	walk(root)
	return out
}

// procArgv reads /proc/<pid>/cmdline as a real argv slice (NUL-separated).
func procArgv(pid int) []string {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range strings.Split(strings.TrimRight(string(b), "\x00"), "\x00") {
		if a != "" {
			out = append(out, a)
		}
	}
	return out
}

// parentPID reads PPid from /proc/<pid>/status.
func parentPID(pid int) int {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "PPid:"); ok {
			n, _ := strconv.Atoi(strings.TrimSpace(v))
			return n
		}
	}
	return 0
}
