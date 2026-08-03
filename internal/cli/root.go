// Package cli implements the pm command-line, operating directly on the core
// service so it works standalone whether or not the daemon is running.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/antoniojosev/pm/internal/api"
	"github.com/antoniojosev/pm/internal/config"
	"github.com/antoniojosev/pm/internal/registry"
	"github.com/spf13/cobra"
)

// Version is stamped at build time.
var Version = "0.1.0"

func svc() (*api.Service, error) {
	paths := config.Resolve()
	if err := paths.EnsureDirs(); err != nil {
		return nil, err
	}
	store, err := registry.Open(paths)
	if err != nil {
		return nil, err
	}
	return api.New(paths, store, roots()), nil
}

// roots returns the directories scanned for projects (PM_ROOTS overrides).
func roots() []string {
	if v := os.Getenv("PM_ROOTS"); v != "" {
		return strings.Split(v, ":")
	}
	home, _ := os.UserHomeDir()
	return []string{filepath.Join(home, "projects")}
}

// Command group IDs used to organize `pm --help`.
const (
	grpLifecycle = "lifecycle"
	grpRegistry  = "registry"
	grpState     = "state"
	grpSystem    = "system"
)

// Execute is the CLI entry point. It prints friendly errors itself and returns
// the underlying error so main can exit non-zero.
func Execute() error {
	root := &cobra.Command{
		Use:     "pm",
		Short:   "pm — local project & port manager",
		Long: styleBrand.Render("pm") + styleDim.Render(" — local project & port manager") + `

Launches projects, knows what's running and on which port, and routes <name>.localhost.

Examples:
  pm ps                  see what's running now, grouped by status
  pm run -- npm run dev  launch the cwd (auto-registers and detects the port)
  pm start web           start a registered project`,
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddGroup(
		&cobra.Group{ID: grpLifecycle, Title: "Lifecycle:"},
		&cobra.Group{ID: grpRegistry, Title: "Registry:"},
		&cobra.Group{ID: grpState, Title: "State & groups:"},
		&cobra.Group{ID: grpSystem, Title: "System:"},
	)
	// Hide the auto-generated `completion` command from the main listing.
	root.CompletionOptions.HiddenDefaultCmd = true

	group := func(id string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = id
			root.AddCommand(c)
		}
	}
	group(grpLifecycle, runCmd(), startCmd(), stopCmd(), restartCmd(), psCmd(), logsCmd(), openCmd(), uiCmd())
	group(grpRegistry, addCmd(), editCmd(), rmCmd(), claimCmd(), scanCmd())
	group(grpState, promoteCmd(), demoteCmd(), upCmd(), downCmd(), groupCmd())
	group(grpSystem, serveCmd(), setupCmd(), doctorCmd(), listenCmd())

	cmd, err := root.ExecuteC()
	if err != nil {
		printCLIError(cmd, err)
	}
	return err
}

// printCLIError renders a colorized error with a usage hint, replacing cobra's
// raw argument messages with friendlier ones.
func printCLIError(cmd *cobra.Command, err error) {
	msg := err.Error()
	hint := ""
	switch {
	case strings.Contains(msg, "arg(s)"):
		msg = "invalid number of arguments"
		hint = cmd.UseLine()
	case strings.HasPrefix(msg, "unknown command"):
		msg = "unknown command"
		hint = cmd.UseLine()
	case strings.HasPrefix(msg, "unknown flag: "):
		msg = "unknown flag: " + strings.TrimPrefix(msg, "unknown flag: ")
		hint = cmd.UseLine()
	case strings.HasPrefix(msg, "unknown shorthand flag"):
		msg = "unknown flag (" + msg + ")"
		hint = cmd.UseLine()
	case strings.HasPrefix(msg, "required flag"):
		msg = "missing a required flag: " + strings.TrimPrefix(msg, "required flag(s) ")
		hint = cmd.UseLine()
	}
	fmt.Fprintln(os.Stderr, styleFail.Render(glyphFail)+" "+msg)
	if hint != "" {
		fmt.Fprintln(os.Stderr, "  "+styleDim.Render("usage: "+hint))
	}
}

func die(err error) {
	fmt.Fprintln(os.Stderr, styleFail.Render(glyphFail)+" "+err.Error())
	os.Exit(1)
}

func projectFromFlags(cmd *cobra.Command) registry.Project {
	name, _ := cmd.Flags().GetString("name")
	dir, _ := cmd.Flags().GetString("dir")
	port, _ := cmd.Flags().GetInt("port")
	cmdline, _ := cmd.Flags().GetString("cmd")
	host, _ := cmd.Flags().GetString("host")
	group, _ := cmd.Flags().GetString("group")
	p := registry.Project{
		Name:       name,
		Dir:        dir,
		PreferPort: port,
		Host:       host,
		Group:      group,
		Tier:       registry.TierEphemeral,
		Kind:       registry.KindNative,
	}
	if cmdline != "" {
		p.Cmd = strings.Fields(cmdline)
	}
	return p
}
