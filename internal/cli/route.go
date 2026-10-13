package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/antoniojosev/pm/internal/api"
	"github.com/antoniojosev/pm/internal/proxy"
	"github.com/antoniojosev/pm/internal/registry"
	"github.com/spf13/cobra"
)

// routeCmd manages explicit routes: a name for a port pm did not start. Like
// lease it works on the registry directly; the daemon, when it runs, picks the
// change up on its next proxy round (within a few seconds).
func routeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "route",
		Short: "Publish a <name>.localhost for a port pm did not start",
		Long: `Routes <name>.localhost to a port on 127.0.0.1 through Caddy, for something pm
does not run itself: a tunnel, a container, a service on another tool's daemon.
The route is saved in the registry and the daemon applies it with the routes it
derives from your projects. When both name the same host, the explicit one wins.

Examples:
  pm route add crb-144.localhost 9810
  pm route add crb-144.localhost 9810 --owner wt-qa
  pm route ls
  pm route rm crb-144.localhost`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	c.AddCommand(routeAddCmd(), routeRmCmd(), routeLsCmd())
	return c
}

func routeAddCmd() *cobra.Command {
	var owner string
	var asJSON bool
	c := &cobra.Command{
		Use:   "add <host> <port>",
		Short: "Route <host> (<name>.localhost) to a port",
		Long: `Routes <host> to 127.0.0.1:<port>. Repeating the command with the same port is
fine; a host that already routes to another port is an error: remove it first.
With --owner, 'pm down --owner' removes the route along with the owner's other
things.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			port, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("invalid port %q: use a number from 1 to 65535", args[1])
			}
			s, err := svc()
			if err != nil {
				return err
			}
			route, err := s.AddRoute(registry.Route{Host: args[0], Port: port, Owner: owner})
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSONLine(os.Stdout, route)
			}
			fmt.Printf("%s route %s %s %s\n", styleOK.Render(glyphOK), styleName.Render(route.Host), styleDim.Render("→"), stylePort.Render(portToken(route.Port)))
			if !proxy.Available() {
				fmt.Println("  " + styleDim.Render("caddy is not installed: the route is saved and applies once it is (pm setup)"))
			}
			return nil
		},
	}
	c.Flags().StringVar(&owner, "owner", "", "who the route is for, e.g. a worktree or script name; deleting the owner removes it")
	c.Flags().BoolVar(&asJSON, "json", false, "print the route as JSON")
	return c
}

func routeRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <host>",
		Aliases: []string{"remove"},
		Short:   "Remove an explicit route",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := svc()
			if err != nil {
				return err
			}
			r, err := s.RemoveRoute(args[0])
			if err != nil {
				return err
			}
			fmt.Printf("%s route %s removed\n", styleDim.Render("■"), styleName.Render(r.Host))
			return nil
		},
	}
}

func routeLsCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List the routes Caddy serves: explicit ones and those derived from projects",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := svc()
			if err != nil {
				return err
			}
			routes, err := s.Routes()
			if err != nil {
				return err
			}
			if routes == nil {
				routes = []api.RouteInfo{}
			}
			if asJSON {
				return writeJSONLine(os.Stdout, routes)
			}
			renderRoutes(os.Stdout, routes)
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print the routes as JSON")
	return c
}

// renderRoutes prints a HOST/PORT/OWNER/SOURCE table.
func renderRoutes(w io.Writer, routes []api.RouteInfo) {
	fmt.Fprintln(w)
	if len(routes) == 0 {
		fmt.Fprintln(w, "  "+styleBrand.Render("pm route")+"   "+styleSummary.Render("no routes"))
		fmt.Fprintln(w)
		return
	}
	noun := "routes"
	if len(routes) == 1 {
		noun = "route"
	}
	fmt.Fprintln(w, "  "+styleBrand.Render("pm route")+"   "+styleSummary.Render(fmt.Sprintf("%d %s", len(routes), noun)))
	fmt.Fprintln(w)
	hostW, portW, ownerW := len("HOST"), len("PORT"), len("OWNER")
	for _, r := range routes {
		hostW = maxi(hostW, len(r.Host))
		portW = maxi(portW, len(portToken(r.Port)))
		ownerW = maxi(ownerW, len(ownerCell(r.Owner)))
	}
	fmt.Fprintln(w, "    "+styleDim.Render(pad("HOST", hostW)+"  "+pad("PORT", portW)+"  "+pad("OWNER", ownerW)+"  SOURCE"))
	for _, r := range routes {
		fmt.Fprintln(w, "    "+styleName.Render(pad(r.Host, hostW))+"  "+stylePort.Render(pad(portToken(r.Port), portW))+"  "+pad(ownerCell(r.Owner), ownerW)+"  "+styleDim.Render(r.Source))
	}
	fmt.Fprintln(w)
}

func ownerCell(owner string) string {
	if owner == "" {
		return "-"
	}
	return owner
}
