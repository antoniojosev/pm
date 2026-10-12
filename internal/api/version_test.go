package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/antoniojosev/pm/internal/version"
)

// fakeBin drops an executable shell script named name into dir, standing in
// for the real binary on a PATH that contains nothing else.
func fakeBin(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestVersionWithoutAnyBinary(t *testing.T) {
	svc := newTestService(t) // PATH is an empty directory
	got := svc.Version()
	want := VersionInfo{Version: version.Version, API: 1}
	if got != want {
		t.Fatalf("Version() = %+v, want %+v", got, want)
	}
}

func TestVersionDetectsCapabilities(t *testing.T) {
	svc := newTestService(t)
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	fakeBin(t, bin, "caddy", "exit 0")
	fakeBin(t, bin, "docker", `[ "$1" = version ] || exit 2; echo 27.0.0`)
	fakeBin(t, bin, "systemctl", `[ "$1 $2" = "--user is-system-running" ] || exit 2; echo running`)

	got := svc.Version()
	if !got.Caddy || !got.Docker || !got.Systemd {
		t.Fatalf("Version() = %+v, want every capability true", got)
	}
	if got.API != 1 || got.Version != version.Version {
		t.Fatalf("Version() = %+v", got)
	}
}

func TestVersionCapabilitiesAreIndependent(t *testing.T) {
	tests := []struct {
		name                 string
		caddy                bool
		docker, systemctl    string // script body; "" = binary absent
		wantDocker, wantSysd bool
	}{
		{"docker binary but daemon down", false, "exit 1", "", false, false},
		{"systemd degraded still counts", false, "", "echo degraded; exit 1", false, true},
		{"systemd stopping does not", false, "", "echo stopping; exit 1", false, false},
		{"systemd user manager unreachable", false, "", "exit 1", false, false},
		{"caddy only", true, "", "", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t)
			bin := t.TempDir()
			t.Setenv("PATH", bin)
			if tc.caddy {
				fakeBin(t, bin, "caddy", "exit 0")
			}
			if tc.docker != "" {
				fakeBin(t, bin, "docker", tc.docker)
			}
			if tc.systemctl != "" {
				fakeBin(t, bin, "systemctl", tc.systemctl)
			}
			got := svc.Version()
			if got.Caddy != tc.caddy || got.Docker != tc.wantDocker || got.Systemd != tc.wantSysd {
				t.Fatalf("Version() = %+v, want caddy=%v docker=%v systemd=%v", got, tc.caddy, tc.wantDocker, tc.wantSysd)
			}
		})
	}
}
