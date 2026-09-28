package daemon

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/antoniojosev/pm/internal/registry"
	"github.com/antoniojosev/pm/internal/runner"
)

func (d *Daemon) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/ps", d.hPS)
	mux.HandleFunc("GET /api/projects", d.hProjects)
	mux.HandleFunc("POST /api/projects", d.hUpsertProject)
	mux.HandleFunc("DELETE /api/projects/{name}", d.hDeleteProject)
	mux.HandleFunc("POST /api/projects/{name}/start", d.hStart)
	mux.HandleFunc("POST /api/projects/{name}/stop", d.hStop)
	mux.HandleFunc("POST /api/projects/{name}/restart", d.hRestart)
	mux.HandleFunc("POST /api/projects/{name}/promote", d.hPromote)
	mux.HandleFunc("POST /api/projects/{name}/demote", d.hDemote)
	mux.HandleFunc("GET /api/projects/{name}/logs", d.hLogs)
	mux.HandleFunc("POST /api/claim", d.hClaim)
	mux.HandleFunc("POST /api/scan", d.hScan)
	mux.HandleFunc("GET /api/groups", d.hGroups)
	mux.HandleFunc("POST /api/groups", d.hUpsertGroup)
	mux.HandleFunc("DELETE /api/groups/{name}", d.hDeleteGroup)
	mux.HandleFunc("POST /api/groups/{name}/up", d.hGroupUp)
	mux.HandleFunc("POST /api/groups/{name}/down", d.hGroupDown)
	mux.HandleFunc("GET /api/events", d.hEvents)
	mux.HandleFunc("GET /api/logs/stream", d.hLogStream)
	mux.HandleFunc("POST /api/instances/{port}/stop", d.hInstanceStop)
	mux.HandleFunc("POST /api/instances/{port}/restart", d.hInstanceRestart)
	mux.HandleFunc("GET /api/fs", d.hBrowse)
	mux.HandleFunc("GET /api/detect", d.hDetect)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
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

// hEvents streams the ps table as SSE every 2s (and once immediately).
func (d *Daemon) hEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, errNoFlush)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

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
	send()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			send()
		}
	}
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
