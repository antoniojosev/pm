package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/antoniojosev/pm/internal/api"
	"github.com/antoniojosev/pm/internal/registry"
	"github.com/antoniojosev/pm/internal/runner"
)

// APIPrefix is the versioned base path of the HTTP API.
const APIPrefix = "/api/v1"

// legacyPrefix is the unversioned alias kept during v0.2. Every route under it
// answers like its /api/v1 twin plus a Deprecation header.
const legacyPrefix = "/api"

// Route is one entry of the API table. Path is relative to the API prefix
// (e.g. "/projects/{name}/start"). docs/api/openapi.yaml must describe every
// entry here and nothing else; TestRoutesMatchOpenAPI enforces both ways.
type Route struct {
	Method  string
	Path    string
	Handler http.HandlerFunc
}

// apiRoutes is the single source of truth for the HTTP API.
func (d *Daemon) apiRoutes() []Route {
	return []Route{
		{"GET", "/ps", d.hPS},
		{"GET", "/projects", d.hProjects},
		{"POST", "/projects", d.hUpsertProject},
		{"DELETE", "/projects/{name}", d.hDeleteProject},
		{"POST", "/run", d.hRun},
		{"POST", "/projects/{name}/start", d.hStart},
		{"POST", "/projects/{name}/stop", d.hStop},
		{"POST", "/projects/{name}/restart", d.hRestart},
		{"POST", "/projects/{name}/promote", d.hPromote},
		{"POST", "/projects/{name}/demote", d.hDemote},
		{"GET", "/projects/{name}/logs", d.hLogs},
		{"POST", "/claim", d.hClaim},
		{"POST", "/scan", d.hScan},
		{"GET", "/groups", d.hGroups},
		{"POST", "/groups", d.hUpsertGroup},
		{"DELETE", "/groups/{name}", d.hDeleteGroup},
		{"POST", "/groups/{name}/up", d.hGroupUp},
		{"POST", "/groups/{name}/down", d.hGroupDown},
		{"POST", "/leases", d.hCreateLease},
		{"GET", "/leases", d.hLeases},
		{"DELETE", "/leases/{id}", d.hReleaseLease},
		{"POST", "/owners/{owner}/stop", d.hOwnerStop},
		{"DELETE", "/owners/{owner}", d.hOwnerDelete},
		{"POST", "/routes", d.hAddRoute},
		{"GET", "/routes", d.hRoutes},
		{"DELETE", "/routes/{host}", d.hRemoveRoute},
		{"GET", "/events", d.hEvents},
		{"GET", "/logs/stream", d.hLogStream},
		{"POST", "/instances/{port}/stop", d.hInstanceStop},
		{"POST", "/instances/{port}/restart", d.hInstanceRestart},
		{"GET", "/fs", d.hBrowse},
		{"GET", "/detect", d.hDetect},
		{"GET", "/health", d.hHealth},
		{"GET", "/version", d.hVersion},
	}
}

// router is the part of *http.ServeMux that routes() needs. Taking an
// interface lets a test record every pattern that gets registered.
type router interface {
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// routes mounts every API route under /api/v1 and, as a deprecated alias,
// under /api. It is the only place that registers API patterns.
func (d *Daemon) routes(mux router) {
	for _, rt := range d.apiRoutes() {
		mux.HandleFunc(rt.Method+" "+APIPrefix+rt.Path, rt.Handler)
		mux.HandleFunc(rt.Method+" "+legacyPrefix+rt.Path, deprecated(rt.Handler))
	}
}

// deprecationDate is the Deprecation header value: an RFC 9745 Structured Field
// Date (`@<unix seconds>`), here 2026-10-12 00:00:00 UTC, the day ADR 0001
// deprecated the unversioned /api alias.
const deprecationDate = "@1791763200"

// deprecated marks a response from the unversioned alias with the Deprecation
// header (RFC 9745) plus a successor-version link to the same request under
// /api/v1.
func deprecated(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		successor := APIPrefix + strings.TrimPrefix(r.URL.EscapedPath(), legacyPrefix)
		if r.URL.RawQuery != "" {
			successor += "?" + r.URL.RawQuery
		}
		w.Header().Set("Deprecation", deprecationDate)
		w.Header().Add("Link", "<"+successor+">; rel=\"successor-version\"")
		h(w, r)
	}
}

func (d *Daemon) hHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (d *Daemon) hVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, d.svc.Version())
}

func (d *Daemon) hInstanceStop(w http.ResponseWriter, r *http.Request) {
	port, _ := strconv.Atoi(r.PathValue("port"))
	if err := d.svc.StopInstance(port); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, ok())
}

func (d *Daemon) hInstanceRestart(w http.ResponseWriter, r *http.Request) {
	port, _ := strconv.Atoi(r.PathValue("port"))
	res, err := d.svc.RestartInstance(port)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func (d *Daemon) hBrowse(w http.ResponseWriter, r *http.Request) {
	listing, err := d.svc.Browse(r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, listing)
}

func (d *Daemon) hDetect(w http.ResponseWriter, r *http.Request) {
	det, ok := d.svc.DetectPath(r.URL.Query().Get("path"))
	writeJSON(w, 200, map[string]any{
		"ok":          ok,
		"stack":       det.Stack,
		"cmd":         det.Cmd,
		"prefer_port": det.PreferPort,
		"kind":        det.Kind,
	})
}

func (d *Daemon) hPS(w http.ResponseWriter, _ *http.Request) {
	rows, err := d.svc.PS()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, rows)
}

func (d *Daemon) hProjects(w http.ResponseWriter, _ *http.Request) {
	_ = d.svc.Store.Reload()
	writeJSON(w, 200, d.svc.List())
}

func (d *Daemon) hUpsertProject(w http.ResponseWriter, r *http.Request) {
	var p registry.Project
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, err)
		return
	}
	if err := d.svc.Add(p); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, p)
}

func (d *Daemon) hDeleteProject(w http.ResponseWriter, r *http.Request) {
	if err := d.svc.Remove(r.PathValue("name")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, ok())
}

// hRun starts a process from a specification, without registering it. Unknown
// fields are an error: a misspelt `limits` must not silently run uncapped.
func (d *Daemon) hRun(w http.ResponseWriter, r *http.Request) {
	var spec runner.RunSpec
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		writeErr(w, err)
		return
	}
	res, err := d.svc.StartRun(spec)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func (d *Daemon) hStart(w http.ResponseWriter, r *http.Request) {
	res, err := d.svc.Start(r.PathValue("name"), launchOpts(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func (d *Daemon) hStop(w http.ResponseWriter, r *http.Request) {
	if err := d.svc.Stop(r.PathValue("name")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, ok())
}

func (d *Daemon) hRestart(w http.ResponseWriter, r *http.Request) {
	res, err := d.svc.Restart(r.PathValue("name"), launchOpts(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func (d *Daemon) hPromote(w http.ResponseWriter, r *http.Request) {
	if err := d.svc.Promote(r.PathValue("name")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, ok())
}

func (d *Daemon) hDemote(w http.ResponseWriter, r *http.Request) {
	if err := d.svc.Demote(r.PathValue("name")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, ok())
}

func (d *Daemon) hLogs(w http.ResponseWriter, r *http.Request) {
	n := 200
	if v := r.URL.Query().Get("n"); v != "" {
		if x, err := strconv.Atoi(v); err == nil {
			n = x
		}
	}
	out, err := d.svc.Logs(r.PathValue("name"), n)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"logs": out})
}

func (d *Daemon) hClaim(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Port int    `json:"port"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, err)
		return
	}
	p, err := d.svc.Claim(body.Port, body.Name)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, p)
}

func (d *Daemon) hScan(w http.ResponseWriter, _ *http.Request) {
	added, err := d.svc.Scan()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, added)
}

func (d *Daemon) hGroups(w http.ResponseWriter, _ *http.Request) {
	_ = d.svc.Store.Reload()
	writeJSON(w, 200, d.svc.Groups())
}

func (d *Daemon) hUpsertGroup(w http.ResponseWriter, r *http.Request) {
	var g registry.Group
	if err := json.NewDecoder(r.Body).Decode(&g); err != nil {
		writeErr(w, err)
		return
	}
	if err := d.svc.SetGroup(g); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, g)
}

func (d *Daemon) hDeleteGroup(w http.ResponseWriter, r *http.Request) {
	if err := d.svc.RemoveGroup(r.PathValue("name")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, ok())
}

func (d *Daemon) hGroupUp(w http.ResponseWriter, r *http.Request) {
	res, err := d.svc.Up(r.PathValue("name"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func (d *Daemon) hGroupDown(w http.ResponseWriter, r *http.Request) {
	if err := d.svc.Down(r.PathValue("name")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, ok())
}

// hCreateLease reserves ports for an owner.
func (d *Daemon) hCreateLease(w http.ResponseWriter, r *http.Request) {
	var req api.LeaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, err)
		return
	}
	lease, err := d.svc.CreateLease(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, lease)
}

// hLeases lists leases, optionally only those of ?owner=.
func (d *Daemon) hLeases(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, d.svc.Leases(r.URL.Query().Get("owner")))
}

func (d *Daemon) hReleaseLease(w http.ResponseWriter, r *http.Request) {
	if _, err := d.svc.ReleaseLease(r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, ok())
}

// hAddRoute publishes a host for a port pm did not start. The body is as strict
// as POST /run's: an unknown field is an error, so a misspelt `owner` does not
// leave a route that deleting the owner would not remove.
func (d *Daemon) hAddRoute(w http.ResponseWriter, r *http.Request) {
	var req registry.Route
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, err)
		return
	}
	route, err := d.svc.AddRoute(req)
	if err != nil {
		writeRouteErr(w, err)
		return
	}
	writeJSON(w, 201, route)
}

// hRoutes lists every route served, explicit and derived from projects.
func (d *Daemon) hRoutes(w http.ResponseWriter, _ *http.Request) {
	routes, err := d.svc.Routes()
	if err != nil {
		writeErr(w, err)
		return
	}
	if routes == nil {
		routes = []api.RouteInfo{}
	}
	writeJSON(w, 200, routes)
}

// hRemoveRoute deletes an explicit route. A success has no body.
func (d *Daemon) hRemoveRoute(w http.ResponseWriter, r *http.Request) {
	if _, err := d.svc.RemoveRoute(r.PathValue("host")); err != nil {
		writeRouteErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeRouteErr answers a route failure: 409 for a host that already routes
// elsewhere, 404 for a host with no explicit route, the usual 400 otherwise.
func writeRouteErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, registry.ErrRouteConflict):
		writeJSON(w, 409, map[string]string{"error": err.Error()})
	case errors.Is(err, registry.ErrRouteNotFound):
		writeJSON(w, 404, map[string]string{"error": err.Error()})
	default:
		writeErr(w, err)
	}
}

// hOwnerStop stops everything that belongs to an owner and keeps its leases.
func (d *Daemon) hOwnerStop(w http.ResponseWriter, r *http.Request) {
	res, err := d.svc.StopOwner(r.PathValue("owner"))
	writeOwnerResult(w, res, err)
}

// hOwnerDelete stops everything that belongs to an owner and releases its leases.
func (d *Daemon) hOwnerDelete(w http.ResponseWriter, r *http.Request) {
	res, err := d.svc.DeleteOwner(r.PathValue("owner"))
	writeOwnerResult(w, res, err)
}

// writeOwnerResult answers an owner operation. A failure is the usual 400
// {"error": ...}, plus the result fields so the caller still learns what was
// stopped before it.
func writeOwnerResult(w http.ResponseWriter, res api.OwnerResult, err error) {
	if err == nil {
		writeJSON(w, 200, res)
		return
	}
	writeJSON(w, 400, struct {
		Error string `json:"error"`
		api.OwnerResult
	}{err.Error(), res})
}

// hEvents streams live state as SSE. The unnamed `data:` frames carry the ps
// table, immediately and then every 2s. Named frames report changes: a
// `lease.created` or `lease.released` event with the lease as data, as soon as
// the daemon makes the change and within one tick when another process (the
// CLI) does. A client sees only the leases that change after it connected;
// GET /leases gives the starting point. `owner.stopped` and `owner.deleted`
// carry the owner result, as soon as the daemon (API or MCP) stops or deletes
// an owner; the CLI, a separate process, does not publish them. `route.created`
// and `route.deleted` carry an explicit route, like the lease events: at once
// when the daemon changes it, within one tick when the CLI does.
func (d *Daemon) hEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, errNoFlush)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Take the wake-up channel before the baseline so a change in between is
	// not lost: it either shows in the baseline or closes the channel.
	wake := d.svc.LeaseChanged()
	routeWake := d.svc.RouteChanged()
	ownerEvents, cancelOwner := d.svc.SubscribeOwnerEvents()
	defer cancelOwner()
	known := leasesByID(d.svc.Leases(""))
	knownRoutes := routesByHost(d.svc.ExplicitRoutes())

	send := func() {
		rows, err := d.svc.PS()
		if err != nil {
			return
		}
		b, _ := json.Marshal(rows)
		w.Write([]byte("data: "))
		w.Write(b)
		w.Write([]byte("\n\n"))
		flusher.Flush()
	}
	sendLeaseChanges := func() {
		current := leasesByID(d.svc.Leases(""))
		for _, ev := range diffLeases(known, current) {
			b, _ := json.Marshal(ev.Lease)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, b)
		}
		known = current
		flusher.Flush()
	}
	sendRouteChanges := func() {
		current := routesByHost(d.svc.ExplicitRoutes())
		for _, ev := range diffExplicitRoutes(knownRoutes, current) {
			b, _ := json.Marshal(ev.Route)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, b)
		}
		knownRoutes = current
		flusher.Flush()
	}
	send()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-wake:
			wake = d.svc.LeaseChanged()
			sendLeaseChanges()
		case <-routeWake:
			routeWake = d.svc.RouteChanged()
			sendRouteChanges()
		case ev := <-ownerEvents:
			b, _ := json.Marshal(ev.Result)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, b)
			flusher.Flush()
		case <-ticker.C:
			send()
			sendLeaseChanges()
			sendRouteChanges()
		}
	}
}

// leaseEvent is one change between two lease sets.
type leaseEvent struct {
	Name  string // lease.created | lease.released
	Lease registry.Lease
}

func leasesByID(ls []registry.Lease) map[string]registry.Lease {
	out := make(map[string]registry.Lease, len(ls))
	for _, l := range ls {
		out[l.ID] = l
	}
	return out
}

// diffLeases lists what changed from prev to cur, creations first, each group
// in id order so the stream is deterministic.
func diffLeases(prev, cur map[string]registry.Lease) []leaseEvent {
	var evs []leaseEvent
	for _, id := range sortedIDs(cur) {
		if _, had := prev[id]; !had {
			evs = append(evs, leaseEvent{"lease.created", cur[id]})
		}
	}
	for _, id := range sortedIDs(prev) {
		if _, has := cur[id]; !has {
			evs = append(evs, leaseEvent{"lease.released", prev[id]})
		}
	}
	return evs
}

func sortedIDs(m map[string]registry.Lease) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// routeEvent is one change between two route sets.
type routeEvent struct {
	Name  string // route.created | route.deleted
	Route registry.Route
}

func routesByHost(rs []registry.Route) map[string]registry.Route {
	out := make(map[string]registry.Route, len(rs))
	for _, r := range rs {
		out[r.Host] = r
	}
	return out
}

// diffExplicitRoutes lists what changed from prev to cur, in host order so the stream
// is deterministic: removals first, so a host that changed port reads as the
// old route going and the new one arriving.
func diffExplicitRoutes(prev, cur map[string]registry.Route) []routeEvent {
	var evs []routeEvent
	for _, host := range sortedHosts(prev) {
		if now, has := cur[host]; !has || now.Port != prev[host].Port {
			evs = append(evs, routeEvent{"route.deleted", prev[host]})
		}
	}
	for _, host := range sortedHosts(cur) {
		if was, had := prev[host]; !had || was.Port != cur[host].Port {
			evs = append(evs, routeEvent{"route.created", cur[host]})
		}
	}
	return evs
}

func sortedHosts(m map[string]registry.Route) []string {
	hosts := make([]string, 0, len(m))
	for h := range m {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	return hosts
}

// hLogStream tails a project's log file over SSE.
func (d *Daemon) hLogStream(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, errNoFlush)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	f, err := os.Open(d.svc.LogPath(name))
	if err != nil {
		// no log yet: keep the stream open, poll for the file
		w.Write([]byte(": waiting for logs\n\n"))
		flusher.Flush()
		<-r.Context().Done()
		return
	}
	defer f.Close()
	reader := bufio.NewReader(f)
	for {
		select {
		case <-r.Context().Done():
			return
		default:
		}
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			w.Write([]byte("data: "))
			w.Write([]byte(line))
			w.Write([]byte("\n\n"))
			flusher.Flush()
		}
		if err == io.EOF {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if err != nil {
			return
		}
	}
}

// --- helpers -------------------------------------------------------------

func launchOpts(r *http.Request) runner.LaunchOpts {
	opts := runner.LaunchOpts{}
	if v := r.URL.Query().Get("port"); v != "" {
		if x, err := strconv.Atoi(v); err == nil {
			opts.Port = x
		}
	}
	opts.Force = r.URL.Query().Get("force") == "true"
	return opts
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	writeJSON(w, 400, map[string]string{"error": err.Error()})
}

func ok() map[string]bool { return map[string]bool{"ok": true} }

type simpleErr string

func (e simpleErr) Error() string { return string(e) }

const errNoFlush = simpleErr("streaming not supported")
