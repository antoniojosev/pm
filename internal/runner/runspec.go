package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/antoniojosev/pm/internal/config"
	"github.com/antoniojosev/pm/internal/registry"
)

// LaunchGrace is how long a record that is still launching counts as alive
// even though its unit is not active yet. systemd-run takes well under a second;
// the margin only matters when a launch dies without cleaning up, so that its
// record does not hold the name forever.
const LaunchGrace = 30 * time.Second

// MaxRunPorts bounds how many ports one run can declare.
const MaxRunPorts = 100

// RunSpec describes a process to launch without registering it: the body of
// POST /api/v1/run, `pm run --name x -- cmd` and the pm_run MCP tool. The
// process runs as the transient unit pm-<name>.service, shows up in `pm ps`
// with its owner while it lives and is forgotten when it is stopped.
type RunSpec struct {
	// Name identifies the run: the unit is pm-<name>.service and `pm stop
	// <name>` stops it. Letters, digits, '.', '_' and '-', starting with a
	// letter or digit, at most 64 characters.
	Name string `json:"name"`
	// Dir is the working directory: an absolute path to an existing directory.
	Dir string `json:"dir"`
	// Cmd is the argv to execute; Cmd[0] is looked up in the user manager's PATH.
	Cmd []string `json:"cmd"`
	// Env adds environment variables. PM_PROJECT is reserved. The values reach
	// systemd only: they are never written to the registry.
	Env map[string]string `json:"env,omitempty"`
	// Limits caps memory and CPU of the unit (needs cgroups v2).
	Limits registry.Limits `json:"limits,omitzero"`
	// Compose gives the process a compose project of its own, so two runs of
	// the same compose file (two worktrees of one repo) do not share containers,
	// networks or volumes. pm sets COMPOSE_PROJECT_NAME and, when Cmd is
	// `docker compose ...`, adds -f and -p after it (see applyCompose). The run
	// is then a docker run: limits do not apply, the stack's containers are
	// attributed to it by their com.docker.compose.project label.
	Compose *registry.Compose `json:"compose,omitempty"`
	// Log is the file stdout and stderr are appended to: an absolute path.
	// Empty means ~/.pm/logs/<name>.log.
	Log string `json:"log,omitempty"`
	// Owner is who the run is for. Empty means registry.DefaultOwner.
	Owner string `json:"owner,omitempty"`
	// Ports are the ports the process will use, typically those of a lease.
	// pm does not pass them to the process (put them in Env, or in Cmd, the
	// way your stack wants them); it checks they are free, and keeps them away
	// from other leases and launches while the run exists.
	Ports []int `json:"ports,omitempty"`
}

// RunResult is the answer to a run: the unit that was started, the pid systemd
// reports for it and the ports the run holds.
type RunResult struct {
	Unit  string `json:"unit"`
	PID   int    `json:"pid"`
	Ports []int  `json:"ports"`
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Normalize validates the spec and returns it with the defaults applied (the
// owner). Nothing is touched on disk or in systemd.
func (s RunSpec) Normalize() (RunSpec, error) {
	if err := registry.ValidateRunName(s.Name); err != nil {
		return s, err
	}
	if s.Dir == "" {
		return s, fmt.Errorf("dir is required")
	}
	if !filepath.IsAbs(s.Dir) {
		return s, fmt.Errorf("invalid dir %q: use an absolute path", s.Dir)
	}
	if fi, err := os.Stat(s.Dir); err != nil || !fi.IsDir() {
		return s, fmt.Errorf("invalid dir %q: not an existing directory", s.Dir)
	}
	if len(s.Cmd) == 0 || s.Cmd[0] == "" {
		return s, fmt.Errorf("cmd is required: the program to run and its arguments")
	}
	for _, a := range s.Cmd {
		if strings.ContainsRune(a, 0) || !utf8.ValidString(a) {
			return s, fmt.Errorf("invalid cmd: arguments must be valid text without NUL bytes")
		}
	}
	for k, v := range s.Env {
		if !envKeyRe.MatchString(k) {
			return s, fmt.Errorf("invalid env name %q: use letters, digits and '_', not starting with a digit", k)
		}
		if k == config.EnvMarker {
			return s, fmt.Errorf("env %s is reserved: pm sets it to the run's name", k)
		}
		if strings.ContainsRune(v, 0) || !utf8.ValidString(v) {
			return s, fmt.Errorf("invalid value for env %s: must be valid text without NUL bytes", k)
		}
	}
	if err := s.Limits.Validate(); err != nil {
		return s, err
	}
	if s.Compose != nil {
		// Checked as the docker project the run becomes: it refuses limits.
		if err := (registry.Project{Kind: registry.KindDocker, Limits: s.Limits, Compose: s.Compose}).Validate(); err != nil {
			return s, err
		}
		if _, set := s.Env[EnvComposeProject]; set {
			return s, fmt.Errorf("env %s is reserved with compose: pm sets it from compose.project", EnvComposeProject)
		}
		if _, set := s.Env[EnvComposeFile]; set && s.Compose.File != "" {
			return s, fmt.Errorf("env %s is reserved with compose.file: pm sets it itself", EnvComposeFile)
		}
	}
	if s.Log != "" {
		if !filepath.IsAbs(s.Log) || strings.ContainsFunc(s.Log, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return s, fmt.Errorf("invalid log %q: use an absolute path without control characters", s.Log)
		}
		if fi, err := os.Stat(s.Log); err == nil && fi.IsDir() {
			return s, fmt.Errorf("invalid log %q: it is a directory", s.Log)
		}
	}
	if s.Owner == "" {
		s.Owner = registry.DefaultOwner
	}
	if err := registry.ValidateOwner(s.Owner); err != nil {
		return s, err
	}
	if len(s.Ports) > MaxRunPorts {
		return s, fmt.Errorf("too many ports: at most %d", MaxRunPorts)
	}
	seen := map[int]bool{}
	for _, p := range s.Ports {
		if p < 1 || p > 65535 {
			return s, fmt.Errorf("invalid port %d: use a port from 1 to 65535", p)
		}
		if seen[p] {
			return s, fmt.Errorf("port %d is listed twice", p)
		}
		seen[p] = true
	}
	return s, nil
}

// project is the registry entry that stands for the run while it lives.
func (s RunSpec) project() registry.Project {
	kind := registry.KindNative
	var compose *registry.Compose
	if s.Compose != nil {
		kind = registry.KindDocker
		c := *s.Compose
		compose = &c
	}
	return registry.Project{
		Name:    s.Name,
		Dir:     s.Dir,
		Cmd:     append([]string{}, s.Cmd...),
		Kind:    kind,
		Tier:    registry.TierEphemeral,
		Limits:  s.Limits,
		Compose: compose,
		Owner:   s.Owner,
		Run: &registry.RunInfo{
			Ports:     append([]int{}, s.Ports...),
			Log:       s.Log,
			Nonce:     registry.NewRunNonce(),
			Started:   time.Now().UTC(),
			Launching: true,
		},
	}
}

// LaunchSpec starts the process the spec describes. It records the run in the
// registry first (that is what makes the name and ports exclusive) and takes
// the record back if systemd refuses to start the unit. Whether the ports are
// free of listeners is the caller's check: Service.StartRun does it.
func (r *Runner) LaunchSpec(spec RunSpec) (RunResult, error) {
	spec, err := spec.Normalize()
	if err != nil {
		return RunResult{}, err
	}
	if err := r.Paths.EnsureDirs(); err != nil {
		return RunResult{}, err
	}
	p := spec.project()
	if spec.Log != "" {
		if err := os.MkdirAll(filepath.Dir(spec.Log), 0o755); err != nil {
			return RunResult{}, fmt.Errorf("cannot create the log directory: %w", err)
		}
	}
	// Ports of runs that died must not block this one.
	r.ReleaseDeadRunPorts()
	// A dead run of the same name (it exited, or failed) is replaced; a live
	// one, or one that is still being launched, is not.
	if err := r.Store.AddRun(p, r.RunDead); err != nil {
		return RunResult{}, err
	}

	env := make([]string, 0, len(spec.Env))
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	cmd, composeEnv := applyCompose(spec.Cmd, p.Compose)
	env = append(env, composeEnv...)
	sort.Strings(env)
	if err := r.runTransient(p, cmd, env); err != nil {
		// Only our own record: a concurrent launch of the same name cannot
		// have one (AddRun refused it), but a later one may have written its own.
		_ = r.Store.RemoveRun(p.Name, p.Run.Nonce)
		return RunResult{}, err
	}
	_ = r.Store.FinishLaunch(p.Name, p.Run.Nonce)
	res := RunResult{Unit: UnitName(p.Name), PID: r.mainPID(UnitName(p.Name)), Ports: p.Run.Ports}
	return res, nil
}

// launching reports whether the record is a run whose launch is in flight.
func launching(p registry.Project) bool {
	return p.IsRun() && p.Run.Launching && time.Since(p.Run.Started) < LaunchGrace
}

// RunDead reports whether a run record may be replaced by a new run of the same
// name: its unit is not active and its launch is not still in flight.
func (r *Runner) RunDead(p registry.Project) bool {
	return !launching(p) && !r.IsActive(p.Name)
}

// ReleaseDeadRunPorts lets go of the ports of runs whose process is gone, so
// they stop blocking leases and launches until someone runs `pm stop`. The
// record stays (pm ps still shows how the run ended). systemd is asked once,
// outside the store's lock; when it cannot answer nothing is released.
func (r *Runner) ReleaseDeadRunPorts() {
	_ = r.Store.Reload()
	var held []registry.Project
	for _, p := range r.Store.List() {
		if p.IsRun() && !p.Run.Released && len(p.Run.Ports) > 0 && !launching(p) {
			held = append(held, p)
		}
	}
	if len(held) == 0 {
		return
	}
	units := r.UnitStates()
	if units == nil {
		return
	}
	for _, p := range held {
		if st, ok := units[UnitName(p.Name)]; ok && (st.Active == "active" || st.Active == "activating" || st.Active == "reloading") {
			continue
		}
		_ = r.Store.ReleaseRunPorts(p.Name, p.Run.Nonce)
	}
}

// mainPID asks systemd for the pid of a unit's main process, 0 when unknown.
func (r *Runner) mainPID(unit string) int {
	out, err := execCommand("systemctl", "--user", "show", unit, "-p", "MainPID", "--value").Output()
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return pid
}

// LogPath is where a project's output goes: the file a run was given, else
// ~/.pm/logs/<name>.log.
func (r *Runner) LogPath(name string) string {
	if p, ok := r.Store.Get(name); ok {
		return r.logPath(p)
	}
	return r.Paths.LogFile(name)
}

func (r *Runner) logPath(p registry.Project) string {
	if p.Run != nil && p.Run.Log != "" {
		return p.Run.Log
	}
	return r.Paths.LogFile(p.Name)
}

// refuseRun rejects verb on a run entry: it has no recipe to repeat (its
// environment was never stored) and no catalog entry to change. Starting the
// process again means running it again.
func refuseRun(p registry.Project, verb string) error {
	if !p.IsRun() {
		return nil
	}
	return fmt.Errorf("cannot %s %q: it was started with pm run and is not a registered project; run it again instead", verb, p.Name)
}
