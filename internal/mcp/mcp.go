// Package mcp exposes pm's operations to MCP clients over MCP (streamable HTTP).
// Every tool mirrors a CLI verb and shares the same service layer.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/antoniojosev/pm/internal/api"
	"github.com/antoniojosev/pm/internal/registry"
	"github.com/antoniojosev/pm/internal/runner"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Handler builds an MCP streamable-HTTP handler backed by the service.
func Handler(svc *api.Service) http.Handler {
	s := server.NewMCPServer("pm", "0.1.0",
		server.WithToolCapabilities(true),
		server.WithInstructions("pm manages local projects and their ports: list what's running, start/stop, run a process by specification, promote to a permanent service, adopt ports, view logs and groups."),
	)
	register(s, svc)
	return server.NewStreamableHTTPServer(s)
}

func register(s *server.MCPServer, svc *api.Service) {
	s.AddTool(mcp.NewTool("pm_list",
		mcp.WithDescription("List projects and what's running now (name, port, url, tier, status). Includes unmanaged listeners.")),
		func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			rows, err := svc.PS()
			return jsonResult(rows, err)
		})

	s.AddTool(mcp.NewTool("pm_start",
		mcp.WithDescription("Start a registered project. Optional: port to pin it."),
		mcp.WithString("name", mcp.Required(), mcp.Description("project name")),
		mcp.WithNumber("port", mcp.Description("port to pin (0 = auto)"))),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			res, err := svc.Start(req.GetString("name", ""), runner.LaunchOpts{Port: req.GetInt("port", 0)})
			return jsonResult(res, err)
		})

	s.AddTool(mcp.NewTool("pm_run",
		mcp.WithDescription("Start a process from a specification without registering it. It runs as pm-<name>.service, shows in pm_list as an ephemeral row with its owner while it lives, and pm_stop stops it and forgets it. Returns {unit, pid, ports}."),
		mcp.WithString("name", mcp.Required(), mcp.Description("names the unit and what pm_stop takes: letters, digits, '.', '_' and '-'")),
		mcp.WithString("dir", mcp.Required(), mcp.Description("working directory: an absolute path to an existing directory")),
		mcp.WithArray("cmd", mcp.Required(), mcp.WithStringItems(), mcp.Description("the program and its arguments, as an argv (no shell)")),
		mcp.WithObject("env", mcp.AdditionalProperties(map[string]any{"type": "string"}), mcp.Description("environment variables to add (PM_PROJECT and PM_OWNER are reserved); never stored")),
		mcp.WithObject("limits",
			mcp.Properties(map[string]any{
				"memory": map[string]any{"type": "string", "description": "MemoryMax, e.g. 512M, 2G or 50%"},
				"cpu":    map[string]any{"type": "string", "description": "CPUQuota as a percentage of one CPU, e.g. 150%"},
			}),
			mcp.Description("memory and CPU caps (needs cgroups v2)")),
		mcp.WithObject("compose",
			mcp.Properties(map[string]any{
				"project": map[string]any{"type": "string", "description": "compose project name: lowercase letters, digits, '_' and '-'"},
				"file":    map[string]any{"type": "string", "description": "compose file, absolute or relative to dir (optional)"},
			}),
			mcp.Description("name the docker compose stack: pm sets COMPOSE_PROJECT_NAME and, for `docker compose ...` in cmd, adds -p and -f, so two runs of one compose file do not collide. Limits do not apply")),
		mcp.WithString("log", mcp.Description("absolute path of the log file (default ~/.pm/logs/<name>.log)")),
		mcp.WithString("owner", mcp.Description("who the process is for, e.g. a worktree or script name (default \"local\")")),
		mcp.WithArray("ports", mcp.WithNumberItems(), mcp.Description("ports the process will use, e.g. from a lease; checked free and kept from other leases. pm does not pass them to the process: put them in env or cmd"))),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			spec, err := decodeRunSpec(req)
			if err != nil {
				return jsonResult(nil, err)
			}
			res, err := svc.StartRun(spec)
			return jsonResult(res, err)
		})

	s.AddTool(mcp.NewTool("pm_stop",
		mcp.WithDescription("Stop a project (kills its whole process tree)."),
		mcp.WithString("name", mcp.Required(), mcp.Description("project name"))),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			err := svc.Stop(req.GetString("name", ""))
			return textResult("stopped", err)
		})

	s.AddTool(mcp.NewTool("pm_owner_down",
		mcp.WithDescription("Bring down everything that belongs to an owner (a worktree, a script, an agent): its projects, the processes it started with pm_run (they are forgotten once stopped) and any process that carries PM_OWNER=<owner>. Also releases the owner's port leases and deletes its routes unless keep_leases is true. Returns {owner, stopped[], leases_released[]} (the routes deleted are not listed); an owner with nothing is not an error. If something cannot be stopped the rest still is, and the leases and routes are kept."),
		mcp.WithString("owner", mcp.Required(), mcp.Description("the owner name, as given to pm_run or a lease")),
		mcp.WithBoolean("keep_leases", mcp.Description("stop the processes but keep the port leases and routes (default false)"))),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			owner := req.GetString("owner", "")
			var res api.OwnerResult
			var err error
			if req.GetBool("keep_leases", false) {
				res, err = svc.StopOwner(owner)
			} else {
				res, err = svc.DeleteOwner(owner)
			}
			if err != nil {
				return mcp.NewToolResultError(ownerErrorText(res, err)), nil
			}
			return jsonResult(res, nil)
		})

	s.AddTool(mcp.NewTool("pm_route_add",
		mcp.WithDescription("Publish <host>.localhost for a port pm did not start, so Caddy sends it to 127.0.0.1:<port> (a tunnel, a container, a service pm does not manage). It is saved even without Caddy. An explicit route wins over the one derived from a project with the same host. Repeating the call with the same port is fine; a host already routed to another port is an error: pm_route_rm it first. Returns {host, port, owner}."),
		mcp.WithString("host", mcp.Required(), mcp.Description("the name, <name>.localhost (lowercase letters, digits and '-' per label); pm.localhost is the dashboard's")),
		mcp.WithNumber("port", mcp.Required(), mcp.Description("the port it answers on, 1 to 65535")),
		mcp.WithString("owner", mcp.Description("who the route is for (optional); deleting the owner (pm_owner_down) removes the route"))),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			route, err := svc.AddRoute(registry.Route{
				Host:  req.GetString("host", ""),
				Port:  req.GetInt("port", 0),
				Owner: req.GetString("owner", ""),
			})
			return jsonResult(route, err)
		})

	s.AddTool(mcp.NewTool("pm_route_rm",
		mcp.WithDescription("Remove an explicit route added with pm_route_add. A host that is only a project's own name is not one: it goes away when the project stops."),
		mcp.WithString("host", mcp.Required(), mcp.Description("the route's host, <name>.localhost"))),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			host := req.GetString("host", "")
			_, err := svc.RemoveRoute(host)
			return textResult("removed "+host, err)
		})

	s.AddTool(mcp.NewTool("pm_restart",
		mcp.WithDescription("Restart a project."),
		mcp.WithString("name", mcp.Required())),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			res, err := svc.Restart(req.GetString("name", ""), runner.LaunchOpts{})
			return jsonResult(res, err)
		})

	s.AddTool(mcp.NewTool("pm_promote",
		mcp.WithDescription("Convert a project into a permanent service (starts at boot)."),
		mcp.WithString("name", mcp.Required())),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			err := svc.Promote(req.GetString("name", ""))
			return textResult("promoted", err)
		})

	s.AddTool(mcp.NewTool("pm_demote",
		mcp.WithDescription("Revert a project to ephemeral (on-demand)."),
		mcp.WithString("name", mcp.Required())),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			err := svc.Demote(req.GetString("name", ""))
			return textResult("demoted", err)
		})

	s.AddTool(mcp.NewTool("pm_register",
		mcp.WithDescription("Register a new project (without starting it)."),
		mcp.WithString("name", mcp.Required()),
		mcp.WithString("dir", mcp.Description("root directory (optional: omit it to register by name only, e.g. services on another WSL instance)")),
		mcp.WithString("cmd", mcp.Description("start command (space-separated)")),
		mcp.WithNumber("port", mcp.Description("preferred port"))),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			p := registry.Project{
				Name:       req.GetString("name", ""),
				Dir:        req.GetString("dir", ""),
				PreferPort: req.GetInt("port", 0),
				Tier:       registry.TierEphemeral,
				Kind:       registry.KindNative,
			}
			if c := req.GetString("cmd", ""); c != "" {
				p.Cmd = splitFields(c)
			}
			err := svc.Add(p)
			return jsonResult(p, err)
		})

	s.AddTool(mcp.NewTool("pm_remove",
		mcp.WithDescription("Remove a project from the registry."),
		mcp.WithString("name", mcp.Required())),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			err := svc.Remove(req.GetString("name", ""))
			return textResult("removed", err)
		})

	s.AddTool(mcp.NewTool("pm_claim",
		mcp.WithDescription("Adopt an unmanaged listener into the registry by port."),
		mcp.WithNumber("port", mcp.Required()),
		mcp.WithString("name", mcp.Required())),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			p, err := svc.Claim(req.GetInt("port", 0), req.GetString("name", ""))
			return jsonResult(p, err)
		})

	s.AddTool(mcp.NewTool("pm_scan",
		mcp.WithDescription("Rescan the roots and register new projects.")),
		func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			added, err := svc.Scan()
			return jsonResult(added, err)
		})

	s.AddTool(mcp.NewTool("pm_logs",
		mcp.WithDescription("Return the last lines of a project's log."),
		mcp.WithString("name", mcp.Required()),
		mcp.WithNumber("lines", mcp.Description("number of lines (default 200)"))),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			out, err := svc.Logs(req.GetString("name", ""), req.GetInt("lines", 200))
			return textResult(out, err)
		})

	s.AddTool(mcp.NewTool("pm_group_up",
		mcp.WithDescription("Bring up every project in a group."),
		mcp.WithString("group", mcp.Required())),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			res, err := svc.Up(req.GetString("group", ""))
			return jsonResult(res, err)
		})

	s.AddTool(mcp.NewTool("pm_group_down",
		mcp.WithDescription("Bring down every project in a group."),
		mcp.WithString("group", mcp.Required())),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			err := svc.Down(req.GetString("group", ""))
			return textResult("down", err)
		})
}

// decodeRunSpec reads the tool arguments as a RunSpec, the same document
// POST /api/v1/run takes. Unknown fields are an error, as there: a misspelt
// "limits" must not silently run uncapped.
func decodeRunSpec(req mcp.CallToolRequest) (runner.RunSpec, error) {
	var spec runner.RunSpec
	raw, err := json.Marshal(req.GetRawArguments())
	if err != nil {
		return spec, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return spec, fmt.Errorf("invalid arguments: %w", err)
	}
	return spec, nil
}

// ownerErrorText is the error of a partial owner stop: the message plus what
// was stopped before the failure, which the caller still needs.
func ownerErrorText(res api.OwnerResult, err error) string {
	b, _ := json.MarshalIndent(res, "", "  ")
	return err.Error() + "\n" + string(b)
}

func jsonResult(v any, err error) (*mcp.CallToolResult, error) {
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return mcp.NewToolResultText(string(b)), nil
}

func textResult(msg string, err error) (*mcp.CallToolResult, error) {
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(msg), nil
}

func splitFields(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ' ' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
