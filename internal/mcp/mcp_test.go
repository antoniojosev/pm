package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/antoniojosev/pm/internal/api"
	"github.com/antoniojosev/pm/internal/config"
	"github.com/antoniojosev/pm/internal/detector"
	"github.com/antoniojosev/pm/internal/registry"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func TestSplitFields(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"npm run dev", []string{"npm", "run", "dev"}},
		{"  spaced   out  ", []string{"spaced", "out"}},
		{"", nil},
		{"single", []string{"single"}},
	}
	for _, tc := range tests {
		if got := splitFields(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitFields(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestResultHelpers(t *testing.T) {
	res, err := jsonResult(map[string]int{"port": 3000}, nil)
	if err != nil || res.IsError || !strings.Contains(res.Content[0].(mcpgo.TextContent).Text, "3000") {
		t.Fatalf("jsonResult ok = %+v, %v", res, err)
	}
	res, err = jsonResult(nil, errors.New("boom"))
	if err != nil || !res.IsError {
		t.Fatalf("jsonResult error = %+v, %v", res, err)
	}
	res, err = textResult("stopped", nil)
	if err != nil || res.IsError {
		t.Fatalf("textResult ok = %+v, %v", res, err)
	}
	res, err = textResult("stopped", errors.New("boom"))
	if err != nil || !res.IsError {
		t.Fatalf("textResult error = %+v, %v", res, err)
	}
}

// mcpClient drives the streamable-HTTP endpoint with raw JSON-RPC.
type mcpClient struct {
	t       *testing.T
	srv     *httptest.Server
	session string
	next    int
}

func newMCPClient(t *testing.T) *mcpClient {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	paths := config.Paths{Root: root, Registry: filepath.Join(root, "registry.json"), LogsDir: filepath.Join(root, "logs"), RunDir: filepath.Join(root, "run"), Caddyfile: filepath.Join(root, "Caddyfile")}
	if err := paths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	store, err := registry.Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	svc := api.New(paths, store, nil)
	svc.Run.Snap = func() ([]detector.Instance, error) { return nil, nil }
	srv := httptest.NewServer(Handler(svc))
	t.Cleanup(srv.Close)

	c := &mcpClient{t: t, srv: srv}
	c.rpc("initialize", `{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"0"}}`)
	return c
}

func (c *mcpClient) rpc(method, params string) string {
	c.t.Helper()
	c.next++
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, c.next, method, params)
	req, _ := http.NewRequest("POST", c.srv.URL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	res, err := c.srv.Client().Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		c.t.Fatalf("%s → %d %s", method, res.StatusCode, raw)
	}
	if sid := res.Header.Get("Mcp-Session-Id"); sid != "" {
		c.session = sid
	}
	return string(raw)
}

func (c *mcpClient) call(tool, args string) (text string, isError bool) {
	c.t.Helper()
	raw := c.rpc("tools/call", fmt.Sprintf(`{"name":%q,"arguments":%s}`, tool, args))
	var env struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		c.t.Fatalf("%s: bad response %s", tool, raw)
	}
	if env.Error != nil {
		return env.Error.Message, true
	}
	if len(env.Result.Content) > 0 {
		text = env.Result.Content[0].Text
	}
	return text, env.Result.IsError
}

func TestToolsAreAdvertised(t *testing.T) {
	c := newMCPClient(t)
	raw := c.rpc("tools/list", `{}`)
	for _, tool := range []string{"pm_list", "pm_start", "pm_stop", "pm_restart", "pm_promote", "pm_demote", "pm_register", "pm_remove", "pm_claim", "pm_scan", "pm_logs", "pm_group_up", "pm_group_down"} {
		if !strings.Contains(raw, `"name":"`+tool+`"`) {
			t.Errorf("tool %s not advertised", tool)
		}
	}
}

func TestToolsRoundTrip(t *testing.T) {
	c := newMCPClient(t)

	out, isErr := c.call("pm_register", `{"name":"web","dir":"/srv/web","cmd":"npm run dev","port":5173}`)
	if isErr || !strings.Contains(out, `"prefer_port": 5173`) || !strings.Contains(out, `"npm"`) {
		t.Fatalf("pm_register = %v %s", isErr, out)
	}
	if out, isErr := c.call("pm_register", `{"name":""}`); !isErr || !strings.Contains(out, "name is required") {
		t.Fatalf("pm_register without name = %v %s", isErr, out)
	}

	out, isErr = c.call("pm_list", `{}`)
	if isErr || !strings.Contains(out, `"name": "web"`) || !strings.Contains(out, `"status": "down"`) {
		t.Fatalf("pm_list = %v %s", isErr, out)
	}

	if out, isErr := c.call("pm_logs", `{"name":"web","lines":5}`); isErr || out != "" {
		t.Fatalf("pm_logs (no log yet) = %v %q", isErr, out)
	}
	if out, isErr := c.call("pm_scan", `{}`); isErr || strings.TrimSpace(out) != "null" {
		t.Fatalf("pm_scan = %v %q", isErr, out)
	}

	// lifecycle tools surface service errors as MCP tool errors, not transport errors
	for tool, args := range map[string]string{
		"pm_start":      `{"name":"ghost"}`,
		"pm_restart":    `{"name":"ghost"}`,
		"pm_promote":    `{"name":"ghost"}`,
		"pm_demote":     `{"name":"ghost"}`,
		"pm_stop":       `{"name":"web"}`,
		"pm_claim":      `{"port":1,"name":"x"}`,
		"pm_group_up":   `{"group":"nope"}`,
		"pm_group_down": `{"group":"nope"}`,
	} {
		if out, isErr := c.call(tool, args); !isErr || out == "" {
			t.Errorf("%s should report a tool error, got %v %q", tool, isErr, out)
		}
	}

	if out, isErr := c.call("pm_remove", `{"name":"web"}`); isErr || out != "removed" {
		t.Fatalf("pm_remove = %v %q", isErr, out)
	}
	if _, isErr := c.call("pm_remove", `{"name":"web"}`); !isErr {
		t.Fatal("removing twice should fail")
	}
}
