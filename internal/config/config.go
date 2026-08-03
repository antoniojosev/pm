// Package config resolves runtime paths and constants for pm.
package config

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	// UnitPrefix namespaces every systemd unit pm creates, e.g. pm-myapp.scope.
	UnitPrefix = "pm-"
	// EnvMarker is injected into every launched process so we can attribute it back.
	EnvMarker = "PM_PROJECT"
	// DNSSuffix is the local TLD Chrome resolves to loopback with zero config.
	DNSSuffix = ".localhost"
	// DefaultDaemonPort is where the HTTP API + web UI + MCP live.
	DefaultDaemonPort = 9797
	// ProxyPort is the Caddy listen port that routes *.localhost by Host header.
	ProxyPort = 80
)

// RemoteSuffix is an extra DNS suffix Caddy routes alongside .localhost, so
// projects are reachable from other devices (e.g. a phone via Tailscale
// MagicDNS split-DNS: point *.pm at this host's tailnet IP). Default ".pm".
// Configurable via PM_REMOTE_SUFFIX; set it to "off" or "-" to disable.
var RemoteSuffix = resolveRemoteSuffix()

func resolveRemoteSuffix() string {
	s := strings.TrimSpace(os.Getenv("PM_REMOTE_SUFFIX"))
	if s == "" {
		s = ".pm"
	}
	if s == "off" || s == "-" {
		return ""
	}
	if !strings.HasPrefix(s, ".") {
		s = "." + s
	}
	return s
}

// Paths holds every filesystem location pm uses at runtime.
type Paths struct {
	Root      string // ~/.pm
	Registry  string // ~/.pm/registry.json
	LogsDir   string // ~/.pm/logs
	RunDir    string // ~/.pm/run
	Caddyfile string // ~/.pm/Caddyfile
}

// Resolve returns the runtime paths, honoring $PM_HOME when set.
func Resolve() Paths {
	root := os.Getenv("PM_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		root = filepath.Join(home, ".pm")
	}
	return Paths{
		Root:      root,
		Registry:  filepath.Join(root, "registry.json"),
		LogsDir:   filepath.Join(root, "logs"),
		RunDir:    filepath.Join(root, "run"),
		Caddyfile: filepath.Join(root, "Caddyfile"),
	}
}

// EnsureDirs creates the runtime directory tree if missing.
func (p Paths) EnsureDirs() error {
	for _, d := range []string{p.Root, p.LogsDir, p.RunDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// LogFile returns the log path for a named project.
func (p Paths) LogFile(name string) string {
	return filepath.Join(p.LogsDir, name+".log")
}

// DaemonURL is the base URL of the local daemon.
func DaemonURL() string {
	if v := os.Getenv("PM_DAEMON_URL"); v != "" {
		return v
	}
	return "http://127.0.0.1:9797"
}
