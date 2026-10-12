package cli

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/antoniojosev/pm/internal/api"
	"github.com/antoniojosev/pm/internal/config"
	"github.com/antoniojosev/pm/internal/proxy"
	"github.com/antoniojosev/pm/internal/version"
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
			check("cgroup v2", func() (string, bool) { return cgroupCheck(cgroupControllers, userCgroupControllers()) })
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
				out, err := exec.Command("curl", "-s", config.DaemonURL()+"/api/v1/health").Output()
				return config.DaemonURL(), err == nil && strings.Contains(string(out), "ok")
			})
			check("api", func() (string, bool) { return apiCheck(config.DaemonURL()) })
			return nil
		},
	}
}

// cgroupControllers exists only on the unified (v2) cgroup hierarchy.
const cgroupControllers = "/sys/fs/cgroup/cgroup.controllers"

// userCgroupControllers lists the controllers delegated to the user manager
// (user@UID.service). pm's units are --user units, so MemoryMax= and CPUQuota=
// only take effect when memory and cpu appear here, not just at the root.
func userCgroupControllers() string {
	uid := strconv.Itoa(os.Getuid())
	return filepath.Join("/sys/fs/cgroup/user.slice", "user-"+uid+".slice", "user@"+uid+".service", "cgroup.controllers")
}

// cgroupCheck reports whether pm's --mem/--cpu limits can be enforced: cgroups
// v2 must be mounted and the user manager must have been delegated the memory
// and cpu controllers. Without either, systemd accepts MemoryMax= and
// CPUQuota= and ignores them, so the check fails with that consequence spelled
// out.
func cgroupCheck(rootControllers, userControllers string) (string, bool) {
	if _, err := os.Stat(rootControllers); err != nil {
		return "unavailable: --mem/--cpu limits will not be enforced (boot with systemd.unified_cgroup_hierarchy=1)", false
	}
	b, err := os.ReadFile(userControllers)
	if err != nil {
		return "unified, but the user manager's controllers are unreadable (" + userControllers + "): --mem/--cpu limits may not be enforced", false
	}
	have := strings.Fields(string(b))
	var missing []string
	for _, c := range []string{"memory", "cpu"} {
		if !slices.Contains(have, c) {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return "unified, but " + strings.Join(missing, " and ") + " not delegated to the user manager: --mem/--cpu limits will not be enforced (enable it with Delegate=memory cpu in a user@.service drop-in)", false
	}
	return "unified", true
}

// apiCheck asks the daemon for GET /api/v1/version and summarises it: release,
// API generation and the capabilities the daemon sees. It fails when the daemon
// is unreachable or speaks a different API generation than this CLI.
func apiCheck(baseURL string) (string, bool) {
	info, err := fetchVersion(baseURL)
	if err != nil {
		return "daemon not reachable (" + err.Error() + ")", false
	}
	yn := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	detail := fmt.Sprintf("pm %s · api %d · caddy %s · docker %s · systemd %s",
		info.Version, info.API, yn(info.Caddy), yn(info.Docker), yn(info.Systemd))
	if info.API != version.API {
		return fmt.Sprintf("%s (this CLI expects api %d)", detail, version.API), false
	}
	return detail, true
}

// fetchVersion reads /api/v1/version from the daemon at baseURL.
func fetchVersion(baseURL string) (api.VersionInfo, error) {
	var info api.VersionInfo
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(strings.TrimRight(baseURL, "/") + "/api/v1/version")
	if err != nil {
		return info, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return info, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return info, fmt.Errorf("bad response: %w", err)
	}
	return info, nil
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
		resp, err := client.Get(config.DaemonURL() + "/api/v1/health")
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
