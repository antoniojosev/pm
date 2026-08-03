# pm — local project & port manager

A single (Go) binary that knows **what runs on which port**, lets you **start/stop** projects from any stack, gives them a **name in the browser** (`<project>.localhost`), and exposes everything over **CLI**, **web dashboard**, and an **MCP server** for MCP clients.

Built for WSL2 + systemd + Chrome.

## Core idea

- **Launch anything with a prefix**: `pm run -- npm run dev`. pm puts it inside a *systemd scope* (`pm-<project>.service`) with the `PM_PROJECT` env var, so it can **attribute** the process and **kill its whole tree** when you stop it.
- **Detects the real system state** with `ss` + `/proc` (cwd, cgroup, cmdline) + `docker ps`. It even sees what you started outside of pm (it shows up as *unmanaged* and you adopt it).
- **Discovers the port on its own**: it looks at what the cgroup binds; no need to declare it.
- **Collision → next port**: if the preferred port is taken, it reassigns to the next free one and injects the port according to the stack (`--port` in Vite, `-p` in Next, `PORT=` env, `0.0.0.0:{port}` in Django…).
- **`.localhost` names**: Caddy on `:80` routes `myapp.localhost` → the real live port. The config regenerates itself from state.
- **Tiers**: `ephemeral` (transient scope) ↔ `service` (user unit, starts at boot). Toggle with `promote`/`demote`.

## Install

```bash
./scripts/install.sh        # builds to ~/.local/bin/pm
pm setup                    # prepares ~/.pm, Caddyfile and systemd units
pm doctor                   # checks the environment
# install Caddy if missing:  sudo apt install -y caddy
systemctl --user enable --now pm-daemon pm-caddy
```

From **Windows**: copy `scripts/pm.cmd` into a folder on your PATH → `pm ps`, `pm ui`, etc. forward to WSL.

## CLI

```
pm run -- <cmd>        launches the cwd (auto-registers, detects port, marks it)
pm ps                  list running projects (add -a to include stopped)
pm start|stop|restart <name>
pm promote|demote <name>       ephemeral ↔ permanent service
pm up|down <group>             bring a group up/down
pm add|edit|rm <name>          registry CRUD
pm claim <port>                adopt an 'unmanaged' listener
pm scan                        discover projects under the roots (PM_ROOTS)
pm listen                      ports on the machine that pm doesn't manage
pm logs <name> [-f]            logs (stream with -f)
pm open <name> | pm ui         open in the browser
pm serve                       start the daemon (API + web + SSE + MCP)
pm doctor | pm setup
```

## Web

`http://pm.localhost` (or `pm ui`). Live dashboard (SSE): status, start/stop, promote/demote, streaming logs, adopt unmanaged listeners, groups and full CRUD.

## MCP

The daemon exposes MCP over HTTP at `/mcp`. Add it to your `.mcp.json`:

```json
{ "mcpServers": { "pm": { "type": "http", "url": "http://127.0.0.1:9797/mcp" } } }
```

Tools (mirror of the CLI): `pm_list`, `pm_start`, `pm_stop`, `pm_restart`, `pm_promote`, `pm_demote`, `pm_register`, `pm_remove`, `pm_claim`, `pm_scan`, `pm_logs`, `pm_group_up`, `pm_group_down`.

## Config

- `PM_HOME` — runtime root (defaults to `~/.pm`): registry, logs, Caddyfile.
- `PM_ROOTS` — folders to scan, separated by `:` (defaults to `~/projects`).
- `PM_DAEMON_URL` — daemon URL (defaults to `http://127.0.0.1:9797`).

## Architecture

```
cmd/pm            entrypoint
internal/
  config          paths and constants
  registry        persistent catalog (JSON)
  detector        real state: ss + /proc + docker
  scan            stack detection / auto-registration
  runner          systemd scope/service, ports, collision, tiers, groups
  proxy           generates Caddyfile + reload
  daemon          HTTP API + SSE + embedded web + proxy reconciliation
  api             shared service layer (CLI + daemon + MCP)
  mcp             MCP server (streamable HTTP)
  cli             cobra commands
  webui           embedded SPA (go:embed)
```

One core, four faces: CLI, Web, HTTP API and MCP all share `internal/api`.
