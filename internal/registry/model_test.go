package registry

import "testing"

func TestSlugify(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "myapp", "myapp"},
		{"uppercase folds", "MyApp", "myapp"},
		{"space becomes hyphen", "web dashboard", "web-dashboard"},
		{"runs collapse", "web   dashboard", "web-dashboard"},
		{"underscore and dot", "api_v2.backend", "api-v2-backend"},
		{"leading garbage dropped", "  --app", "app"},
		{"trailing garbage dropped", "app--  ", "app"},
		{"digits kept", "svc2024", "svc2024"},
		{"unicode replaced", "café ñu", "caf-u"},
		{"empty falls back", "", "app"},
		{"only symbols falls back", "***", "app"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := slugify(tc.in); got != tc.want {
				t.Fatalf("slugify(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestProjectHostname(t *testing.T) {
	tests := []struct {
		name string
		p    Project
		want string
	}{
		{"name only", Project{Name: "myapp"}, "myapp.localhost"},
		{"name with spaces", Project{Name: "Web Dashboard"}, "web-dashboard.localhost"},
		{"explicit host wins", Project{Name: "myapp", Host: "front"}, "front.localhost"},
		{"explicit host is slugified", Project{Name: "x", Host: "Admin Panel"}, "admin-panel.localhost"},
		{"host with suffix already", Project{Name: "x", Host: "front.localhost"}, "front-localhost.localhost"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.Hostname(); got != tc.want {
				t.Fatalf("Hostname() = %q, want %q", got, tc.want)
			}
		})
	}
}
