package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/antoniojosev/pm/internal/api"
	"github.com/antoniojosev/pm/internal/registry"
	"github.com/spf13/cobra"
)

// leaseCmd reserves ports for an owner before anything is started. It works on
// the registry directly, like the rest of the CLI: the registry is locked
// across processes, so it never hands out a port a running daemon has leased.
func leaseCmd() *cobra.Command {
	var owner string
	var prefer int
	var asJSON bool
	c := &cobra.Command{
		Use:   "lease [count] --owner <owner>",
		Short: "Reserve ports for an owner before starting anything",
		Long: `Reserves ports so nothing else receives them: not another lease, and not a project
pm starts on its own. The lease does not open the port; your process binds it
later. Release it with 'pm lease release <id>'.

Examples:
  pm lease --owner wt-qa            reserve one port
  pm lease 3 --owner wt-qa          reserve three
  pm lease 2 --owner wt-qa --prefer 4100
  pm lease ls                       list leases
  pm lease release l-3f9a1c0b7d2e`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			count := 1
			if len(args) == 1 {
				n, err := strconv.Atoi(args[0])
				if err != nil {
					return fmt.Errorf("invalid count %q: use a number of ports", args[0])
				}
				count = n
			}
			s, err := svc()
			if err != nil {
				return err
			}
			lease, err := s.CreateLease(api.LeaseRequest{Owner: owner, Count: count, Prefer: prefer})
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSONLine(os.Stdout, lease)
			}
			printLease(os.Stdout, lease)
			return nil
		},
	}
	c.Flags().StringVar(&owner, "owner", "", "who the ports are for, e.g. a worktree or script name (required)")
	c.Flags().IntVar(&prefer, "prefer", 0, "first port to try (default 20000); busy ports are skipped")
	c.Flags().BoolVar(&asJSON, "json", false, "print the lease as JSON")
	_ = c.MarkFlagRequired("owner")
	// "pm lease ls" and "pm lease release" must not demand --owner.
	c.AddCommand(leaseLsCmd(), leaseReleaseCmd())
	return c
}

func leaseLsCmd() *cobra.Command {
	var owner string
	var asJSON bool
	c := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List port leases",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := svc()
			if err != nil {
				return err
			}
			leases := s.Leases(owner)
			if asJSON {
				return writeJSONLine(os.Stdout, leases)
			}
			renderLeases(os.Stdout, leases)
			return nil
		},
	}
	c.Flags().StringVar(&owner, "owner", "", "only the leases of this owner")
	c.Flags().BoolVar(&asJSON, "json", false, "print the leases as JSON")
	return c
}

func leaseReleaseCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "release <id>",
		Aliases: []string{"rm"},
		Short:   "Release a lease and free its ports",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := svc()
			if err != nil {
				return err
			}
			l, err := s.ReleaseLease(args[0])
			if err != nil {
				return err
			}
			fmt.Printf("%s lease %s released (%s)\n", styleDim.Render("■"), styleName.Render(l.ID), portList(l.Ports))
			return nil
		},
	}
}

func writeJSONLine(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func portList(ports []int) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = portToken(p)
	}
	return strings.Join(parts, " ")
}

func printLease(w io.Writer, l registry.Lease) {
	fmt.Fprintf(w, "%s lease %s for %s\n", styleOK.Render(glyphOK), styleName.Render(l.ID), l.Owner)
	fmt.Fprintf(w, "  %s\n", stylePort.Render(portList(l.Ports)))
}

// renderLeases prints an ID/OWNER/PORTS/AGE table.
func renderLeases(w io.Writer, leases []registry.Lease) {
	fmt.Fprintln(w)
	if len(leases) == 0 {
		fmt.Fprintln(w, "  "+styleBrand.Render("pm lease")+"   "+styleSummary.Render("no leases"))
		fmt.Fprintln(w)
		return
	}
	noun := "leases"
	if len(leases) == 1 {
		noun = "lease"
	}
	fmt.Fprintln(w, "  "+styleBrand.Render("pm lease")+"   "+styleSummary.Render(fmt.Sprintf("%d %s", len(leases), noun)))
	fmt.Fprintln(w)
	idW, ownerW, portsW := len("ID"), len("OWNER"), len("PORTS")
	for _, l := range leases {
		idW = maxi(idW, len(l.ID))
		ownerW = maxi(ownerW, len(l.Owner))
		portsW = maxi(portsW, len(portList(l.Ports)))
	}
	fmt.Fprintln(w, "    "+styleDim.Render(pad("ID", idW)+"  "+pad("OWNER", ownerW)+"  "+pad("PORTS", portsW)+"  AGE"))
	for _, l := range leases {
		fmt.Fprintln(w, "    "+pad(l.ID, idW)+"  "+styleName.Render(pad(l.Owner, ownerW))+"  "+stylePort.Render(pad(portList(l.Ports), portsW))+"  "+styleDim.Render(age(l.CreatedAt)))
	}
	fmt.Fprintln(w)
}

// age renders how long ago t was, coarsely: 40s, 12m, 3h, 2d.
func age(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(int(d.Seconds()), 0))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
