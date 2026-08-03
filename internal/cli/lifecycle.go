package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/antoniojosev/pm/internal/api"
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
// gray ○ down). Unmanaged/system listeners are never shown here — they live in
// `pm listen`.
func renderPS(w *os.File, rows []api.Row, opts psOpts) {
	var up, down []api.Row
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
		case "unclaimed":
			unmanaged++
		}
	}

	upPrefix := "  " + styleUp.Render(glyphUp) + " "
	downPrefix := "  " + styleDown.Render(glyphDown) + " "

	fmt.Fprintln(w)

	// --stopped: only the stopped rows, gray dot per row.
	if opts.stopped {
		fmt.Fprintln(w, summaryLine(fmt.Sprintf("%d stopped", len(down))))
		if len(down) == 0 {
			fmt.Fprintln(w, "  "+styleDim.Render("nothing stopped"))
			fmt.Fprintln(w)
			return
		}
		nameW, portW, urlW := psWidths(down)
		fmt.Fprintln(w)
		fmt.Fprintln(w, colHeader(nameW, portW, urlW))
		for _, r := range down {
			fmt.Fprintln(w, projectRow(downPrefix, r, nameW, portW, urlW))
		}
		fmt.Fprintln(w)
		return
	}

	// -a/--all: running + stopped in one flat list, running rows first, the dot
	// on each row encoding its status. No section-label lines.
	if opts.all {
		fmt.Fprintln(w, summaryLine(fmt.Sprintf("%d running · %d stopped", len(up), len(down))))
		both := append(append([]api.Row{}, up...), down...)
		nameW, portW, urlW := psWidths(both)
		fmt.Fprintln(w)
		fmt.Fprintln(w, colHeader(nameW, portW, urlW))
		for _, r := range up {
			fmt.Fprintln(w, projectRow(upPrefix, r, nameW, portW, urlW))
		}
		for _, r := range down {
			fmt.Fprintln(w, projectRow(downPrefix, r, nameW, portW, urlW))
		}
		fmt.Fprintln(w)
		return
	}

	// default: running only, short.
	if len(up) == 0 {
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

	fmt.Fprintln(w, summaryLine(fmt.Sprintf("%d running", len(up))))
	nameW, portW, urlW := psWidths(up)
	fmt.Fprintln(w)
	fmt.Fprintln(w, colHeader(nameW, portW, urlW))
	for _, r := range up {
		fmt.Fprintln(w, projectRow(upPrefix, r, nameW, portW, urlW))
	}

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
// data columns (4-space indent, matching the running glyph prefix width).
func colHeader(nameW, portW, urlW int) string {
	return "    " + styleDim.Render(pad("NAME", nameW)+"  "+pad("PORT", portW)+"  "+pad("URL", urlW)+"  "+"TIER")
}

// psWidths computes column widths from the (already truncated) row names,
// never dropping below the header label widths so headers stay aligned.
func psWidths(rows []api.Row) (nameW, portW, urlW int) {
	nameW, portW, urlW = len("NAME"), len("PORT"), len("URL")
	for _, r := range rows {
		nameW = maxi(nameW, lipgloss.Width(truncName(r.Name)))
		portW = maxi(portW, lipgloss.Width(portToken(r.Port)))
		urlW = maxi(urlW, lipgloss.Width(urlToken(r)))
	}
	return
}

// projectRow renders one aligned NAME/PORT/URL/TIER row with the given prefix.
func projectRow(prefix string, r api.Row, nameW, portW, urlW int) string {
	name := styleName.Render(pad(truncName(r.Name), nameW))
	var port string
	if r.Port != 0 {
		port = stylePort.Render(pad(portToken(r.Port), portW))
	} else {
		port = styleDim.Render(pad("—", portW))
	}
	url := styleURL.Render(pad(urlToken(r), urlW))
	tier := styleTier.Render(r.Tier)
	return prefix + name + "  " + port + "  " + url + "  " + tier
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
	var name string
	var port int
	var force, always bool
	c := &cobra.Command{
		Use:                "run [-- <cmd>...]",
		Short:              "Launch the cwd (auto-registers, detects the port, injects the marker)",
		DisableFlagParsing: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := svc()
			if err != nil {
				return err
			}
			dir, _ := os.Getwd()
			res, err := s.RunAdhoc(dir, args, runner.AdhocOpts{
				LaunchOpts: runner.LaunchOpts{Port: port, Force: force},
				Name:       name,
				Always:     always,
			})
			if err != nil {
				return err
			}
			printResult(res)
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "project name (defaults to the dir)")
	c.Flags().IntVar(&port, "port", 0, "pin the port")
	c.Flags().BoolVar(&force, "force", false, "fail if the port is taken")
	c.Flags().BoolVar(&always, "always", false, "register as a permanent service")
	return c
}

func startCmd() *cobra.Command {
	var port int
	var force bool
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
			res, err := s.Start(args[0], runner.LaunchOpts{Port: port, Force: force})
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
	return c
}

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
