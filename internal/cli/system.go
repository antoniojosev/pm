package cli

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/antoniojosev/pm/internal/config"
	"github.com/antoniojosev/pm/internal/proxy"
	"github.com/spf13/cobra"
)

func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose the environment (systemd, cgroups, ss, docker, caddy)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			check("systemd --user", func() (string, bool) {
				out, _ := exec.Command("systemctl", "--user", "is-system-running").Output()
				st := strings.TrimSpace(string(out))
				return st, st == "running" || st == "degraded"
			})
			check("cgroup v2", func() (string, bool) {
				_, err := os.Stat("/sys/fs/cgroup/cgroup.controllers")
				return "unified", err == nil
			})
			check("systemd-run", bin("systemd-run"))
			check("ss", bin("ss"))
			check("docker", bin("docker"))
			check("caddy", func() (string, bool) {
				if proxy.Available() {
					return "installed", true
				}
				return "not installed (pm setup installs it)", false
			})
			check("daemon", func() (string, bool) {
				out, err := exec.Command("curl", "-s", config.DaemonURL()+"/api/health").Output()
				return config.DaemonURL(), err == nil && strings.Contains(string(out), "ok")
			})
			return nil
		},
	}
}

func check(name string, fn func() (string, bool)) {
	detail, ok := fn()
	glyph := styleOK.Render(glyphOK)
	if !ok {
		glyph = styleFail.Render(glyphFail)
	}
	fmt.Printf(" %s  %s %s\n", glyph, styleName.Render(fmt.Sprintf("%-28s", name)), styleDim.Render(detail))
}

func bin(name string) func() (string, bool) {
	return func() (string, bool) {
		p, err := exec.LookPath(name)
		if err != nil {
			return "not found", false
		}
		return p, true
	}
}

func setupCmd() *cobra.Command {
	var installCaddy bool
	c := &cobra.Command{
		Use:   "setup",
		Short: "Get pm running: prepares ~/.pm, starts daemon+proxy and sets up the MCP",
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths := config.Resolve()
			step("Preparing %s", paths.Root)
			if err := paths.EnsureDirs(); err != nil {
				return err
			}
			if err := proxy.Write(paths.Caddyfile, nil, config.DefaultDaemonPort); err != nil {
				return err
			}
			done("directories and Caddyfile ready")

			// 1) daemon: write unit, reload, enable and start
			step("Starting the daemon (API + web + MCP)")
			if err := writeDaemonUnit(); err != nil {
				return fmt.Errorf("could not write the daemon unit: %w", err)
			}
			_ = sc("daemon-reload")
			if err := sc("enable", "--now", "pm-daemon.service"); err != nil {
				warn("could not start pm-daemon: %v", err)
			}
			if waitHealth(6 * time.Second) {
				done("daemon alive at " + config.DaemonURL())
			} else {
				warn("the daemon didn't answer health in time (check: journalctl --user -u pm-daemon)")
			}

			// 2) Caddy proxy
			step("Configuring the .localhost proxy (Caddy)")
			if !proxy.Available() {
				if installCaddy {
					if err := aptInstallCaddy(); err != nil {
						warn("could not install Caddy: %v", err)
					}
				}
			}
			if proxy.Available() {
				if caddyAlreadyRunning() {
					// Another Caddy is already listening on the admin API (e.g. the
					// apt package's caddy.service). We don't start pm-caddy: the
					// daemon pushes the config via `caddy reload` to the existing Caddy.
					_ = sc("disable", "--now", "pm-caddy.service")
					_ = sc("reset-failed", "pm-caddy.service")
					_ = proxy.Reload(paths.Caddyfile)
					done("a Caddy is already running; using it (config via reload). Not starting pm-caddy.")
				} else if err := writeProxyUnit(paths.Caddyfile); err != nil {
					warn("could not write the caddy unit: %v", err)
				} else {
					_ = sc("daemon-reload")
					if err := sc("enable", "--now", "pm-caddy.service"); err != nil {
						warn("could not start pm-caddy: %v", err)
					} else {
						done("proxy active → http://pm.localhost and <project>.localhost")
					}
				}
			} else {
				warn("Caddy is not installed. .localhost names won't route until you install it:")
				fmt.Println("      sudo apt install -y caddy      (or: pm setup --install-caddy)")
				fmt.Println("      the rest of pm already works via direct port.")
			}

			// 3) MCP
			step("Preparing the MCP server")
			mcpFile := writeMCPSnippet(paths)
			done("MCP snippet written to " + mcpFile)
			fmt.Println("      add the MCP server to your MCP client's config file (.mcp.json):")
			fmt.Println(`      { "mcpServers": { "pm": { "type": "http", "url": "http://127.0.0.1:9797/mcp" } } }`)

			fmt.Println("\n✅ Done. Try:  pm ps   ·   pm ui   ·   pm run -- <your command>")
			return nil
		},
	}
	c.Flags().BoolVar(&installCaddy, "install-caddy", false, "install Caddy with apt if missing (uses sudo)")
	return c
}

func step(f string, a ...any) { fmt.Printf("\n▸ "+f+"\n", a...) }
func done(f string, a ...any) { fmt.Printf("  ✓ "+f+"\n", a...) }
func warn(f string, a ...any) { fmt.Printf("  ⚠ "+f+"\n", a...) }

// sc runs a systemctl --user command.
func sc(args ...string) error {
	full := append([]string{"--user"}, args...)
	out, err := exec.Command("systemctl", full...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}

// waitHealth polls the daemon health endpoint until ok or timeout.
func waitHealth(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(config.DaemonURL() + "/api/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return true
			}
		}
		time.Sleep(400 * time.Millisecond)
	}
	return false
}

// caddyAlreadyRunning reports whether a Caddy admin endpoint is already up on
// :2019 (e.g. the apt-installed system caddy.service). If so, we reuse it
// instead of starting our own and colliding on the admin port.
func caddyAlreadyRunning() bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:2019", 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func aptInstallCaddy() error {
	fmt.Println("      installing Caddy with apt (may ask for your password)…")
	cmd := exec.Command("sudo", "apt", "install", "-y", "caddy")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func writeMCPSnippet(paths config.Paths) string {
	snippet := fmt.Sprintf("{\n  \"mcpServers\": {\n    \"pm\": { \"type\": \"http\", \"url\": \"%s/mcp\" }\n  }\n}\n", config.DaemonURL())
	path := filepath.Join(paths.Root, "mcp.json")
	_ = os.WriteFile(path, []byte(snippet), 0o644)
	return path
}

func userUnitDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user")
}

func writeProxyUnit(caddyfile string) error {
	dir := userUnitDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	unit := fmt.Sprintf(`[Unit]
Description=pm reverse proxy (Caddy)
After=network.target

[Service]
ExecStart=caddy run --config %s --adapter caddyfile
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
`, caddyfile)
	return os.WriteFile(filepath.Join(dir, "pm-caddy.service"), []byte(unit), 0o644)
}

func writeDaemonUnit() error {
	dir := userUnitDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		self = "pm"
	}
	unit := fmt.Sprintf(`[Unit]
Description=pm daemon (API + web + MCP)
After=network.target

[Service]
ExecStart=%s serve
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
`, self)
	return os.WriteFile(filepath.Join(dir, "pm-daemon.service"), []byte(unit), 0o644)
}
