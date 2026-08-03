// Package registry is the persistent catalog of known projects and groups.
package registry

// Tier describes how a project is expected to run.
type Tier string

const (
	// TierEphemeral is launched on demand as a transient systemd scope.
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
	// Draft marks entries produced by scan that the user has not confirmed.
	Draft bool `json:"draft,omitempty"`
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
