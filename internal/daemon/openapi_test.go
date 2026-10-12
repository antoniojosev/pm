package daemon

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// httpMethods are the keys of an OpenAPI path item that denote operations (the
// others, like "parameters", are not routes).
var httpMethods = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true,
	"options": true, "head": true, "patch": true, "trace": true,
}

// documentedRoutes returns "METHOD /path" for every operation in the OpenAPI
// file at path.
func documentedRoutes(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(doc.Paths) == 0 {
		t.Fatalf("%s has no paths", path)
	}
	var out []string
	for p, item := range doc.Paths {
		for key := range item {
			if httpMethods[key] {
				out = append(out, strings.ToUpper(key)+" "+p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// diffRoutes returns what is in code but not in docs and vice versa.
func diffRoutes(code, docs []string) (undocumented, stale []string) {
	in := func(set []string, v string) bool {
		for _, s := range set {
			if s == v {
				return true
			}
		}
		return false
	}
	for _, c := range code {
		if !in(docs, c) {
			undocumented = append(undocumented, c)
		}
	}
	for _, d := range docs {
		if !in(code, d) {
			stale = append(stale, d)
		}
	}
	return undocumented, stale
}

// TestRoutesMatchOpenAPI fails when a route exists in the API table without
// being documented in docs/api/openapi.yaml, and when the document describes a
// route the daemon does not serve.
func TestRoutesMatchOpenAPI(t *testing.T) {
	d, _ := newTestDaemon(t)
	var code []string
	for _, rt := range d.apiRoutes() {
		code = append(code, rt.Method+" "+rt.Path)
	}
	sort.Strings(code)

	docs := documentedRoutes(t, filepath.Join("..", "..", "docs", "api", "openapi.yaml"))
	undocumented, stale := diffRoutes(code, docs)
	for _, r := range undocumented {
		t.Errorf("route in code but not in docs/api/openapi.yaml: %s", r)
	}
	for _, r := range stale {
		t.Errorf("route in docs/api/openapi.yaml but not served: %s", r)
	}
}

// recorder is a mounter that only remembers the patterns it is given.
type recorder struct{ patterns []string }

func (r *recorder) HandleFunc(pattern string, _ func(http.ResponseWriter, *http.Request)) {
	r.patterns = append(r.patterns, pattern)
}
func (r *recorder) Handle(pattern string, _ http.Handler) {
	r.patterns = append(r.patterns, pattern)
}

// TestMountedAPIPatternsAreTheTable checks what Serve really mounts, not the
// table itself: every pattern registered under /api must be a table entry under
// /api/v1 or /api, so a hand-added mux.HandleFunc("GET /api/v1/foo", ...) fails
// here even though apiRoutes() (and so the OpenAPI comparison) never saw it.
func TestMountedAPIPatternsAreTheTable(t *testing.T) {
	d, _ := newTestDaemon(t)
	rec := &recorder{}
	d.mount(rec)

	var want []string
	for _, rt := range d.apiRoutes() {
		want = append(want, rt.Method+" "+APIPrefix+rt.Path, rt.Method+" "+legacyPrefix+rt.Path)
	}
	var got []string
	for _, p := range rec.patterns {
		path := p
		if i := strings.IndexByte(p, ' '); i >= 0 {
			path = p[i+1:]
		}
		if path == legacyPrefix || strings.HasPrefix(path, legacyPrefix+"/") {
			got = append(got, p)
		}
	}
	sort.Strings(want)
	sort.Strings(got)
	undocumented, stale := diffRoutes(got, want)
	for _, p := range undocumented {
		t.Errorf("pattern mounted under /api but not in the route table: %s", p)
	}
	for _, p := range stale {
		t.Errorf("route table entry never mounted: %s", p)
	}
	if len(got) != len(want) {
		t.Errorf("mounted %d /api patterns, table implies %d (duplicates?)", len(got), len(want))
	}
}

func TestDiffRoutesBothDirections(t *testing.T) {
	code := []string{"GET /a", "POST /b"}
	docs := []string{"GET /a", "DELETE /c"}
	undocumented, stale := diffRoutes(code, docs)
	if len(undocumented) != 1 || undocumented[0] != "POST /b" {
		t.Errorf("undocumented = %v, want [POST /b]", undocumented)
	}
	if len(stale) != 1 || stale[0] != "DELETE /c" {
		t.Errorf("stale = %v, want [DELETE /c]", stale)
	}
	if u, s := diffRoutes(code, code); len(u)+len(s) != 0 {
		t.Errorf("identical sets reported a diff: %v %v", u, s)
	}
}

func TestRouteTableHasNoDuplicates(t *testing.T) {
	d, _ := newTestDaemon(t)
	seen := map[string]bool{}
	for _, rt := range d.apiRoutes() {
		k := rt.Method + " " + rt.Path
		if seen[k] {
			t.Errorf("duplicate route %s", k)
		}
		seen[k] = true
		if rt.Handler == nil {
			t.Errorf("route %s has no handler", k)
		}
	}
}

func TestV1AndLegacyAlias(t *testing.T) {
	_, srv := newTestDaemon(t)

	get := func(path string) *http.Response {
		t.Helper()
		res, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { res.Body.Close() })
		return res
	}

	v1 := get("/api/v1/health")
	if v1.StatusCode != 200 {
		t.Fatalf("/api/v1/health = %d", v1.StatusCode)
	}
	if h := v1.Header.Get("Deprecation"); h != "" {
		t.Errorf("/api/v1 must not be deprecated, got Deprecation=%q", h)
	}

	old := get("/api/health")
	if old.StatusCode != 200 {
		t.Fatalf("/api/health = %d", old.StatusCode)
	}
	if h := old.Header.Get("Deprecation"); h != deprecationDate {
		t.Errorf("alias Deprecation = %q, want %q", h, deprecationDate)
	}
	if l := old.Header.Get("Link"); l != `</api/v1/health>; rel="successor-version"` {
		t.Errorf("alias Link = %q", l)
	}

	// the successor link keeps path values and the query string
	q := get("/api/projects/web/logs?n=3")
	if l := q.Header.Get("Link"); l != `</api/v1/projects/web/logs?n=3>; rel="successor-version"` {
		t.Errorf("alias Link with query = %q", l)
	}
}

// Every route must be reachable under both prefixes (the wrong-method case is
// TestWrongMethodIsRejected).
func TestEveryRouteUnderBothPrefixes(t *testing.T) {
	d, _ := newTestDaemon(t)
	mux := http.NewServeMux()
	d.routes(mux)
	for _, rt := range d.apiRoutes() {
		path := strings.NewReplacer("{name}", "x", "{port}", "1").Replace(rt.Path)
		for _, prefix := range []string{APIPrefix, legacyPrefix} {
			req := httptest.NewRequest(rt.Method, prefix+path, nil)
			if _, pattern := mux.Handler(req); pattern == "" {
				t.Errorf("%s %s%s not routed", rt.Method, prefix, rt.Path)
			}
		}
	}
}

func TestWrongMethodIsRejected(t *testing.T) {
	_, srv := newTestDaemon(t)
	for _, path := range []string{"/api/v1/claim", "/api/claim"} {
		res, err := srv.Client().Get(srv.URL + path) // /claim is POST-only
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want 405", path, res.StatusCode)
		}
	}
}

// openapi.yaml documents POST /projects as echoing the body as sent, without
// the defaults stored by the service; GET /projects returns them applied.
func TestUpsertProjectEchoesBodyWithoutDefaults(t *testing.T) {
	_, srv := newTestDaemon(t)
	code, obj, _ := do(t, srv, "POST", "/api/v1/projects", `{"name":"x"}`)
	if code != 200 || obj["name"] != "x" {
		t.Fatalf("upsert = %d %v", code, obj)
	}
	if obj["kind"] != "" || obj["tier"] != "" {
		t.Errorf("response must not carry defaults, got kind=%v tier=%v", obj["kind"], obj["tier"])
	}
	_, _, raw := do(t, srv, "GET", "/api/v1/projects", "")
	if !strings.Contains(raw, `"kind":"native"`) || !strings.Contains(raw, `"tier":"ephemeral"`) {
		t.Errorf("stored project should have defaults applied: %s", raw)
	}
}
