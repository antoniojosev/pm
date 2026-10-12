package api

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/antoniojosev/pm/internal/proxy"
	"github.com/antoniojosev/pm/internal/version"
)

// VersionInfo is what GET /api/v1/version answers: who is serving, which API
// generation it speaks and which host capabilities it can rely on right now.
type VersionInfo struct {
	Version string `json:"version"`
	API     int    `json:"api"`
	Caddy   bool   `json:"caddy"`   // the caddy binary is on PATH
	Docker  bool   `json:"docker"`  // the docker daemon answers
	Systemd bool   `json:"systemd"` // the systemd --user manager is up
}

// probeTimeout bounds each capability probe so a hung docker or systemd
// cannot stall the endpoint.
const probeTimeout = 2 * time.Second

// Version reports the running version and probes the host capabilities. The
// probes run on every call (nothing is cached), so a daemon started before
// docker came up still reports the truth later.
func (s *Service) Version() VersionInfo {
	return VersionInfo{
		Version: version.Version,
		API:     version.API,
		Caddy:   proxy.Available(),
		Docker:  probe("docker", "version", "--format", "{{.Server.Version}}"),
		Systemd: systemdUp(),
	}
}

// probe reports whether the command exits 0 within probeTimeout.
func probe(name string, args ...string) bool {
	if _, err := exec.LookPath(name); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Run() == nil
}

// systemdUp reports whether the user manager can run units: the same states
// `pm doctor` accepts (a degraded manager still runs services).
func systemdUp() bool {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	// is-system-running exits non-zero for "degraded", so the output decides.
	out, _ := exec.CommandContext(ctx, "systemctl", "--user", "is-system-running").Output()
	st := strings.TrimSpace(string(out))
	return st == "running" || st == "degraded"
}
