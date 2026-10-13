package client

import "time"

// The types below mirror the JSON bodies of the pm HTTP API v1, documented in
// docs/api/openapi.yaml. They are declared here, not imported, so the package
// depends on nothing but the standard library; the contract test in this
// package runs them against the real daemon to keep both sides in step.

// VersionInfo is what GET /api/v1/version answers: who is serving, which API
// generation it speaks and which host capabilities it can rely on right now.
type VersionInfo struct {
	// Version is the pm release, e.g. "0.2.0".
	Version string `json:"version"`
	// API is the API generation. Compare it with APIVersion.
	API int `json:"api"`
	// Caddy reports that the caddy binary is on PATH.
	Caddy bool `json:"caddy"`
	// Docker reports that the docker daemon answers.
	Docker bool `json:"docker"`
	// Systemd reports that the systemd --user manager is up: without it pm
	// cannot start anything.
	Systemd bool `json:"systemd"`
}

// LeaseRequest asks for ports to be reserved. The lease does not open the
// ports: it marks them taken so nobody else is handed them.
type LeaseRequest struct {
	// Owner is who the ports are for: a worktree, a script, an agent. Letters,
	// digits, '.', '_' and '-', at most 64 characters.
	Owner string `json:"owner"`
	// Count is how many ports to reserve; 0 means one, the maximum is 100.
	Count int `json:"count"`
	// Prefer is the first port to try; busy ports are skipped, so the lease may
	// hold higher ones. 0 lets pm choose where to start.
	Prefer int `json:"prefer"`
}

// Lease is a set of ports reserved for an owner.
type Lease struct {
	// ID identifies the lease for Leases.Delete.
	ID        string    `json:"id"`
	Owner     string    `json:"owner"`
	Ports     []int     `json:"ports"`
	CreatedAt time.Time `json:"created_at"`
}

// Limits caps the resources of a process (needs cgroups v2).
type Limits struct {
	// Memory is a systemd MemoryMax= value: bytes with an optional K, M, G or T
	// suffix, a percentage of physical memory, or "infinity". E.g. "512M".
	Memory string `json:"memory,omitempty"`
	// CPU is a CPUQuota= value: a percentage of one CPU, so "150%" is a core
	// and a half. E.g. "50%".
	CPU string `json:"cpu,omitempty"`
}

// Compose gives a process a docker compose project of its own, so two copies
// of one repository do not share containers, networks or volumes.
type Compose struct {
	// File is the compose file, absolute or relative to Dir. Empty lets
	// compose look for its default file.
	File string `json:"file,omitempty"`
	// Project is the compose project name: lowercase letters, digits, '_' and
	// '-', starting with a letter or digit, at most 64 characters.
	Project string `json:"project"`
}

// RunSpec describes a process to start without registering it. It runs as the
// unit pm-<Name>.service, shows up in `pm ps` with its owner while it lives
// and is forgotten when it is stopped.
type RunSpec struct {
	// Name identifies the run; Client.Stop takes it. Letters, digits, '.', '_'
	// and '-', starting with a letter or digit, at most 64 characters.
	Name string `json:"name"`
	// Dir is the working directory: an absolute path to an existing directory.
	Dir string `json:"dir"`
	// Cmd is the program to execute and its arguments.
	Cmd []string `json:"cmd"`
	// Env adds environment variables. PM_PROJECT and PM_OWNER are reserved.
	Env map[string]string `json:"env,omitempty"`
	// Limits caps memory and CPU. Not valid together with Compose.
	Limits Limits `json:"limits,omitzero"`
	// Compose names the compose project of a `docker compose` command.
	Compose *Compose `json:"compose,omitempty"`
	// Log is the file stdout and stderr are appended to: an absolute path.
	// Empty means a file under pm's own logs directory.
	Log string `json:"log,omitempty"`
	// Owner is who the run is for; Owners.Stop and Owners.Delete act on it.
	// Empty means pm's default owner.
	Owner string `json:"owner,omitempty"`
	// Ports are the ports the process will use, typically those of a lease. pm
	// does not hand them to the process (put them in Env or Cmd); it checks
	// they are free and keeps them from other leases while the run exists.
	Ports []int `json:"ports,omitempty"`
}

// RunResult is the answer to Client.Run.
type RunResult struct {
	// Unit is the systemd unit that was started, pm-<name>.service.
	Unit string `json:"unit"`
	// PID is the main process systemd reports; 0 when it is not known.
	PID int `json:"pid"`
	// Ports are the ports the run holds.
	Ports []int `json:"ports"`
}

// Stopped is one thing that was brought down when an owner was stopped.
type Stopped struct {
	// Kind says what it was, e.g. "project", "run", "container" or "process".
	Kind string `json:"kind"`
	// Name is the project, run or container name.
	Name string `json:"name,omitempty"`
	// PID is the process, for Kind "process".
	PID int `json:"pid,omitempty"`
}

// OwnerResult is what stopping or deleting an owner did.
type OwnerResult struct {
	Owner string `json:"owner"`
	// Stopped is every process brought down.
	Stopped []Stopped `json:"stopped"`
	// LeasesReleased are the ids of the leases released: always empty for
	// Owners.Stop, which keeps them.
	LeasesReleased []string `json:"leases_released"`
	// Errors are the things that could not be stopped, one message each.
	Errors []string `json:"errors,omitempty"`
}

// Row is one line of PS: a registered project, a process started with Run or
// a listener nobody claims (Name "?").
type Row struct {
	// Name is the project or run; "?" for an unclaimed listener.
	Name string `json:"name"`
	Port int    `json:"port"`
	// URL is http://<host>.localhost, through the proxy.
	URL string `json:"url"`
	// LocalURL is http://localhost:<port>, direct.
	LocalURL string `json:"local_url,omitempty"`
	Tier     string `json:"tier"`
	// Status is "up", "down", "failed" or "unclaimed".
	Status string `json:"status"`
	// Failure says why a "failed" unit failed, as systemd reports it:
	// "oom-kill", "exit-code", "signal"...
	Failure string `json:"failure,omitempty"`
	Kind    string `json:"kind"`
	Stack   string `json:"stack,omitempty"`
	Source  string `json:"source,omitempty"`
	PID     int    `json:"pid,omitempty"`
	Unit    string `json:"unit,omitempty"`
	Group   string `json:"group,omitempty"`
	// Owner is who the process is for: set for a Run and for an unclaimed
	// process that carries a PM_OWNER marker, which the daemon reads from that
	// process's environment.
	Owner  string `json:"owner,omitempty"`
	Cmd    string `json:"cmd,omitempty"`
	Cwd    string `json:"cwd,omitempty"`
	Docker bool   `json:"docker,omitempty"`
	Draft  bool   `json:"draft,omitempty"`
	// System marks an unmanaged listener with no PID: a system or root
	// process pm cannot manage.
	System bool `json:"system,omitempty"`
}

// Detected is what Detect infers about a directory.
type Detected struct {
	// OK is false when nothing was recognised; the other fields are then empty.
	OK    bool   `json:"ok"`
	Stack string `json:"stack"`
	// Cmd is the launch command; nil when nothing was recognised.
	Cmd []string `json:"cmd"`
	// PreferPort is the stack's usual port, 0 when it has none.
	PreferPort int `json:"prefer_port"`
	// Kind is "native" or "docker"; empty when nothing was recognised.
	Kind string `json:"kind"`
}

// RouteRequest asks for Host to be served on 127.0.0.1:Port.
type RouteRequest struct {
	// Host is <name>.localhost: DNS labels of letters, digits and '-', ending
	// in ".localhost". pm.localhost is the dashboard's and cannot be used.
	// It is stored lowercase.
	Host string `json:"host"`
	// Port is where it answers, 1 to 65535. Nothing needs to listen there yet.
	Port int `json:"port"`
	// Owner is optional, with the grammar of a lease owner. Owners.Delete
	// removes the routes of its owner.
	Owner string `json:"owner,omitempty"`
}

// Route is an explicit route as stored.
type Route struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	// Owner is empty when the route has none.
	Owner string `json:"owner"`
}

// RouteInfo is a route the proxy serves, with where it comes from.
type RouteInfo struct {
	Host  string `json:"host"`
	Port  int    `json:"port"`
	Owner string `json:"owner"`
	// Source is "explicit" (declared with Routes.Add) or "project" (derived
	// from a running project).
	Source string `json:"source"`
}

// Where a RouteInfo comes from.
const (
	RouteExplicit = "explicit"
	RouteProject  = "project"
)
