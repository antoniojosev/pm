package daemon

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/antoniojosev/pm/internal/testfake"
)

func runBody(t *testing.T, extra string) string {
	t.Helper()
	b, _ := json.Marshal(t.TempDir())
	return `{"name":"x","dir":` + string(b) + `,"cmd":["sleep","100"]` + extra + `}`
}

func TestRunOverHTTP(t *testing.T) {
	d, srv := newTestDaemon(t)
	d.svc.PortFree = func(int) bool { return true }
	testfake.Install(t)

	code, obj, raw := do(t, srv, "POST", "/api/v1/run", runBody(t,
		`,"env":{"PORT":"4100"},"limits":{"memory":"256M","cpu":"50%"},"owner":"wt-qa","ports":[4100]`))
	if code != 200 {
		t.Fatalf("POST /run = %d %s", code, raw)
	}
	if obj["unit"] != "pm-x.service" || obj["pid"] != float64(testfake.MainPID) {
		t.Fatalf("body = %s", raw)
	}
	if ports, _ := obj["ports"].([]any); len(ports) != 1 || ports[0] != float64(4100) {
		t.Fatalf("ports = %s", raw)
	}
	assertConforms(t, "POST", "/run", raw)

	// in /ps with its owner, up although nothing listens
	_, _, raw = do(t, srv, "GET", "/api/v1/ps", "")
	var rows []map[string]any
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["name"] != "x" || rows[0]["owner"] != "wt-qa" || rows[0]["status"] != "up" {
		t.Fatalf("ps = %s", raw)
	}
	assertConforms(t, "GET", "/ps", raw)

	// and stopped like any project
	code, obj, _ = do(t, srv, "POST", "/api/v1/projects/x/stop", "")
	if code != 200 || obj["ok"] != true {
		t.Fatalf("stop = %d %v", code, obj)
	}
	_, _, raw = do(t, srv, "GET", "/api/v1/ps", "")
	if strings.TrimSpace(raw) != "null" && strings.TrimSpace(raw) != "[]" {
		t.Fatalf("ps after stop = %s", raw)
	}
}

func TestRunDefaultsAndTheDeprecatedAlias(t *testing.T) {
	_, srv := newTestDaemon(t)
	testfake.Install(t)
	code, obj, raw := do(t, srv, "POST", "/api/run", runBody(t, ""))
	if code != 200 || obj["unit"] != "pm-x.service" {
		t.Fatalf("POST /api/run = %d %s", code, raw)
	}
	if ports, ok := obj["ports"].([]any); !ok || len(ports) != 0 {
		t.Fatalf("no ports must come back as [], got %s", raw)
	}
	_, _, raw = do(t, srv, "GET", "/api/v1/ps", "")
	if !strings.Contains(raw, `"owner":"local"`) {
		t.Fatalf("default owner missing: %s", raw)
	}
}

func TestRunRequestErrors(t *testing.T) {
	d, srv := newTestDaemon(t)
	d.svc.PortFree = func(int) bool { return true }
	sd := testfake.Install(t)

	tests := []struct{ name, body, want string }{
		{"bad json", `{nope`, ""},
		{"unknown field", runBody(t, `,"limit":{"memory":"1M"}`), "unknown field"},
		{"no name", `{"dir":"/tmp","cmd":["true"]}`, "name is required"},
		{"no cmd", `{"name":"x","dir":"/tmp"}`, "cmd is required"},
		{"relative dir", `{"name":"x","dir":"rel","cmd":["true"]}`, "absolute"},
		{"bad limit", runBody(t, `,"limits":{"memory":"lots"}`), "invalid memory limit"},
		{"reserved env", runBody(t, `,"env":{"PM_PROJECT":"y"}`), "reserved"},
		{"bad port", runBody(t, `,"ports":[70000]`), "invalid port"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, obj, raw := do(t, srv, "POST", "/api/v1/run", tc.body)
			msg, _ := obj["error"].(string)
			if code != 400 || msg == "" || !strings.Contains(msg, tc.want) {
				t.Fatalf("= %d %s", code, raw)
			}
		})
	}
	if len(sd.Calls("systemd-run")) != 0 {
		t.Fatal("systemd-run ran for a refused request")
	}

	// a taken name, then a port leased to someone else
	if code, _, raw := do(t, srv, "POST", "/api/v1/run", runBody(t, "")); code != 200 {
		t.Fatalf("first run = %d %s", code, raw)
	}
	code, obj, _ := do(t, srv, "POST", "/api/v1/run", runBody(t, ""))
	if msg, _ := obj["error"].(string); code != 400 || !strings.Contains(msg, "already running") {
		t.Fatalf("same name twice = %d %v", code, obj)
	}
	_, _, raw := do(t, srv, "POST", "/api/v1/leases", `{"owner":"wt-qa","prefer":4300}`)
	l := decodeLease(t, raw)
	body := strings.Replace(runBody(t, `,"owner":"wt-dev","ports":[`+itoa(l.Ports[0])+`]`), `"name":"x"`, `"name":"y"`, 1)
	code, obj, _ = do(t, srv, "POST", "/api/v1/run", body)
	if msg, _ := obj["error"].(string); code != 400 || !strings.Contains(msg, "leased to") {
		t.Fatalf("a port leased to another owner = %d %v", code, obj)
	}
}

func TestSimultaneousRunRequestsNeverShareAPort(t *testing.T) {
	d, srv := newTestDaemon(t)
	d.svc.PortFree = func(int) bool { return true }
	testfake.Install(t)

	const racers = 15
	results := make(chan int, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		body := strings.Replace(runBody(t, `,"ports":[4100]`), `"name":"x"`, `"name":"r`+itoa(i)+`"`, 1)
		go func() {
			<-start
			res, err := srv.Client().Post(srv.URL+"/api/v1/run", "application/json", strings.NewReader(body))
			if err != nil {
				t.Error(err)
				results <- 0
				return
			}
			res.Body.Close()
			results <- res.StatusCode
		}()
	}
	close(start)
	var codes []int
	for i := 0; i < racers; i++ {
		codes = append(codes, <-results)
	}
	if n := len(slices.DeleteFunc(slices.Clone(codes), func(c int) bool { return c != 200 })); n != 1 {
		t.Fatalf("%d requests got port 4100, want 1: %v", n, codes)
	}
}

// POST /run takes `compose`, and /ps reports the run as a docker entry; the
// project it names is also accepted, and kept, on POST /projects.
func TestRunWithComposeOverHTTP(t *testing.T) {
	d, srv := newTestDaemon(t)
	d.svc.PortFree = func(int) bool { return true }
	sd := testfake.Install(t)

	body := strings.Replace(runBody(t, `,"compose":{"file":"compose.yml","project":"shop-qa"}`),
		`["sleep","100"]`, `["docker","compose","up"]`, 1)
	code, _, raw := do(t, srv, "POST", "/api/v1/run", body)
	if code != 200 {
		t.Fatalf("POST /run = %d %s", code, raw)
	}
	assertConforms(t, "POST", "/run", raw)
	args := sd.Calls("systemd-run")[0]
	if !slices.Contains(args, "--setenv=COMPOSE_PROJECT_NAME=shop-qa") || !slices.Contains(args, "-p") {
		t.Fatalf("systemd-run args = %v", args)
	}
	if got := args[slices.Index(args, "--")+1:]; !slices.Equal(got, []string{"docker", "compose", "-f", "compose.yml", "-p", "shop-qa", "up"}) {
		t.Fatalf("command = %v", got)
	}
	_, _, raw = do(t, srv, "GET", "/api/v1/ps", "")
	if !strings.Contains(raw, `"kind":"docker"`) {
		t.Fatalf("ps = %s", raw)
	}

	// refused: bad project name, with a 400 and nothing started
	bad := strings.Replace(runBody(t, `,"compose":{"project":"No Good"}`), `"x"`, `"y"`, 1)
	if code, obj, raw := do(t, srv, "POST", "/api/v1/run", bad); code != 400 || !strings.Contains(raw, "invalid compose project") {
		t.Fatalf("bad compose = %d %v", code, obj)
	}
	// a typo in the field is an error, as for the other fields
	typo := strings.Replace(runBody(t, `,"compse":{"project":"p"}`), `"x"`, `"z"`, 1)
	if code, _, _ := do(t, srv, "POST", "/api/v1/run", typo); code != 400 {
		t.Fatalf("misspelt compose = %d", code)
	}
}

func TestProjectWithComposeOverHTTP(t *testing.T) {
	_, srv := newTestDaemon(t)
	code, _, raw := do(t, srv, "POST", "/api/v1/projects",
		`{"name":"shop","dir":"/srv/shop","cmd":["docker","compose","up"],"compose":{"project":"shop-wt2","file":"c.yml"}}`)
	if code != 200 {
		t.Fatalf("POST /projects = %d %s", code, raw)
	}
	assertConforms(t, "POST", "/projects", raw)
	_, _, raw = do(t, srv, "GET", "/api/v1/projects", "")
	assertConforms(t, "GET", "/projects", raw)
	if !strings.Contains(raw, `"kind":"docker"`) || !strings.Contains(raw, `"project":"shop-wt2"`) {
		t.Fatalf("projects = %s", raw)
	}
	if code, _, raw := do(t, srv, "POST", "/api/v1/projects",
		`{"name":"web","kind":"native","compose":{"project":"web"}}`); code != 400 || !strings.Contains(raw, "kind docker") {
		t.Fatalf("compose on a native project = %d %s", code, raw)
	}
}
