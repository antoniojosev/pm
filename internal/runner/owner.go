package runner

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/antoniojosev/pm/internal/detector"
	"github.com/antoniojosev/pm/internal/registry"
)

// Kinds of what StopOwner stopped.
const (
	StoppedProject   = "project"   // a registered project that was running
	StoppedRun       = "run"       // a process started with pm run; it is forgotten
	StoppedProcess   = "process"   // a process nobody registered that carries PM_OWNER
	StoppedContainer = "container" // a docker container that carries the owner marker
)

// Stopped is one thing StopOwner brought down.
type Stopped struct {
	Kind string `json:"kind"`
	// Name is the project or run name, or the container.
	Name string `json:"name,omitempty"`
	// PID is the process, for Kind "process".
	PID int `json:"pid,omitempty"`
}

// StopOwner stops everything that belongs to owner: the registered projects and
// the runs it owns (a run stopped is forgotten, so nothing of it is left in
// the listing), and every live process that carries PM_OWNER=<owner> without
// being one of those, say one a script left running. It does not stop at the
// first failure: it returns what it stopped and one error per thing it could
// not. Leases are not touched here (see api.Service.DeleteOwner).
//
// A registered project of the owner that is not running is skipped, not an
// error: it stays in the catalog like after `pm stop`.
func (r *Runner) StopOwner(owner string) (stopped []Stopped, errs []error) {
	_ = r.Store.Reload() // the owner's runs may have been started by another process
	insts, _ := r.Snapshot()

	handled := map[string]bool{} // lowercase names of the owner's projects
	for _, p := range r.Store.List() {
		if !strings.EqualFold(p.Owner, owner) {
			continue
		}
		handled[strings.ToLower(p.Name)] = true
		kind := StoppedProject
		if p.IsRun() {
			kind = StoppedRun
		} else if !r.runningProject(p, insts) {
			continue
		}
		if err := r.Stop(p.Name); err != nil {
			errs = append(errs, fmt.Errorf("stop %s: %w", p.Name, err))
			continue
		}
		stopped = append(stopped, Stopped{Kind: kind, Name: p.Name})
	}

	// Whatever else carries the marker. One process can hold several ports, so
	// each pid and container is stopped once.
	seenPID := map[int]bool{}
	seenContainer := map[string]bool{}
	sort.SliceStable(insts, func(a, b int) bool { return insts[a].PID < insts[b].PID })
	for _, i := range insts {
		if !strings.EqualFold(i.Owner, owner) || handled[strings.ToLower(i.Project)] {
			continue
		}
		switch {
		case i.Docker && i.Container != "":
			if seenContainer[i.Container] {
				continue
			}
			seenContainer[i.Container] = true
			if out, err := execCommand("docker", "stop", i.Container).CombinedOutput(); err != nil {
				errs = append(errs, fmt.Errorf("docker stop %s: %v: %s", i.Container, err, strings.TrimSpace(string(out))))
				continue
			}
			stopped = append(stopped, Stopped{Kind: StoppedContainer, Name: i.Container})
		case i.PID > 0 && i.PID != os.Getpid():
			if seenPID[i.PID] {
				continue
			}
			seenPID[i.PID] = true
			killTree(i.PID)
			stopped = append(stopped, Stopped{Kind: StoppedProcess, PID: i.PID})
		}
	}
	return stopped, errs
}

// runningProject reports whether a registered project has something to stop: an
// active unit, or a live process attributed to it.
func (r *Runner) runningProject(p registry.Project, insts []detector.Instance) bool {
	if r.IsActive(p.Name) {
		return true
	}
	for _, i := range insts {
		if strings.EqualFold(i.Project, p.Name) {
			return true
		}
	}
	return false
}
