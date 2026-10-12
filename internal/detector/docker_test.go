package detector

import (
	"reflect"
	"testing"
)

func TestComposeProject(t *testing.T) {
	tests := []struct {
		name   string
		labels string
		want   string
	}{
		{"compose label present", "com.docker.compose.config-hash=abc,com.docker.compose.project=shop,com.docker.compose.service=db", "shop"},
		{"first position", "com.docker.compose.project=shop", "shop"},
		{"no compose labels", "maintainer=someone", ""},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := composeProject(tc.labels); got != tc.want {
				t.Fatalf("composeProject = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPublishedPorts(t *testing.T) {
	tests := []struct {
		name  string
		ports string
		want  []int
	}{
		{"v4 and v6 dedupe", "0.0.0.0:18000->8000/tcp, :::18000->8000/tcp", []int{18000}},
		{"several mappings", "0.0.0.0:80->80/tcp, 0.0.0.0:443->443/tcp", []int{80, 443}},
		{"bound to loopback", "127.0.0.1:5432->5432/tcp", []int{5432}},
		{"exposed but unpublished", "8000/tcp", nil},
		{"empty", "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := publishedPorts(tc.ports); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("publishedPorts(%q) = %v, want %v", tc.ports, got, tc.want)
			}
		})
	}
}

func TestParseDockerPS(t *testing.T) {
	out := `{"ID":"abc","Names":"shop-db-1","Ports":"0.0.0.0:5432->5432/tcp, :::5432->5432/tcp","Labels":"com.docker.compose.project=shop"}
{"ID":"def","Names":"lonely","Ports":"0.0.0.0:8080->80/tcp","Labels":""}
{"ID":"ghi","Names":"noports","Ports":"","Labels":"com.docker.compose.project=shop"}
not json
{"Names":"noid","Ports":"0.0.0.0:9000->9000/tcp","Labels":""}
`
	got := parseDockerPS(out)
	want := []Instance{
		{Project: "shop", Port: 5432, Cmd: "docker: shop-db-1", Source: SourceDocker, Docker: true, Container: "abc", Compose: "shop"},
		{Project: "lonely", Port: 8080, Cmd: "docker: lonely", Source: SourceDocker, Docker: true, Container: "def"},
		{Project: "noid", Port: 9000, Cmd: "docker: noid", Source: SourceDocker, Docker: true, Container: "noid"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseDockerPS =\n%+v\nwant\n%+v", got, want)
	}
}

// Containers of a stack started under a compose name of its own are attributed
// to the pm project that declared it, by their com.docker.compose.project label.
func TestDockerListenersAttributeComposeProjectsToTheirPMProject(t *testing.T) {
	fakeRun(t, map[string]string{"docker": `{"ID":"a1","Names":"shop-a-stack-web-1","Ports":"0.0.0.0:18001->80/tcp","Labels":"com.docker.compose.project=shop-a-stack"}
{"ID":"b1","Names":"shop-b-stack-web-1","Ports":"0.0.0.0:18002->80/tcp","Labels":"com.docker.compose.project=shop-b-stack"}
{"ID":"c1","Names":"other-db-1","Ports":"0.0.0.0:5432->5432/tcp","Labels":"com.docker.compose.project=other"}
{"ID":"d1","Names":"lonely","Ports":"0.0.0.0:8080->80/tcp","Labels":""}
`})
	d := Detector{ComposeToName: func(project string) (string, bool) {
		switch project {
		case "shop-a-stack":
			return "shop-a", true
		case "shop-b-stack":
			return "shop-b", true
		}
		return "", false
	}}
	got := map[int]Instance{}
	for _, i := range d.dockerListeners() {
		got[i.Port] = i
	}
	for port, want := range map[int]struct{ project, compose string }{
		18001: {"shop-a", "shop-a-stack"},
		18002: {"shop-b", "shop-b-stack"},
		5432:  {"other", "other"}, // no pm project declared it: the compose name stands
		8080:  {"lonely", ""},
	} {
		if got[port].Project != want.project || got[port].Compose != want.compose || !got[port].Docker {
			t.Errorf("port %d = %+v, want project %q compose %q", port, got[port], want.project, want.compose)
		}
	}
	// the container is still the one docker knows, so stopping it still works
	if got[18001].Container != "a1" {
		t.Errorf("container = %q", got[18001].Container)
	}

	// without the mapping, attribution is by compose project name as before
	for _, i := range (Detector{}).dockerListeners() {
		if i.Port == 18001 && i.Project != "shop-a-stack" {
			t.Errorf("without ComposeToName: project = %q", i.Project)
		}
	}
}
