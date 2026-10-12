// Package registry is the persistent catalog of known projects and groups.
package registry

import (
	"fmt"
	"regexp"
)

// Tier describes how a project is expected to run.
type Tier string

const (
	// TierEphemeral is launched on demand as a transient systemd service.
	TierEphemeral Tier = "ephemeral"
	// TierService is a persistent systemd --user service enabled at boot.
	TierService Tier = "service"
)

// Kind distinguishes native processes from docker-compose stacks.
type Kind string

const (
	KindNative Kind = "native"
	KindDocker Kind = "docker"
)

// PortStrategy encodes how to inject a chosen port into a stack's command.
// The runner substitutes {port} in Template, or appends Flag+port.
type PortStrategy struct {
	// Kind is a human label: vite, next, django, flutter, env, none...
	Kind string `json:"kind,omitempty"`
	// Flag is prepended before the port, e.g. "--port" (vite) or "-p" (next).
	Flag string `json:"flag,omitempty"`
	// Env sets PORT-style var instead of a flag, e.g. "PORT".
	Env string `json:"env,omitempty"`
	// Template, when set, is spliced verbatim with {port}, e.g.
	// "runserver 0.0.0.0:{port}" for django.
	Template string `json:"template,omitempty"`
}

// Limits caps the resources of the unit pm launches. The values are handed to
// systemd verbatim (MemoryMax= and CPUQuota=), so they need cgroups v2. A
// memory cap also forbids swap (MemorySwapMax=0) so the kernel kills the
// process instead of paging it out. A zero value means "no limit".
type Limits struct {
	// Memory is a MemoryMax= value: bytes with an optional K, M, G or T suffix
	// (base 1024), a percentage of physical memory, or "infinity". E.g. "512M".
	Memory string `json:"memory,omitempty"`
	// CPU is a CPUQuota= value: a percentage of one CPU, so "150%" is a core
	// and a half. E.g. "50%".
	CPU string `json:"cpu,omitempty"`
}

var (
	memoryRe = regexp.MustCompile(`^(infinity|[0-9]+[KMGT]?|[0-9]+(\.[0-9]+)?%)$`)
	cpuRe    = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?%$`)
)

// IsZero reports whether no limit is set. It lets encoding/json drop an empty
// limits object (omitzero).
func (l Limits) IsZero() bool { return l.Memory == "" && l.CPU == "" }

// Validate rejects values systemd would not understand. It matters beyond
// typos: the values end up inside a unit file, so anything outside this
// grammar (a newline, say) must never get that far.
func (l Limits) Validate() error {
	if l.Memory != "" && (!memoryRe.MatchString(l.Memory) || isZero(l.Memory)) {
		return fmt.Errorf("invalid memory limit %q: use bytes with K, M, G or T (512M, 2G), a percentage (50%%) or infinity", l.Memory)
	}
	if l.CPU != "" && (!cpuRe.MatchString(l.CPU) || isZero(l.CPU)) {
		return fmt.Errorf("invalid cpu limit %q: use a percentage of one CPU above zero (50%%, 150%%)", l.CPU)
	}
	return nil
}

// isZero reports whether a number-with-suffix value is zero ("0", "0M", "0.0%"):
// systemd accepts them but they would starve the process, never what was meant.
func isZero(v string) bool {
	if v == "infinity" {
		return false
	}
	for _, r := range v {
		if r >= '1' && r <= '9' {
			return false
		}
	}
	return true
}

// Merge returns l with every field set in over replacing the one in l.
func (l Limits) Merge(over Limits) Limits {
	if over.Memory != "" {
		l.Memory = over.Memory
	}
	if over.CPU != "" {
		l.CPU = over.CPU
	}
	return l
}

// Project is a catalog entry. It is a template for launching; a running
// instance may live on a different actual port (see collision handling).
type Project struct {
	Name         string       `json:"name"`
	Dir          string       `json:"dir"`
	Cmd          []string     `json:"cmd"`
	Kind         Kind         `json:"kind"`
	Tier         Tier         `json:"tier"`
	PreferPort   int          `json:"prefer_port,omitempty"`
	PortStrategy PortStrategy `json:"port_strategy,omitempty"`
	Host         string       `json:"host,omitempty"` // <host>.localhost; defaults to Name
	Stack        string       `json:"stack,omitempty"`
	AutoRestart  bool         `json:"auto_restart,omitempty"`
	Group        string       `json:"group,omitempty"`
	// Limits caps memory and CPU of the launched unit (needs cgroups v2).
	Limits Limits `json:"limits,omitzero"`
	// Draft marks entries produced by scan that the user has not confirmed.
	Draft bool `json:"draft,omitempty"`
}

// Validate checks the project's own consistency: valid limits, and none on a
// docker project, where the unit only wraps the `docker compose` client while
// the containers live outside its cgroup, so a cap would do nothing.
func (p Project) Validate() error {
	if err := p.Limits.Validate(); err != nil {
		return err
	}
	if p.Kind == KindDocker && !p.Limits.IsZero() {
		return fmt.Errorf("limits are not supported on docker projects: the unit only wraps the compose client, not the containers; set mem_limit and cpus in the compose file instead")
	}
	return nil
}

// Hostname returns the .localhost name this project answers to. The label is
// slugified (lowercase, non-alphanumeric → hyphen) so it is a valid host —
// e.g. "web-dashboard" → "web-dashboard.localhost".
func (p Project) Hostname() string {
	h := p.Host
	if h == "" {
		h = p.Name
	}
	return slugify(h) + ".localhost"
}

// slugify turns an arbitrary label into a DNS-safe host label.
func slugify(s string) string {
	var b []rune
	prevHyphen := false
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			b = append(b, r+('a'-'A'))
			prevHyphen = false
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b = append(b, r)
			prevHyphen = false
		default:
			if !prevHyphen && len(b) > 0 {
				b = append(b, '-')
				prevHyphen = true
			}
		}
	}
	out := string(b)
	for len(out) > 0 && out[len(out)-1] == '-' {
		out = out[:len(out)-1]
	}
	if out == "" {
		out = "app"
	}
	return out
}

// Group bundles projects that start/stop together (pm up/down).
type Group struct {
	Name     string   `json:"name"`
	Projects []string `json:"projects"`
}

// State is the on-disk document.
type State struct {
	Version  int       `json:"version"`
	Projects []Project `json:"projects"`
	Groups   []Group   `json:"groups"`
}
