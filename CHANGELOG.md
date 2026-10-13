# Changelog

Format based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versioning is semver as it works before 1.0: a breaking change raises the
minor version and is listed here.

## [v0.2.1] - 2026-10-12

Explicit routes (`pm route`, `/api/v1/routes`) and a Go client that covers the
whole API instead of a subset; the design is in
[ADR 002](docs/adr/0002-rutas-explicitas-y-cliente-completo.md). Nothing that
existed changes.

### Added

- **Explicit routes.** `POST /api/v1/routes` publishes `<name>.localhost` for a
  port pm did not start, `GET /api/v1/routes` lists them together with the
  routes derived from running projects (`source: explicit | project`), and
  `DELETE /api/v1/routes/{host}` removes one. An explicit route wins over a
  derived one with the same host, persists in the registry, and goes away with
  its owner (`DELETE /api/v1/owners/{owner}`). CLI: `pm route add|rm|ls`; MCP:
  `pm_route_add`, `pm_route_rm`; the SSE stream emits `route.created`,
  `route.updated` (the same host and port got another owner) and
  `route.deleted`.
- **Complete Go client.** `client` gains `PS(ctx)` (the attributed picture of
  the machine), `Detect(ctx, dir)` and `Routes.Add/Remove/List`, with their
  types duplicated from `openapi.yaml`. The client-to-daemon contract test
  covers the three and `TestTypesMatchTheDaemons` keeps the new types in step.
  The package still imports only the standard library.

### Fixed

- **Caddy reload is retried.** The daemon used to take a failed `caddy reload`
  as done, so a route added while Caddy was down stayed unserved until another
  route changed. A failed write or reload is now logged and tried again on the
  next round (every 3 seconds).
- `pm down --owner --keep-leases` now says in its flag help that it keeps the
  owner's routes as well as its leases.
- `openapi.yaml` documents the 400 of `DELETE /routes/{host}` and says that
  `Row.owner` also comes from the `PM_OWNER` marker the daemon reads from an
  unclaimed process.

## [v0.2.0] - 2026-10-12

pm becomes the process and port layer other tools can build on: a versioned,
documented HTTP API, port leases, starting a process from a specification,
resource limits, owners and an official Go client. Everything in it also has a
use from the CLI and MCP for one person and their agents; the design is in
[ADR 001](docs/adr/0001-api-para-consumidores-externos.md).

### Added

- **API v1.** Every route is served under `/api/v1/`. The contract lives in
  [`docs/api/openapi.yaml`](docs/api/openapi.yaml) and two tests keep it honest:
  `TestRoutesMatchOpenAPI` fails when the route table and the document disagree
  in either direction, and `TestResponsesConformToOpenAPI` checks real daemon
  responses against the documented schemas.
- **`GET /api/v1/version`** answers `{version, api, caddy, docker, systemd}`:
  the pm release, the API generation and what the host offers. `pm doctor`
  shows it and fails when the daemon is unreachable or speaks another API
  generation.
- **Resource limits.** `--mem 2G --cpu 150%` on `pm add`, `pm edit`, `pm run`
  and `pm start` caps memory and CPU through systemd (`MemoryMax=`,
  `CPUQuota=`), and `limits` exists on projects and in the API. A memory cap
  also sets `MemorySwapMax=0`, so a project that passes it is killed by the
  kernel instead of being paged out. A unit that ends that way is reported as
  `failed` with its reason (`oom-kill`) in `pm ps`, the API (`failure`) and the
  dashboard. The dashboard form edits both limits. `pm doctor` warns when
  cgroups v2 or the `memory`/`cpu` controllers are not delegated to your user
  manager.
- **Port leases.** `POST/GET /api/v1/leases`, `DELETE /api/v1/leases/{id}` and
  `pm lease [n] --owner <o>` (with `ls` and `release <id>`) reserve ports for an
  owner before starting anything. A lease marks the port as taken in pm's view;
  it does not open it. Two simultaneous requests never receive the same port.
  Ports are checked with a test bind (`ports.Free`), which also catches
  listeners that `ss` does not show, and pm's own launches steer around leased
  ports.
- **Start by specification.** `POST /api/v1/run` with
  `{name, dir, cmd[], env{}, limits, compose, log, owner, ports[]}` starts a
  process as `pm-<name>.service` without registering it and answers
  `{unit, pid, ports}`. `pm run --name <n> -- <cmd>` does the same from the
  shell (`--owner`, `--ports`, `-e K=V`, `--log`, `--mem/--cpu`). The process
  shows in `pm ps` as an ephemeral row with its owner and `pm stop` forgets it.
  Its ports are reserved while it lives and released when it dies. `pm run`
  without `--name` keeps registering the working directory as before.
- **Named compose stacks.** `compose: {file, project}` on projects and on runs
  (`--compose-project`, `--compose-file`) starts the stack as
  `docker compose -f <file> -p <project>` with `COMPOSE_PROJECT_NAME` set, so
  two copies of one repo get their own containers, networks and volumes.
  Containers are attributed back through their `com.docker.compose.project`
  label.
- **Owners.** `owner` on runs, leases and projects groups everything one
  worktree, script or agent started. `POST /api/v1/owners/{owner}/stop` stops
  it and keeps its leases; `DELETE /api/v1/owners/{owner}` also releases them,
  only when everything stopped. `pm down --owner <o>` (`--keep-leases`) does the
  same from the CLI. pm sets `PM_OWNER` on the units it starts, and a process
  that carries `PM_OWNER=<o>` in its environment is attributed to that owner
  even when pm did not start it.
- **Events.** `/api/v1/events` also emits `lease.created`, `lease.released`,
  `owner.stopped` and `owner.deleted`.
- **MCP.** Two new tools, `pm_run` and `pm_owner_down`, for 15 in total.
- **Go client.** `github.com/antoniojosev/pm/client`, in the same module, MIT,
  standard library only: `Version`, `Leases.Create/List/Delete`, `Run`, `Stop`,
  `Logs` and `Owners.Stop/Delete`. It checks `GET /api/v1/version` before its
  first call and returns `client.ErrIncompatible` when the daemon speaks another
  API generation. It comes with a runnable example and a contract test against
  an in-memory daemon. Nothing under `internal/` is exported.
- **CI and releases.** GitHub Actions run `go vet`, a gofmt check,
  `go test -race` and golangci-lint on pushes to `main` and on every pull
  request; pushing a `v*` tag publishes a goreleaser release for linux amd64
  and arm64.
- **Docs.** README sections on transient services, on what `scan` does versus
  the detector, and **Use pm from your tool**; `client/README.md`; the
  `docs/adr/` directory with ADR 001.

### Changed

- The HTTP API moves from `/api/` to `/api/v1/`. The dashboard and the CLI use
  the new paths.
- The registry store takes a file lock on every write and starts from what is on
  disk, so the CLI and the daemon can no longer overwrite each other's leases or
  runs.
- The build version lives in `internal/version`, shared by the daemon and the
  CLI; the Makefile ldflags follow.
- Documentation and comments stop calling the units "transient scopes": they are
  transient services (`systemd-run` without `--scope`).
- Adds `gopkg.in/yaml.v3` to `go.mod`, used only by the OpenAPI tests.

### Deprecated

- The unversioned `/api/...` alias. It still answers like its `/api/v1/...`
  twin during v0.2, with a `Deprecation` header (RFC 9745) and a
  `Link: <...>; rel="successor-version"` pointing at the new path. Move to
  `/api/v1/`.

### Fixed

- A project killed for exceeding its memory limit could not be started again:
  systemd refused the unit as "already loaded". pm now clears the stale failed
  unit before launching.
- Two simultaneous `run` calls with the same name could both succeed, and the
  loser's rollback deleted the winner's record, leaving a unit alive with no
  owner and no reserved ports. A run now holds its name while it launches and a
  failed launch removes only its own record.
- The ports of a run whose process died stayed reserved until `pm stop`. They
  are now released before a lease or a launch needs them; the run still shows
  in `pm ps` as down or failed.
- `pm rm` of a run no longer swallows the error of stopping it.
- Saving a project from the dashboard replaced the whole record and dropped the
  fields the form does not show (limits, port strategy, stack, kind). It now
  starts from the stored record, and `draft` is cleared as `pm edit` does.
- The integration test fakes resolved their helper tools from an already emptied
  `PATH`, so 24 tests of `pm run` were skipped while `make test` stayed green. A
  missing tool now fails the test.

## [v0.1.0] - 2026-09-28

First tagged release: the project registry, detection of what listens on which
port, start/stop under `systemd --user`, `.localhost` names through Caddy, a web
dashboard, the HTTP API and an MCP server with 13 tools.

[v0.2.1]: https://github.com/antoniojosev/pm/compare/v0.2.0...v0.2.1
[v0.2.0]: https://github.com/antoniojosev/pm/compare/v0.1.0...v0.2.0
[v0.1.0]: https://github.com/antoniojosev/pm/releases/tag/v0.1.0
