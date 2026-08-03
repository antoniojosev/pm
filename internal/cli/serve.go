package cli

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/antoniojosev/pm/internal/config"
	"github.com/antoniojosev/pm/internal/daemon"
	"github.com/spf13/cobra"
)

func serveCmd() *cobra.Command {
	var port int
	c := &cobra.Command{
		Use:   "serve",
		Short: "Start the daemon (API + web + SSE + MCP)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := svc()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			d := daemon.New(s, port)
			fmt.Printf("pm daemon listening on :%d\n", port)
			return d.Serve(ctx)
		},
	}
	c.Flags().IntVar(&port, "port", config.DefaultDaemonPort, "daemon port")
	return c
}
