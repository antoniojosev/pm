// Package detector reads the real runtime state of the machine: which ports
// are in LISTEN, by which process, and attributes each to a project using the
// systemd cgroup, the PM_PROJECT env marker, cwd heuristics, and docker labels.
package detector

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Source describes how an instance was identified, ranked by confidence.
type Source string

const (
	SourceMarker  Source = "marker"  // launched by us: cgroup/env — certain
	SourceCgroup  Source = "cgroup"  // pm-<name>.scope/service present
	SourceDocker  Source = "docker"  // docker ps labels — certain
	SourceCwd     Source = "cwd"     // external native, cwd matches a project dir
	SourceUnknown Source = "unknown" // listening but unattributed
)

// Instance is one live listener attributed (or not) to a project.
type Instance struct {
	Project   string `json:"project"` // "" when unclaimed
	Port      int    `json:"port"`
	PID       int    `json:"pid"`
	Cmd       string `json:"cmd"`
	Cwd       string `json:"cwd"`
	Unit      string `json:"unit"` // systemd unit (pm-<name>.scope) if any
	Source    Source `json:"source"`
	Docker    bool   `json:"docker"`
	Container string `json:"container,omitempty"` // docker container id/name, if Docker
}

var (
	// captures the local address column and the users:(("name",pid=N,fd=M)) tail.
	ssPidRe = regexp.MustCompile(`pid=(\d+)`)
	// pm-<name>.scope or pm-<name>.service inside a cgroup path
	unitRe = regexp.MustCompile(`pm-([A-Za-z0-9._-]+)\.(scope|service)`)
)

// Detector snapshots live state. Runner is an interface so the detector stays
// decoupled from systemd; the daemon injects the real one.
type Detector struct {
	// Roots to match external native processes by cwd -> project.
	// Populated by the caller from the registry (dir -> name).
	DirToName func(cwd string) (name, unit string, ok bool)
}

// Snapshot returns every LISTEN socket enriched with attribution.
func (d Detector) Snapshot() ([]Instance, error) {
	native, err := d.nativeListeners()
	if err != nil {
		return nil, err
	}
	docker := d.dockerListeners()
	return append(native, docker...), nil
}

func (d Detector) nativeListeners() ([]Instance, error) {
	out, err := run("ss", "-H", "-t", "-l", "-n", "-p")
	if err != nil {
		// ss missing or failed; return nothing rather than erroring hard.
		return nil, nil
	}
	seen := map[string]bool{} // dedupe by port+pid
	var res []Instance
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		port := portFromAddr(fields[3])
		if port == 0 {
			continue
		}
		pids := parsePIDs(line)
		if len(pids) == 0 {
			// listening but we can't see the pid (not ours / needs root)
			key := strconv.Itoa(port) + ":0"
			if !seen[key] {
				seen[key] = true
				res = append(res, Instance{Port: port, Source: SourceUnknown})
			}
			continue
		}
		for _, pid := range pids {
			key := strconv.Itoa(port) + ":" + strconv.Itoa(pid)
			if seen[key] {
				continue
			}
			seen[key] = true
			res = append(res, d.attribute(port, pid))
		}
	}
	return res, nil
}

// attribute resolves a (port, pid) into a fully described Instance.
func (d Detector) attribute(port, pid int) Instance {
	inst := Instance{Port: port, PID: pid, Source: SourceUnknown}
	inst.Cmd = procCmdline(pid)
	inst.Cwd = procCwd(pid)

	// 1. systemd cgroup: strongest signal for anything we launched.
	if unit, name := procUnit(pid); name != "" {
		inst.Unit = unit
		inst.Project = name
		inst.Source = SourceCgroup
		if procHasMarker(pid, name) {
			inst.Source = SourceMarker
		}
		return inst
	}
	// 2. env marker without a pm cgroup (rare, e.g. re-exec).
	if name := procMarker(pid); name != "" {
		inst.Project = name
		inst.Source = SourceMarker
		return inst
	}
	// 3. external native: match cwd against known project dirs.
	if d.DirToName != nil && inst.Cwd != "" {
		if name, unit, ok := d.DirToName(inst.Cwd); ok {
			inst.Project = name
			inst.Unit = unit
			inst.Source = SourceCwd
		}
	}
	return inst
}

// --- /proc helpers -------------------------------------------------------

func procCmdline(pid int) string {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
	return strings.Join(parts, " ")
}

func procCwd(pid int) string {
	p, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
	if err != nil {
		return ""
	}
	return p
}

// procUnit reads /proc/<pid>/cgroup and extracts a pm-<name> unit if present.
func procUnit(pid int) (unit, name string) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return "", ""
	}
	if m := unitRe.FindStringSubmatch(string(b)); m != nil {
		return "pm-" + m[1] + "." + m[2], m[1]
	}
	return "", ""
}

// procMarker reads PM_PROJECT from /proc/<pid>/environ.
func procMarker(pid int) string {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil {
		return ""
	}
	for _, kv := range strings.Split(string(b), "\x00") {
		if v, ok := strings.CutPrefix(kv, "PM_PROJECT="); ok {
			return v
		}
	}
	return ""
}

func procHasMarker(pid int, name string) bool {
	return strings.EqualFold(procMarker(pid), name)
}

// portFromAddr parses "127.0.0.1:18000", "*:5173", "[::]:3000" -> port.
func portFromAddr(addr string) int {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return 0
	}
	n, err := strconv.Atoi(addr[i+1:])
	if err != nil {
		return 0
	}
	return n
}

func parsePIDs(line string) []int {
	var pids []int
	for _, m := range ssPidRe.FindAllStringSubmatch(line, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil {
			pids = append(pids, n)
		}
	}
	return pids
}
