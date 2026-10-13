// Package daemon is the long-lived process: it serves the HTTP API, the SSE
// live feed, the embedded web dashboard and the MCP endpoint, and keeps the
// Caddy proxy config in sync with live state.
package daemon

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/antoniojosev/pm/internal/api"
	"github.com/antoniojosev/pm/internal/mcp"
	"github.com/antoniojosev/pm/internal/proxy"
	"github.com/antoniojosev/pm/internal/webui"
)

// Daemon bundles the service and HTTP server.
type Daemon struct {
	svc  *api.Service
	port int
	// proxyErr is the failure SyncProxy last logged, "" when the last round
	// went through. Only the reconciliation loop touches it.
	proxyErr string
}

// New builds a daemon around a service.
func New(svc *api.Service, port int) *Daemon {
	return &Daemon{svc: svc, port: port}
}

// mounter is the part of *http.ServeMux that mount() needs; a test swaps in a
// recorder to see every pattern the daemon registers.
type mounter interface {
	router
	Handle(pattern string, handler http.Handler)
}

// mount registers everything the daemon serves: the HTTP API, the embedded
// dashboard and the MCP endpoint. Serve calls it; nothing else registers
// patterns, so a recorder passed here sees the complete surface.
func (d *Daemon) mount(mux mounter) {
	d.routes(mux)

	// static SPA (fallback for any non-/api path)
	if sub, err := webui.FS(); err == nil {
		mux.Handle("/", spaHandler(sub))
	}

	// MCP endpoint (streamable HTTP)
	mux.Handle("/mcp", mcp.Handler(d.svc))
	mux.Handle("/mcp/", mcp.Handler(d.svc))
}

// Handler is everything the daemon serves as one http.Handler, without
// listening or running the proxy loop. Serve uses it; so can a test that wants
// the real daemon behind an httptest server.
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	d.mount(mux)
	return mux
}

// Serve blocks serving until ctx is cancelled.
func (d *Daemon) Serve(ctx context.Context) error {
	mux := d.Handler()

	srv := &http.Server{
		Addr:    "127.0.0.1:" + itoa(d.port),
		Handler: logRequests(mux),
	}

	go d.reconcileProxy(ctx)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("pm daemon at http://127.0.0.1:%d  (dashboard: http://pm.localhost)", d.port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// reconcileProxy regenerates the Caddyfile whenever the set of live routes
// changes, then reloads Caddy.
func (d *Daemon) reconcileProxy(ctx context.Context) {
	if os.Getenv("PM_NO_PROXY") == "1" {
		return // don't manage Caddy (useful for a secondary daemon / tests)
	}
	var last string
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		last = d.SyncProxy(last)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// SyncProxy is one round of the reconciliation: it renders the Caddyfile from
// every route (those declared with `pm route add` and those derived from
// running projects, the explicit one winning on a shared host), and when the
// text differs from last it writes the file and reloads Caddy. It returns the
// text now in force, to pass to the next call. When the file cannot be written
// or the reload fails it returns "", which is never equal to a render: the
// file on disk may already hold text Caddy did not accept, so the next round
// must write and reload again whatever the routes are by then. Returning last
// would be wrong, because a route removed in the meantime would render the
// same text as last and the stale file would stay on disk, to be served by
// Caddy the next time it starts. A failure is logged when it first appears or
// its message changes, and the recovery once; a Caddy that stays down does not
// fill the journal. A missing caddy binary is not a failure: there is nothing
// to reload.
func (d *Daemon) SyncProxy(last string) string {
	routes, err := d.svc.Routes()
	if err != nil {
		return last
	}
	pr := make([]proxy.Route, 0, len(routes))
	for _, r := range routes {
		pr = append(pr, proxy.Route{Host: r.Host, Port: r.Port})
	}
	content := proxy.Generate(pr, d.port)
	if content == last {
		return last
	}
	if err := proxy.Write(d.svc.Paths.Caddyfile, pr, d.port); err != nil {
		d.proxyFailed(fmt.Sprintf("proxy: write %s: %v (will retry)", d.svc.Paths.Caddyfile, err))
		return ""
	}
	if err := proxy.Reload(d.svc.Paths.Caddyfile); err != nil {
		d.proxyFailed(fmt.Sprintf("proxy: %v (will retry)", err))
		return ""
	}
	if d.proxyErr != "" {
		log.Printf("proxy: reload succeeded again")
		d.proxyErr = ""
	}
	return content
}

// proxyFailed logs msg unless it is the message already logged for the
// failure in progress.
func (d *Daemon) proxyFailed(msg string) {
	if msg != d.proxyErr {
		log.Print(msg)
		d.proxyErr = msg
	}
}

// spaHandler serves the embedded SPA with index.html fallback for client routes.
func spaHandler(sub fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := fs.Stat(sub, trimLeadingSlash(r.URL.Path)); err != nil && r.URL.Path != "/" {
			// unknown path -> serve index.html (client-side routing)
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

func trimLeadingSlash(p string) string {
	if len(p) > 0 && p[0] == '/' {
		return p[1:]
	}
	return p
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
	})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
