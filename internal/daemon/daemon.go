// Package daemon is the long-lived process: it serves the HTTP API, the SSE
// live feed, the embedded web dashboard and the MCP endpoint, and keeps the
// Caddy proxy config in sync with live state.
package daemon

import (
	"context"
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
}

// New builds a daemon around a service.
func New(svc *api.Service, port int) *Daemon {
	return &Daemon{svc: svc, port: port}
}

// Serve blocks serving until ctx is cancelled.
func (d *Daemon) Serve(ctx context.Context) error {
	mux := http.NewServeMux()
	d.routes(mux)

	// static SPA (fallback for any non-/api path)
	if sub, err := webui.FS(); err == nil {
		mux.Handle("/", spaHandler(sub))
	}

	// MCP endpoint (streamable HTTP)
	mux.Handle("/mcp", mcp.Handler(d.svc))
	mux.Handle("/mcp/", mcp.Handler(d.svc))

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
	paths := d.svc.Paths
	var last string
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		rows, err := d.svc.PS()
		if err == nil {
			var routes []proxy.Route
			for _, r := range rows {
				if r.Status == "up" && r.Port != 0 && r.Name != "?" {
					host := hostFor(d.svc, r.Name)
					routes = append(routes, proxy.Route{Host: host, Port: r.Port})
				}
			}
			content := proxy.Generate(routes, d.port)
			if content != last {
				if err := proxy.Write(paths.Caddyfile, routes, d.port); err == nil {
					_ = proxy.Reload(paths.Caddyfile)
					last = content
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func hostFor(svc *api.Service, name string) string {
	if p, ok := svc.Get(name); ok {
		return p.Hostname()
	}
	return name + ".localhost"
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
