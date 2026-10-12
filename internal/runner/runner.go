// Package runner launches, stops and supervises projects through systemd
// --user units. Ephemeral projects run as transient services; service-tier
// projects get a persistent unit enabled at boot. Every unit is named
// pm-<name>.service and carries PM_PROJECT so the detector can attribute it.
package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/antoniojosev/pm/internal/config"
	"github.com/antoniojosev/pm/internal/detector"
	"github.com/antoniojosev/pm/internal/registry"
)

// Runner orchestrates process lifecycle.
type Runner struct {
	Paths config.Paths
	Store *registry.Store
	Det   detector.Detector
	// Snap overrides Det.Snapshot when set (tests inject fake live state).
	Snap func() ([]detector.Instance, error)
	// PortWait bounds how long a launch waits for the unit to bind a port.
	PortWait time.Duration
}

// execCommand is the seam through which every external binary (systemctl,
// systemd-run, docker) is invoked; tests swap it for a fake.
var execCommand = exec.Command

// New builds a Runner wired to the shared store, with a detector that
// resolves external processes against the registry by cwd.
func New(paths config.Paths, store *registry.Store) *Runner {
	det := detector.Detector{
		DirToName: func(cwd string) (string, string, bool) {
			if p, ok := store.FindByDir(cwd); ok {
				return p.Name, config.UnitPrefix + p.Name + ".service", true
			}
			return "", "", false
		},
		ComposeToName: func(project string) (string, bool) {
			if p, ok := store.FindByCompose(project); ok {
				return p.Name, true
			}
			return "", false
		},
	}
	return &Runner{Paths: paths, Store: store, Det: det, PortWait: 8 * time.Second}
}

// Snapshot returns the live listeners, via Snap when injected.
func (r *Runner) Snapshot() ([]detector.Instance, error) {
	if r.Snap != nil {
		return r.Snap()
	}
	return r.Det.Snapshot()
}

// LaunchOpts tweak a single start.
type LaunchOpts struct {
	Port  int  // 0 = use prefer/auto; >0 = fix this port
	Force bool // fail on collision instead of reassigning
	// Limits override the project's own limits for this launch, field by
	// field; an empty field keeps the project's value.
	Limits registry.Limits
}

// Result reports the outcome of a launch.
type Result struct {
	Project string
	Unit    string
	Port    int
	URL     string
	Note    string
}

// UnitName returns pm-<name>.service.
func UnitName(name string) string { return config.UnitPrefix + name + ".service" }

// Start launches a registered project. It resolves a free port (honoring
// collisions), builds the command with the port injected, and starts the
// unit — transient for ephemeral, persistent for service tier.
func (r *Runner) Start(name string, opts LaunchOpts) (Result, error) {
	p, ok := r.Store.Get(name)
	if !ok {
		return Result{}, fmt.Errorf("project %q is not registered", name)
	}
	return r.launch(p, opts)
}

func (r *Runner) launch(p registry.Project, opts LaunchOpts) (Result, error) {
	if err := refuseRun(p, "start"); err != nil {
		return Result{}, err
	}
	if p.Dir == "" {
		return Result{}, fmt.Errorf("cannot start %q: no local directory (registered by name only)", p.Name)
	}
	if err := r.Paths.EnsureDirs(); err != nil {
		return Result{}, err
	}
	if p.Tier == registry.TierService && !opts.Limits.IsZero() {
		// A service-tier unit is a file enabled at boot: an override written
		// into it would outlive this start and every restart after it.
		return Result{}, fmt.Errorf("cannot start %q with --mem/--cpu: it is a service-tier project whose unit persists across restarts; set the limits with `pm edit %s --mem ... --cpu ...`, then `pm restart %s` to rewrite the unit", p.Name, p.Name, p.Name)
	}
	p.Limits = p.Limits.Merge(opts.Limits)
	if err := p.Validate(); err != nil {
		return Result{}, fmt.Errorf("cannot start %q: %w", p.Name, err)
	}
	res := Result{Project: p.Name, Unit: UnitName(p.Name)}

	r.ReleaseDeadRunPorts() // reloads the store: leases may have been taken by another process
	port, note, err := resolvePort(p, opts, r.occupiedPorts(), r.Store.LeasedPorts())
	if err != nil {
		return Result{}, err
	}
	res.Port, res.Note = port, note

	cmd, env := applyPort(p, port)
	cmd, composeEnv := applyCompose(cmd, p.Compose)
	env = append(env, composeEnv...)

	if p.Tier == registry.TierService {
		if err := r.writeUnit(p, cmd, env); err != nil {
			return Result{}, err
		}
		if err := systemctl("daemon-reload"); err != nil {
			return Result{}, err
		}
		if err := systemctl("enable", "--now", UnitName(p.Name)); err != nil {
			return Result{}, err
		}
	} else {
		if err := r.runTransient(p, cmd, env); err != nil {
			return Result{}, err
		}
	}

	// Discover the actual bound port if we didn't fix one.
	if actual := r.waitForPort(p.Name, r.PortWait); actual != 0 {
		res.Port = actual
	}
	res.URL = "http://" + p.Hostname()
	if res.Port != 0 && res.Port != 80 {
		res.URL = fmt.Sprintf("http://%s (:%d)", p.Hostname(), res.Port)
	}
	return res, nil
}

// runTransient starts a detached transient service via systemd-run.
func (r *Runner) runTransient(p registry.Project, cmd, env []string) error {
	// A unit the kernel killed (an OOM kill under MemoryMax=, say) stays loaded
	// in the failed state, and systemd-run refuses to reuse its name until that
	// is cleared. Nothing to clear is the normal case, so the error is ignored.
	_ = systemctl("reset-failed", UnitName(p.Name))
	log := r.logPath(p)
	args := []string{
		"--user",
		"--unit=" + UnitName(p.Name),
		"--working-directory=" + p.Dir,
		"--setenv=" + config.EnvMarker + "=" + p.Name,
		"-p", "StandardOutput=append:" + log,
		"-p", "StandardError=append:" + log,
	}
	for _, prop := range limitProps(p.Limits) {
		args = append(args, "-p", prop)
	}
	for _, e := range env {
		args = append(args, "--setenv="+e)
	}
	args = append(args, "--")
	args = append(args, cmd...)
	out, err := execCommand("systemd-run", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemd-run failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Stop tears down the project. If it runs under a pm systemd unit, systemctl
// stops the whole cgroup tree. If it's an external/claimed process (no pm
// unit), it falls back to stopping the detected process directly.
//
// A run started by specification (pm run, POST /run) is forgotten once it is
// stopped, so nothing of it is left in the listing. If its process is already
// gone, stopping only forgets the record.
func (r *Runner) Stop(name string) error {
	unit := UnitName(name)
	_ = r.Store.Reload() // the run may have been started by another process
	p, registered := r.Store.Get(name)
	isRun := registered && p.IsRun()
	if unitExists(unit) {
		if err := systemctl("stop", unit); err != nil {
			return err
		}
		// Transient units vanish on stop; persistent ones stay enabled unless demoted.
		_ = systemctl("reset-failed", unit)
		if isRun {
			_ = r.Store.Remove(name)
		}
		return nil
	}
	if isRun {
		return r.Store.Remove(name)
	}
	return r.stopExternal(name)
}

// stopExternal stops a process pm did not launch (adopted via `pm claim`).
func (r *Runner) stopExternal(name string) error {
	insts, _ := r.Snapshot()
	for _, i := range insts {
		if !strings.EqualFold(i.Project, name) {
			continue
		}
		if i.Docker && i.Container != "" {
			if out, err := execCommand("docker", "stop", i.Container).CombinedOutput(); err != nil {
				return fmt.Errorf("docker stop %s: %v: %s", i.Container, err, strings.TrimSpace(string(out)))
			}
			return nil
		}
		if i.PID > 0 {
			killTree(i.PID)
			return nil
		}
	}
	return fmt.Errorf("%q is not running under pm and its process wasn't detected (was it launched by another user or does it need root?)", name)
}

// StopInstance stops a live listener identified by port, even if pm did not
// launch it (adopted/unclaimed). Docker containers are stopped via docker;
// native processes get their tree killed.
func (r *Runner) StopInstance(port int) error {
	for _, i := range r.instancesOnPort(port) {
		if i.Docker && i.Container != "" {
			if out, err := execCommand("docker", "stop", i.Container).CombinedOutput(); err != nil {
				return fmt.Errorf("docker stop: %v: %s", err, strings.TrimSpace(string(out)))
			}
			return nil
		}
		if i.PID > 0 {
			killTree(i.PID)
			return nil
		}
		return fmt.Errorf("port %d has no visible PID (another user's process or needs root)", port)
	}
	return fmt.Errorf("nothing listening on :%d", port)
}

// RestartInstance stops and relaunches a live listener by port. Reliable for
// docker (docker restart); best-effort for native (re-exec its argv in its
// cwd) — fails with a clear message when the command can't be recovered.
func (r *Runner) RestartInstance(port int) (Result, error) {
	for _, i := range r.instancesOnPort(port) {
		if i.Docker && i.Container != "" {
			if out, err := execCommand("docker", "restart", i.Container).CombinedOutput(); err != nil {
				return Result{}, fmt.Errorf("docker restart: %v: %s", err, strings.TrimSpace(string(out)))
			}
			return Result{Project: i.Project, Port: port, Note: "docker restart"}, nil
		}
		if i.PID <= 0 {
			return Result{}, fmt.Errorf("port %d has no visible PID; can't restart it", port)
		}
		argv := procArgv(i.PID)
		cwd := i.Cwd
		if len(argv) == 0 || cwd == "" {
			return Result{}, fmt.Errorf("could not recover the command/cwd of :%d; claim it and configure it to manage it", port)
		}
		killTree(i.PID)
		name := i.Project
		if name == "" {
			name = filepath.Base(cwd)
		}
		relaunch := registry.Project{Name: name, Dir: cwd}
		if sp, ok := r.Store.Get(name); ok {
			relaunch.Limits = sp.Limits // a registered project keeps its caps
		}
		if err := r.runTransient(relaunch, argv, nil); err != nil {
			return Result{}, err
		}
		return Result{Project: name, Port: port, Note: "relaunched"}, nil
	}
	return Result{}, fmt.Errorf("nothing listening on :%d", port)
}

func (r *Runner) instancesOnPort(port int) []detector.Instance {
	insts, _ := r.Snapshot()
	var out []detector.Instance
	for _, i := range insts {
		if i.Port == port {
			out = append(out, i)
		}
	}
	return out
}

// Restart stops then starts again.
func (r *Runner) Restart(name string, opts LaunchOpts) (Result, error) {
	// Checked before stopping: stopping a run forgets it.
	if p, ok := r.Store.Get(name); ok {
		if err := refuseRun(p, "restart"); err != nil {
			return Result{}, err
		}
	}
	_ = r.Stop(name)
	return r.Start(name, opts)
}

// Promote converts a project to the persistent service tier.
func (r *Runner) Promote(name string) error {
	p, ok := r.Store.Get(name)
	if !ok {
		return fmt.Errorf("project %q is not registered", name)
	}
	if err := refuseRun(p, "promote"); err != nil {
		return err
	}
	wasRunning := r.IsActive(name)
	_ = r.Stop(name)
	p.Tier = registry.TierService
	if err := r.Store.Upsert(p); err != nil {
		return err
	}
	if wasRunning {
		_, err := r.launch(p, LaunchOpts{Port: p.PreferPort})
		return err
	}
	return nil
}

// Demote converts a project back to ephemeral and removes its persistent unit.
func (r *Runner) Demote(name string) error {
	p, ok := r.Store.Get(name)
	if !ok {
		return fmt.Errorf("project %q is not registered", name)
	}
	if err := refuseRun(p, "demote"); err != nil {
		return err
	}
	_ = systemctl("disable", "--now", UnitName(name))
	_ = os.Remove(r.unitPath(name))
	_ = systemctl("daemon-reload")
	p.Tier = registry.TierEphemeral
	return r.Store.Upsert(p)
}

// Up starts every project in a group.
func (r *Runner) Up(group string) ([]Result, error) {
	g, ok := r.Store.GetGroup(group)
	if !ok {
		return nil, fmt.Errorf("group %q does not exist", group)
	}
	var out []Result
	for _, name := range g.Projects {
		res, err := r.Start(name, LaunchOpts{})
		if err != nil {
			res = Result{Project: name, Note: "error: " + err.Error()}
		}
		out = append(out, res)
	}
	return out, nil
}

// Down stops every project in a group.
func (r *Runner) Down(group string) error {
	g, ok := r.Store.GetGroup(group)
	if !ok {
		return fmt.Errorf("group %q does not exist", group)
	}
	for _, name := range g.Projects {
		_ = r.Stop(name)
	}
	return nil
}

// IsActive reports whether the project's unit is running.
func (r *Runner) IsActive(name string) bool {
	out, _ := execCommand("systemctl", "--user", "is-active", UnitName(name)).Output()
	return strings.TrimSpace(string(out)) == "active"
}

// UnitState is what systemd says about one pm unit it still has loaded.
type UnitState struct {
	// Active is ActiveState=: "active", "failed", "activating"...
	Active string
	// Result is Result=: "success", "oom-kill", "exit-code", "signal"...
	Result string
	// PID is MainPID=, 0 when there is none.
	PID int
}

// UnitStates returns every pm unit systemd has loaded, keyed by unit name. A
// stopped transient unit is unloaded and absent; a failed one stays until it
// is cleared. One systemctl call covers every project.
func (r *Runner) UnitStates() map[string]UnitState {
	out, err := execCommand("systemctl", "--user", "show", config.UnitPrefix+"*.service",
		"-p", "Id", "-p", "ActiveState", "-p", "Result", "-p", "MainPID").Output()
	if err != nil {
		return nil
	}
	states := map[string]UnitState{}
	for _, block := range strings.Split(string(out), "\n\n") {
		props := map[string]string{}
		for _, line := range strings.Split(block, "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				props[k] = v
			}
		}
		if props["Id"] == "" {
			continue
		}
		pid, _ := strconv.Atoi(props["MainPID"])
		states[props["Id"]] = UnitState{Active: props["ActiveState"], Result: props["Result"], PID: pid}
	}
	return states
}

// FailedUnits returns, for every pm unit systemd still holds in the failed
// state, the reason it gave (the unit's Result: "oom-kill", "exit-code",
// "signal", ...), keyed by unit name. Stopping a project clears its entry.
func (r *Runner) FailedUnits() map[string]string {
	failed := map[string]string{}
	for id, st := range r.UnitStates() {
		if st.Active == "failed" {
			failed[id] = st.Result
		}
	}
	return failed
}

// --- port helpers --------------------------------------------------------

// resolvePort picks the port a launch will use. An explicit opts.Port wins
// over the project's PreferPort. Native projects whose port is already taken
// are bumped to the next free one (or refused with opts.Force); docker
// projects keep their compose mapping untouched. A zero port means "let the
// stack choose" and is discovered after launch.
//
// leased holds the ports reserved by a lease. They count as taken for a port pm
// picks on its own (the project's preferred one, and the replacement when that
// is taken) but not for an explicit opts.Port: pm cannot tell who holds the
// lease, so whoever passes the port is trusted to be its owner.
func resolvePort(p registry.Project, opts LaunchOpts, occupied, leased map[int]bool) (port int, note string, err error) {
	port = opts.Port
	if port == 0 {
		port = p.PreferPort
	}
	if p.Kind != registry.KindNative || port == 0 {
		return port, "", nil
	}
	taken := occupied[port] || (opts.Port == 0 && leased[port])
	if !taken {
		return port, "", nil
	}
	if opts.Force {
		return 0, "", fmt.Errorf("port %d is taken (--force)", port)
	}
	free := nextFree(port, unionPorts(occupied, leased))
	return free, fmt.Sprintf("port %d taken → using %d", port, free), nil
}

func unionPorts(a, b map[int]bool) map[int]bool {
	out := make(map[int]bool, len(a)+len(b))
	for p := range a {
		out[p] = true
	}
	for p := range b {
		out[p] = true
	}
	return out
}

func (r *Runner) occupiedPorts() map[int]bool {
	insts, _ := r.Snapshot()
	return occupiedFrom(insts)
}

// LivePorts returns every port with a listener right now.
func (r *Runner) LivePorts() map[int]bool { return r.occupiedPorts() }

// occupiedFrom collects every bound port from a live snapshot.
func occupiedFrom(insts []detector.Instance) map[int]bool {
	occ := map[int]bool{}
	for _, i := range insts {
		if i.Port != 0 {
			occ[i.Port] = true
		}
	}
	return occ
}

// waitForPort polls the live state until the project's unit exposes a LISTEN.
func (r *Runner) waitForPort(name string, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		insts, _ := r.Snapshot()
		for _, i := range insts {
			if strings.EqualFold(i.Project, name) && i.Port != 0 {
				return i.Port
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return 0
}

func nextFree(from int, occupied map[int]bool) int {
	for p := from + 1; p < from+200; p++ {
		if !occupied[p] {
			return p
		}
	}
	return from
}

// --- persistent unit files ----------------------------------------------

func (r *Runner) unitPath(name string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user", UnitName(name))
}

func (r *Runner) writeUnit(p registry.Project, cmd, env []string) error {
	path := r.unitPath(p.Name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	log := r.Paths.LogFile(p.Name)
	var b strings.Builder
	fmt.Fprintf(&b, "[Unit]\nDescription=pm project %s\nAfter=network.target\n\n", p.Name)
	b.WriteString("[Service]\n")
	fmt.Fprintf(&b, "WorkingDirectory=%s\n", p.Dir)
	fmt.Fprintf(&b, "Environment=%s=%s\n", config.EnvMarker, p.Name)
	for _, e := range env {
		fmt.Fprintf(&b, "Environment=%s\n", e)
	}
	fmt.Fprintf(&b, "ExecStart=%s\n", shellJoin(cmd))
	fmt.Fprintf(&b, "StandardOutput=append:%s\nStandardError=append:%s\n", log, log)
	for _, prop := range limitProps(p.Limits) {
		b.WriteString(prop + "\n")
	}
	if p.AutoRestart {
		b.WriteString("Restart=on-failure\nRestartSec=2\n")
	}
	b.WriteString("\n[Install]\nWantedBy=default.target\n")
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// limitProps turns limits into systemd properties. The same strings feed
// `systemd-run -p` for transient units and the [Service] section of a
// persistent one. Callers validate the limits first.
//
// A memory cap also pins MemorySwapMax=0: MemoryMax= alone only makes the
// kernel reclaim into swap, so on a host with swap (WSL2 has some) the process
// keeps running over its limit and is never OOM-killed. "infinity" means no
// cap, so it gets no swap restriction either.
func limitProps(l registry.Limits) []string {
	var props []string
	if l.Memory != "" {
		props = append(props, "MemoryMax="+l.Memory)
		if l.Memory != "infinity" {
			props = append(props, "MemorySwapMax=0")
		}
	}
	if l.CPU != "" {
		props = append(props, "CPUQuota="+l.CPU)
	}
	return props
}

// --- command / port strategy --------------------------------------------

// applyPort returns the command and extra env (KEY=VAL) with the chosen port
// injected according to the project's strategy.
func applyPort(p registry.Project, port int) (cmd []string, env []string) {
	cmd = append([]string{}, p.Cmd...)
	if p.Kind == registry.KindDocker || port == 0 {
		return cmd, nil
	}
	s := p.PortStrategy
	switch {
	case s.Env != "":
		env = append(env, s.Env+"="+strconv.Itoa(port))
	case s.Template != "":
		tokens := strings.Fields(strings.ReplaceAll(s.Template, "{port}", strconv.Itoa(port)))
		cmd = append(cmd, tokens...)
	case s.Flag != "":
		if isNodePM(cmd) && !hasDashDash(cmd) {
			cmd = append(cmd, "--")
		}
		cmd = append(cmd, s.Flag, strconv.Itoa(port))
	}
	return cmd, env
}

func isNodePM(cmd []string) bool {
	if len(cmd) == 0 {
		return false
	}
	switch cmd[0] {
	case "npm", "pnpm", "yarn":
		return true
	}
	return false
}

func hasDashDash(cmd []string) bool {
	for _, a := range cmd {
		if a == "--" {
			return true
		}
	}
	return false
}

func shellJoin(cmd []string) string {
	parts := make([]string, len(cmd))
	for i, c := range cmd {
		if strings.ContainsAny(c, " \t\"'") {
			parts[i] = strconv.Quote(c)
		} else {
			parts[i] = c
		}
	}
	return strings.Join(parts, " ")
}

func systemctl(args ...string) error {
	full := append([]string{"--user"}, args...)
	out, err := execCommand("systemctl", full...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// SortedPorts is a small helper for stable output in tests/CLI.
func SortedPorts(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}
