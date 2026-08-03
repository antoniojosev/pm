package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/antoniojosev/pm/internal/config"
	"github.com/antoniojosev/pm/internal/registry"
	"github.com/antoniojosev/pm/internal/scan"
	"github.com/spf13/cobra"
)

func addCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "add [dir]",
		Short: "Register a project without starting it (detects the stack)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := svc()
			if err != nil {
				return err
			}
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			abs, _ := os.Getwd()
			if dir != "." {
				abs = dir
			}
			p := projectFromFlags(cmd)
			if p.Dir == "" {
				p.Dir = abs
			}
			if p.Name == "" {
				p.Name = baseName(p.Dir)
			}
			if det, ok := scan.Detect(p.Dir); ok {
				if len(p.Cmd) == 0 {
					p.Cmd = det.Cmd
				}
				if p.PreferPort == 0 {
					p.PreferPort = det.PreferPort
				}
				p.Kind = det.Kind
				p.Stack = det.Stack
				p.PortStrategy = det.PortStrategy
			}
			if err := s.Add(p); err != nil {
				return err
			}
			fmt.Printf("＋ %s registered (%s) → %s\n", p.Name, p.Stack, p.Hostname())
			return nil
		},
	}
	addProjectFlags(c)
	return c
}

func rmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a project from the registry (stops it first)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := svc()
			if err != nil {
				return err
			}
			if err := s.Remove(args[0]); err != nil {
				return err
			}
			fmt.Printf("－ %s removed\n", args[0])
			return nil
		},
	}
}

func editCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "edit <name>",
		Short: "Edit metadata (cmd, port, host, group)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := svc()
			if err != nil {
				return err
			}
			p, ok := s.Get(args[0])
			if !ok {
				return fmt.Errorf("project %q does not exist", args[0])
			}
			if v, _ := cmd.Flags().GetString("cmd"); v != "" {
				p.Cmd = strings.Fields(v)
			}
			if v, _ := cmd.Flags().GetInt("port"); v != 0 {
				p.PreferPort = v
			}
			if v, _ := cmd.Flags().GetString("host"); v != "" {
				p.Host = v
			}
			if v, _ := cmd.Flags().GetString("group"); v != "" {
				p.Group = v
			}
			if v, _ := cmd.Flags().GetString("dir"); v != "" {
				p.Dir = v
			}
			p.Draft = false
			if err := s.Edit(p); err != nil {
				return err
			}
			fmt.Printf("✎ %s updated\n", p.Name)
			return nil
		},
	}
	addProjectFlags(c)
	return c
}

func claimCmd() *cobra.Command {
	var name string
	c := &cobra.Command{
		Use:   "claim <port>",
		Short: "Adopt an unmanaged listener into the registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := svc()
			if err != nil {
				return err
			}
			var port int
			fmt.Sscanf(args[0], "%d", &port)
			if name == "" {
				name = fmt.Sprintf("port-%d", port)
			}
			p, err := s.Claim(port, name)
			if err != nil {
				return err
			}
			fmt.Printf("✚ adopted :%d as %q → %s\n", port, p.Name, p.Hostname())
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "name to assign")
	return c
}

func scanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "scan",
		Short: "Rescan the roots and register new projects (drafts)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := svc()
			if err != nil {
				return err
			}
			added, err := s.Scan()
			if err != nil {
				return err
			}
			if len(added) == 0 {
				fmt.Println("no new projects")
				return nil
			}
			for _, p := range added {
				fmt.Printf("＋ %s (%s)\n", p.Name, p.Stack)
			}
			return nil
		},
	}
}

func logsCmd() *cobra.Command {
	var follow bool
	var n int
	c := &cobra.Command{
		Use:   "logs <name>",
		Short: "Show a project's logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := svc()
			if err != nil {
				return err
			}
			if follow {
				return tailFollow(s.LogPath(args[0]))
			}
			out, err := s.Logs(args[0], n)
			if err != nil {
				return err
			}
			fmt.Println(out)
			return nil
		},
	}
	c.Flags().BoolVarP(&follow, "follow", "f", false, "follow live")
	c.Flags().IntVarP(&n, "lines", "n", 200, "number of lines")
	return c
}

func tailFollow(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	f.Seek(0, io.SeekEnd)
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			fmt.Print(line)
		}
		if err == io.EOF {
			continue
		}
		if err != nil {
			return err
		}
	}
}

func openCmd() *cobra.Command {
	var useHost bool
	c := &cobra.Command{
		Use:   "open <name>",
		Short: "Open the project in the browser (direct port; --host for .localhost)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := svc()
			if err != nil {
				return err
			}
			if useHost {
				p, ok := s.Get(args[0])
				if !ok {
					return fmt.Errorf("project %q does not exist", args[0])
				}
				return openBrowser("http://" + p.Hostname())
			}
			url, err := s.BestURL(args[0])
			if err != nil {
				return err
			}
			return openBrowser(url)
		},
	}
	c.Flags().BoolVar(&useHost, "host", false, "open via <name>.localhost (requires Caddy) instead of the direct port")
	return c
}

func uiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ui",
		Short: "Open the web dashboard",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return openBrowser("http://pm" + config.DNSSuffix)
		},
	}
}

// openBrowser launches the Windows browser from WSL, falling back to xdg-open.
func openBrowser(url string) error {
	candidates := [][]string{
		{"wslview", url},
		{"cmd.exe", "/c", "start", url},
		{"powershell.exe", "-NoProfile", "Start-Process", url},
		{"xdg-open", url},
	}
	for _, c := range candidates {
		if _, err := exec.LookPath(c[0]); err == nil {
			if err := exec.Command(c[0], c[1:]...).Start(); err == nil {
				fmt.Println("↗ " + url)
				return nil
			}
		}
	}
	fmt.Println("Open manually: " + url)
	return nil
}

func groupCmd() *cobra.Command {
	c := &cobra.Command{Use: "group", Short: "Manage groups"}
	c.AddCommand(
		&cobra.Command{
			Use:   "set <group> <proj...>",
			Short: "Create/update a group with the given project list",
			Args:  cobra.MinimumNArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				s, err := svc()
				if err != nil {
					return err
				}
				g := registry.Group{Name: args[0], Projects: args[1:]}
				if err := s.SetGroup(g); err != nil {
					return err
				}
				fmt.Printf("⛿ group %s = %s\n", g.Name, strings.Join(g.Projects, ", "))
				return nil
			},
		},
		&cobra.Command{
			Use:   "rm <group>",
			Short: "Remove a group",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				s, err := svc()
				if err != nil {
					return err
				}
				if err := s.RemoveGroup(args[0]); err != nil {
					return err
				}
				fmt.Printf("－ group %s removed\n", args[0])
				return nil
			},
		},
		&cobra.Command{
			Use:   "ls",
			Short: "List groups",
			RunE: func(cmd *cobra.Command, _ []string) error {
				s, err := svc()
				if err != nil {
					return err
				}
				for _, g := range s.Groups() {
					fmt.Printf("%s: %s\n", g.Name, strings.Join(g.Projects, ", "))
				}
				return nil
			},
		},
	)
	return c
}

func addProjectFlags(c *cobra.Command) {
	c.Flags().String("name", "", "name")
	c.Flags().String("dir", "", "directory")
	c.Flags().Int("port", 0, "preferred port")
	c.Flags().String("cmd", "", "start command")
	c.Flags().String("host", "", ".localhost host (defaults to = name)")
	c.Flags().String("group", "", "group")
}

func baseName(dir string) string {
	dir = strings.TrimRight(dir, "/")
	if i := strings.LastIndex(dir, "/"); i >= 0 {
		return dir[i+1:]
	}
	return dir
}
