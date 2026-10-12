package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/antoniojosev/pm/internal/api"
	"github.com/antoniojosev/pm/internal/registry"
	"github.com/antoniojosev/pm/internal/runner"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

// psOpts holds the display filters for `pm ps`.
type psOpts struct {
	all     bool   // -a/--all: show running + stopped in one flat list
	stopped bool   // --stopped: show only stopped rows
	group   string // --group: filter rows to a single group
}

func psCmd() *cobra.Command {
	var opts psOpts
	c := &cobra.Command{
		Use:     "ps",
		Aliases: []string{"ls"},
		Short:   "Show what's running now (-a includes stopped)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := svc()
			if err != nil {
				return err
			}
			rows, err := s.PS()
			if err != nil {
				return err
			}
			renderPS(os.Stdout, rows, opts)
			return nil
		},
	}
	c.Flags().BoolVarP(&opts.all, "all", "a", false, "include stopped (registered but not running)")
	c.Flags().BoolVar(&opts.stopped, "stopped", false, "show only stopped")
	c.Flags().StringVar(&opts.group, "group", "", "filter by group")
	return c
}

// renderPS prints the running-first process listing per the approved design.
// Rows sit in a single flat list; the leading dot encodes status (green ● up,
// red ✗ failed, gray ○ down). Failed rows are the ones systemd still holds as
// failed, typically a project the kernel killed for passing its memory limit:
// they show in every view because they need attention. Unmanaged/system
// listeners are never shown here — they live in `pm listen`.
func renderPS(w *os.File, rows []api.Row, opts psOpts) {
	var up, down, failed []api.Row
	unmanaged := 0
	for _, r := range rows {
		if opts.group != "" && !strings.EqualFold(r.Group, opts.group) {
			continue
		}
		switch r.Status {
		case "up":
			up = append(up, r)
		case "down":
			down = append(down, r)
		case "failed":
			failed = append(failed, r)
		case "unclaimed":
			unmanaged++
		}
	}

	upPrefix := "  " + styleUp.Render(glyphUp) + " "
	downPrefix := "  " + styleDown.Render(glyphDown) + " "
	failPrefix := "  " + styleFail.Render(glyphFail) + " "
	// printRows lays the up, failed and down groups out (in that order, each
	// with its own dot) under one header, columns sized over all of them.
	printRows := func(groups ...[]api.Row) {
		var all []api.Row
		for _, g := range groups {
			all = append(all, g...)
		}
		nameW, portW, urlW, ownerW := psWidths(all)
		fmt.Fprintln(w)
		fmt.Fprintln(w, colHeader(nameW, portW, urlW, ownerW))
		for i, g := range groups {
			prefix := []string{upPrefix, failPrefix, downPrefix}[i]
			for _, r := range g {
				fmt.Fprintln(w, projectRow(prefix, r, nameW, portW, urlW, ownerW))
			}
		}
	}
	withFailed := func(text string) string {
		if len(failed) == 0 {
			return text
		}
		return fmt.Sprintf("%s · %d failed", text, len(failed))
	}

	fmt.Fprintln(w)

	// --stopped: only what is not running (failed rows first), one dot per row.
	if opts.stopped {
		fmt.Fprintln(w, summaryLine(withFailed(fmt.Sprintf("%d stopped", len(down)))))
		if len(down)+len(failed) == 0 {
			fmt.Fprintln(w, "  "+styleDim.Render("nothing stopped"))
			fmt.Fprintln(w)
			return
		}
		printRows(nil, failed, down)
		fmt.Fprintln(w)
		return
	}

	// -a/--all: running, failed and stopped in one flat list, the dot on each
	// row encoding its status. No section-label lines.
	if opts.all {
		fmt.Fprintln(w, summaryLine(withFailed(fmt.Sprintf("%d running · %d stopped", len(up), len(down)))))
		printRows(up, failed, down)
		fmt.Fprintln(w)
		return
	}

	// default: running (plus anything failed), short.
	if len(up)+len(failed) == 0 {
		fmt.Fprintln(w, summaryLine("nothing running"))
		var hints []string
		if len(down) > 0 {
			hints = append(hints, fmt.Sprintf("pm ps -a to see the %d registered", len(down)))
		}
		hints = append(hints, "pm run -- <cmd> to start something")
		fmt.Fprintln(w, "  "+styleDim.Render(strings.Join(hints, " · ")))
		fmt.Fprintln(w)
		return
	}

	if len(up) == 0 {
		fmt.Fprintln(w, summaryLine(withFailed("nothing running")))
	} else {
		fmt.Fprintln(w, summaryLine(withFailed(fmt.Sprintf("%d running", len(up)))))
	}
	printRows(up, failed, nil)

	// footer: counts of what's hidden + how to reveal it.
	var counts, hints []string
	if len(down) > 0 {
		counts = append(counts, fmt.Sprintf("%d stopped", len(down)))
		hints = append(hints, "pm ps -a")
	}
	if unmanaged > 0 {
		counts = append(counts, fmt.Sprintf("%d unmanaged", unmanaged))
		hints = append(hints, "pm listen")
	}
	if len(counts) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "    "+styleDim.Render(strings.Join(counts, " · ")+"   ·   "+strings.Join(hints, "  ·  ")))
	}
	fmt.Fprintln(w)
}

// summaryLine renders the "  pm   <text>" header line.
func summaryLine(text string) string {
	return "  " + styleBrand.Render("pm") + "   " + styleSummary.Render(text)
}

// colHeader renders the dim, uppercase NAME/PORT/URL/TIER header aligned to the
// data columns (4-space indent, matching the running glyph prefix width). The
// OWNER column is there only when some row has an owner (ownerW > 0).
func colHeader(nameW, portW, urlW, ownerW int) string {
	h := pad("NAME", nameW) + "  " + pad("PORT", portW) + "  " + pad("URL", urlW) + "  " + "TIER"
	if ownerW > 0 {
		h = pad("NAME", nameW) + "  " + pad("PORT", portW) + "  " + pad("URL", urlW) + "  " + pad("TIER", tierW) + "  " + "OWNER"
	}
	return "    " + styleDim.Render(h)
}

// tierW is the width of the TIER column when OWNER follows it: the longest
// tier, "ephemeral".
const tierW = len("ephemeral")

// psWidths computes column widths from the (already truncated) row names,
// never dropping below the header label widths so headers stay aligned. ownerW
// is 0 when no row has an owner, which drops the OWNER column.
func psWidths(rows []api.Row) (nameW, portW, urlW, ownerW int) {
	nameW, portW, urlW = len("NAME"), len("PORT"), len("URL")
	for _, r := range rows {
		nameW = maxi(nameW, lipgloss.Width(truncName(r.Name)))
		portW = maxi(portW, lipgloss.Width(portToken(r.Port)))
		urlW = maxi(urlW, lipgloss.Width(urlToken(r)))
		if r.Owner != "" {
			ownerW = maxi(ownerW, lipgloss.Width(r.Owner))
		}
	}
	if ownerW > 0 {
		ownerW = maxi(ownerW, len("OWNER"))
	}
	return
}

// projectRow renders one aligned NAME/PORT/URL/TIER[/OWNER] row with the given
// prefix.
func projectRow(prefix string, r api.Row, nameW, portW, urlW, ownerW int) string {
	name := styleName.Render(pad(truncName(r.Name), nameW))
	var port string
	if r.Port != 0 {
		port = stylePort.Render(pad(portToken(r.Port), portW))
	} else {
		port = styleDim.Render(pad("—", portW))
	}
	url := styleURL.Render(pad(urlToken(r), urlW))
	tier := styleTier.Render(r.Tier)
	if ownerW > 0 {
		tier = styleTier.Render(pad(r.Tier, tierW))
	}
	line := prefix + name + "  " + port + "  " + url + "  " + tier
	if ownerW > 0 {
		line += "  " + styleName.Render(r.Owner)
	}
	if r.Status == "failed" {
		reason := "failed"
		if r.Failure != "" {
			reason += ": " + r.Failure
		}
		line += "  " + styleFail.Render(reason)
	}
	return line
}

// truncName caps a name at 26 runes, truncating to 25 + "…" when longer.
func truncName(s string) string {
	r := []rune(s)
	if len(r) <= 26 {
		return s
	}
	return string(r[:25]) + "…"
}

func portToken(port int) string {
	if port == 0 {
		return "—"
	}
	return fmt.Sprintf(":%d", port)
}

func urlToken(r api.Row) string {
	if r.Status == "unclaimed" {
		return ""
	}
	return strings.TrimPrefix(r.URL, "http://")
}

// listenCmd shows raw listeners not managed by pm (unmanaged + system).
func listenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "listen",
		Short: "Listening ports pm doesn't manage (unmanaged)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := svc()
			if err != nil {
				return err
			}
			rows, err := s.PS()
			if err != nil {
				return err
			}
			renderListen(os.Stdout, rows)
			return nil
		},
	}
}

// renderListen prints the unmanaged/system listeners as a PORT/PID/PROCESS table.
func renderListen(w *os.File, rows []api.Row) {
	var un []api.Row
	for _, r := range rows {
		if r.Status == "unclaimed" {
			un = append(un, r)
		}
	}
	fmt.Fprintln(w)
	if len(un) == 0 {
		fmt.Fprintln(w, "  "+styleBrand.Render("pm listen")+"   "+styleSummary.Render("no unmanaged ports"))
		fmt.Fprintln(w)
		return
	}
	sort.Slice(un, func(a, b int) bool { return un[a].Port < un[b].Port })

	fmt.Fprintln(w, "  "+styleBrand.Render("pm listen")+"   "+styleSummary.Render(fmt.Sprintf("%d unmanaged ports", len(un))))
	fmt.Fprintln(w)

	portW, pidW := len("PORT"), len("PID")
	for _, r := range un {
		portW = maxi(portW, lipgloss.Width(portToken(r.Port)))
		pidW = maxi(pidW, lipgloss.Width(pidToken(r)))
	}
	fmt.Fprintln(w, "    "+styleDim.Render(pad("PORT", portW)+"  "+pad("PID", pidW)+"  "+"PROCESS"))
	for _, r := range un {
		port := stylePort.Render(pad(portToken(r.Port), portW))
		pid := pad(pidToken(r), pidW)
		fmt.Fprintln(w, "    "+port+"  "+pid+"  "+processName(r))
	}
	fmt.Fprintln(w)
}

func pidToken(r api.Row) string {
	if r.PID == 0 {
		return "—"
	}
	return strconv.Itoa(r.PID)
}

// processName is the process/cmd name for a listener; system listeners with no
// visible pid show a dim "(system)".
func processName(r api.Row) string {
	if f := strings.Fields(r.Cmd); len(f) > 0 {
		return filepath.Base(f[0])
	}
	if r.Docker && r.Name != "" && r.Name != "?" {
		return r.Name
	}
	return styleDim.Render("(system)")
}

// pad right-pads plain text to width w (measured in display cells).
func pad(s string, w int) string {
	gap := w - lipgloss.Width(s)
	if gap <= 0 {
		return s
	}
	return s + strings.Repeat(" ", gap)
}

func maxi(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func runCmd() *cobra.Command {
	var name, owner, logFile, composeProject, composeFile string
	var port int
	var ports []int
	var env []string
	var force, always, asJSON bool
	var limits registry.Limits
	c := &cobra.Command{
		Use:   "run [--name <name>] [-- <cmd>...]",
		Short: "Launch the cwd, or any command by name without registering it",
		Long: `Without --name, launches the cwd as a project: it auto-registers, detects the
stack and its port, and remembers the command and --mem/--cpu.

With --name, starts <cmd> as that name in the cwd and registers nothing. It
runs under systemd as pm-<name>.service, shows in 'pm ps' with its owner and
ports while it lives, and 'pm stop <name>' stops it and forgets it. Give the
--compose-project (and --compose-file) give a docker compose stack a name of its
own: pm sets COMPOSE_PROJECT_NAME and, for 'docker compose ...', adds -p and -f,
so two runs of one compose file (two worktrees) do not share containers or
volumes, and each run's containers show under its own name in 'pm ps'.
Give the ports it will use (from 'pm lease') with --ports; pm checks they are free and
keeps them from other leases, but does not pass them to the process: put them
in --env or in the command, the way your stack wants.

Examples:
  pm run -- npm run dev
  pm run --name api-qa --mem 512M -- npm run dev
  pm run --name api-qa --owner wt-qa --ports 4100 -e PORT=4100 -- ./serve
  pm run --name shop-qa --compose-project shop-qa -- docker compose up
  pm stop api-qa`,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := svc()
			if err != nil {
				return err
			}
			dir, _ := os.Getwd()
			if name == "" {
				for _, f := range []string{"owner", "env", "log", "ports", "json", "compose-project", "compose-file"} {
					if cmd.Flags().Changed(f) {
						return fmt.Errorf("--%s needs --name: it applies to a process started by name, not to the cwd project", f)
					}
				}
				res, err := s.RunAdhoc(dir, args, runner.AdhocOpts{
					LaunchOpts: runner.LaunchOpts{Port: port, Force: force, Limits: limits},
					Always:     always,
				})
				if err != nil {
					return err
				}
				printResult(res)
				return nil
			}
			for _, f := range []string{"port", "force", "always"} {
				if cmd.Flags().Changed(f) {
					return fmt.Errorf("--%s applies to a registered project: drop --name, or 'pm add' the project and 'pm start' it (a named run takes --ports)", f)
				}
			}
			envMap, err := parseEnvFlags(env)
			if err != nil {
				return err
			}
			res, err := s.StartRun(runner.RunSpec{
				Name: name, Dir: dir, Cmd: args, Env: envMap, Limits: limits,
				Log: logFile, Owner: owner, Ports: ports, Compose: composeFromFlags(cmd),
			})
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSONLine(os.Stdout, res)
			}
			printRun(os.Stdout, name, owner, res)
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "start the command under this name without registering it (alone: the project's name, defaults to the dir)")
	c.Flags().IntVar(&port, "port", 0, "pin the port (without --name)")
	c.Flags().BoolVar(&force, "force", false, "fail if the port is taken (without --name)")
	c.Flags().BoolVar(&always, "always", false, "register as a permanent service (without --name)")
	c.Flags().StringVar(&owner, "owner", "", "who the process is for, e.g. a worktree or script name (with --name; default "+registry.DefaultOwner+")")
	c.Flags().StringArrayVarP(&env, "env", "e", nil, "KEY=VALUE environment variable, repeatable (with --name; never stored)")
	c.Flags().StringVar(&logFile, "log", "", "absolute path of the log file (with --name; default ~/.pm/logs/<name>.log)")
	c.Flags().StringVar(&composeProject, "compose-project", "", "compose project name for the stack: pm sets COMPOSE_PROJECT_NAME and adds -p to `docker compose` (with --name)")
	c.Flags().StringVar(&composeFile, "compose-file", "", "compose file, absolute or relative to the cwd; needs --compose-project (with --name)")
	c.Flags().IntSliceVar(&ports, "ports", nil, "ports the process will use, e.g. from pm lease (with --name)")
	c.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON (with --name)")
	limitFlags(c, &limits, "remembered with the project; with --name, for this process only")
	return c
}

// parseEnvFlags turns repeated KEY=VALUE flags into a map. The last one wins.
func parseEnvFlags(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, kv := range pairs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid --env %q: use KEY=VALUE", kv)
		}
		out[k] = v
	}
	return out, nil
}

// printRun reports a process started by name.
func printRun(w io.Writer, name, owner string, res runner.RunResult) {
	if owner == "" {
		owner = registry.DefaultOwner
	}
	line := styleUp.Render("▶") + " " + styleName.Render(name)
	if res.PID != 0 {
		line += " " + styleDim.Render(fmt.Sprintf("pid %d", res.PID))
	}
	if len(res.Ports) > 0 {
		line += " " + stylePort.Render(portList(res.Ports))
	}
	line += "  " + styleDim.Render("owner "+owner+" · "+res.Unit)
	fmt.Fprintln(w, line)
}

func startCmd() *cobra.Command {
	var port int
	var force bool
	var limits registry.Limits
	c := &cobra.Command{
		Use:   "start <name>",
		Short: "Start a registered project",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := svc()
			if err != nil {
				return err
			}
			progress("starting", args[0])
			res, err := s.Start(args[0], runner.LaunchOpts{Port: port, Force: force, Limits: limits})
			progressClear()
			if err != nil {
				return err
			}
			printResult(res)
			return nil
		},
	}
	c.Flags().IntVar(&port, "port", 0, "pin the port")
	c.Flags().BoolVar(&force, "force", false, "fail if the port is taken")
	limitFlags(c, &limits, "this start only, ephemeral projects; pm edit sets them for good, and a service-tier project then needs pm restart")
	return c
}

// limitFlags adds --mem and --cpu, which fill l when given. scope says how long
// the value lasts for that command.
func limitFlags(c *cobra.Command, l *registry.Limits, scope string) {
	c.Flags().StringVar(&l.Memory, "mem", "", memFlagHelp+"; "+scope+")")
	c.Flags().StringVar(&l.CPU, "cpu", "", cpuFlagHelp+"; "+scope+")")
}

const (
	memFlagHelp = "memory limit, e.g. 512M, 2G or 50% (needs cgroups v2"
	cpuFlagHelp = "CPU limit as a percentage of one CPU, e.g. 150% (needs cgroups v2"
)

func stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <name>",
		Short: "Stop a project (kills the whole tree)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := svc()
			if err != nil {
				return err
			}
			progress("stopping", args[0])
			err = s.Stop(args[0])
			progressClear()
			if err != nil {
				return err
			}
			fmt.Printf("%s %s stopped\n", styleDim.Render("■"), styleName.Render(args[0]))
			return nil
		},
	}
}

func restartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart <name>",
		Short: "Restart a project",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := svc()
			if err != nil {
				return err
			}
			progress("restarting", args[0])
			res, err := s.Restart(args[0], runner.LaunchOpts{})
			progressClear()
			if err != nil {
				return err
			}
			printResult(res)
			return nil
		},
	}
}

func promoteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "promote <name>",
		Short: "Convert to a permanent service (starts at boot)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := svc()
			if err != nil {
				return err
			}
			if err := s.Promote(args[0]); err != nil {
				return err
			}
			fmt.Printf("▲ %s is now a permanent service\n", args[0])
			return nil
		},
	}
}

func demoteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "demote <name>",
		Short: "Revert to ephemeral (on-demand)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := svc()
			if err != nil {
				return err
			}
			if err := s.Demote(args[0]); err != nil {
				return err
			}
			fmt.Printf("▼ %s is now ephemeral\n", args[0])
			return nil
		},
	}
}

func upCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "up <group>",
		Short: "Bring up every project in a group",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := svc()
			if err != nil {
				return err
			}
			results, err := s.Up(args[0])
			if err != nil {
				return err
			}
			for _, r := range results {
				printResult(r)
			}
			return nil
		},
	}
}

func downCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "down <group>",
		Short: "Bring down every project in a group",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := svc()
			if err != nil {
				return err
			}
			if err := s.Down(args[0]); err != nil {
				return err
			}
			fmt.Printf("■ group %s down\n", args[0])
			return nil
		},
	}
}

func printResult(r runner.Result) {
	if r.Note != "" {
		fmt.Printf("%s %s\n", styleUnclaimed.Render("⚠"), r.Note)
	}
	line := styleUp.Render("▶") + " " + styleName.Render(r.Project)
	if r.Port != 0 {
		line += " " + stylePort.Render(fmt.Sprintf(":%d", r.Port))
	}
	if r.URL != "" {
		line += "  " + styleDim.Render("→ "+strings.TrimPrefix(r.URL, "http://"))
	}
	fmt.Println(line)
}
