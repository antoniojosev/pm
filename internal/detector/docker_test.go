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
		{Project: "shop", Port: 5432, Cmd: "docker: shop-db-1", Source: SourceDocker, Docker: true, Container: "abc"},
		{Project: "lonely", Port: 8080, Cmd: "docker: lonely", Source: SourceDocker, Docker: true, Container: "def"},
		{Project: "noid", Port: 9000, Cmd: "docker: noid", Source: SourceDocker, Docker: true, Container: "noid"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseDockerPS =\n%+v\nwant\n%+v", got, want)
	}
}
