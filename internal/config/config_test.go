package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveHonorsPMHome(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PM_HOME", root)

	p := Resolve()
	want := Paths{
		Root:      root,
		Registry:  filepath.Join(root, "registry.json"),
		LogsDir:   filepath.Join(root, "logs"),
		RunDir:    filepath.Join(root, "run"),
		Caddyfile: filepath.Join(root, "Caddyfile"),
	}
	if p != want {
		t.Fatalf("Resolve = %+v, want %+v", p, want)
	}
	if got := p.LogFile("web"); got != filepath.Join(root, "logs", "web.log") {
		t.Fatalf("LogFile = %q", got)
	}
}

func TestResolveDefaultsToHomeDotPM(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PM_HOME", "")
	t.Setenv("HOME", home)

	if p := Resolve(); p.Root != filepath.Join(home, ".pm") {
		t.Fatalf("Root = %q, want %q", p.Root, filepath.Join(home, ".pm"))
	}
}

func TestEnsureDirs(t *testing.T) {
	t.Setenv("PM_HOME", filepath.Join(t.TempDir(), "nested", "pm"))
	p := Resolve()
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	for _, d := range []string{p.Root, p.LogsDir, p.RunDir} {
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			t.Errorf("%s not created", d)
		}
	}
	// idempotent
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("second EnsureDirs: %v", err)
	}
}

func TestDaemonURL(t *testing.T) {
	t.Setenv("PM_DAEMON_URL", "")
	if got := DaemonURL(); got != "http://127.0.0.1:9797" {
		t.Fatalf("default DaemonURL = %q", got)
	}
	t.Setenv("PM_DAEMON_URL", "http://10.0.0.2:1234")
	if got := DaemonURL(); got != "http://10.0.0.2:1234" {
		t.Fatalf("overridden DaemonURL = %q", got)
	}
}

func TestResolveRemoteSuffix(t *testing.T) {
	tests := []struct{ env, want string }{
		{"", ".pm"},
		{"  ", ".pm"},
		{"off", ""},
		{"-", ""},
		{"tail", ".tail"},
		{".tail", ".tail"},
	}
	for _, tc := range tests {
		t.Setenv("PM_REMOTE_SUFFIX", tc.env)
		if got := resolveRemoteSuffix(); got != tc.want {
			t.Errorf("PM_REMOTE_SUFFIX=%q → %q, want %q", tc.env, got, tc.want)
		}
	}
}
