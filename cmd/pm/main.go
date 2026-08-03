// Command pm is a local project & port manager: it launches projects through
// systemd, detects what's running, routes <name>.localhost via Caddy, and
// exposes everything over a CLI, a web dashboard and an MCP server.
package main

import (
	"os"

	"github.com/antoniojosev/pm/internal/cli"
)

func main() {
	// Execute prints friendly errors itself; we only need the exit code.
	if err := cli.Execute(); err != nil {
		os.Exit(1)
	}
}
