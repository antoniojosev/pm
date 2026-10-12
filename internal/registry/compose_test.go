package registry

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestComposeValidate(t *testing.T) {
	tests := []struct {
		name    string
		c       Compose
		wantErr string // substring; "" means valid
	}{
		{"project only", Compose{Project: "shop"}, ""},
		{"project and file", Compose{Project: "shop-qa_2", File: "deploy/compose.yml"}, ""},
		{"absolute file", Compose{Project: "shop", File: "/srv/shop/compose.yml"}, ""},
		{"digit first", Compose{Project: "1shop"}, ""},
		{"no project", Compose{File: "compose.yml"}, "compose.project is required"},
		{"uppercase", Compose{Project: "Shop"}, "invalid compose project"},
		{"space", Compose{Project: "my shop"}, "invalid compose project"},
		{"dash first", Compose{Project: "-shop"}, "invalid compose project"},
		{"dot", Compose{Project: "shop.qa"}, "invalid compose project"},
		{"too long", Compose{Project: strings.Repeat("a", MaxComposeProjectLen+1)}, "invalid compose project"},
		{"newline in file", Compose{Project: "shop", File: "a\nb.yml"}, "invalid compose file"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.c.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestProjectValidateCompose(t *testing.T) {
	good := Project{Name: "shop", Kind: KindDocker, Compose: &Compose{Project: "shop-qa"}}
	if err := good.Validate(); err != nil {
		t.Fatalf("docker project with a compose stack: %v", err)
	}
	native := good
	native.Kind = KindNative
	if err := native.Validate(); err == nil || !strings.Contains(err.Error(), "kind docker") {
		t.Fatalf("compose on a native project: %v", err)
	}
	bad := good
	bad.Compose = &Compose{Project: "No Good"}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "invalid compose project") {
		t.Fatalf("bad compose project: %v", err)
	}
	// limits stay refused on docker projects, with or without a compose name
	capped := good
	capped.Limits = Limits{Memory: "1G"}
	if err := capped.Validate(); err == nil || !strings.Contains(err.Error(), "docker") {
		t.Fatalf("limits on a compose project: %v", err)
	}
}

func TestComposeSurvivesTheRegistryFile(t *testing.T) {
	paths := tempPaths(t)
	s, err := Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	p := Project{Name: "shop", Dir: t.TempDir(), Kind: KindDocker, Tier: TierEphemeral,
		Cmd: []string{"docker", "compose", "up"}, Compose: &Compose{File: "compose.yml", Project: "shop-qa"}}
	if err := s.Upsert(p); err != nil {
		t.Fatal(err)
	}
	again, err := Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := again.Get("shop")
	if !ok || got.Compose == nil || *got.Compose != *p.Compose {
		t.Fatalf("reloaded = %+v, %v", got, ok)
	}
	// and a project without one writes no compose key at all
	raw, _ := json.Marshal(Project{Name: "web"})
	if strings.Contains(string(raw), "compose") {
		t.Fatalf("an empty compose was serialised: %s", raw)
	}
}

func TestFindByCompose(t *testing.T) {
	s, err := Open(tempPaths(t))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Upsert(Project{Name: "plain", Kind: KindDocker})
	_ = s.Upsert(Project{Name: "shop-a", Kind: KindDocker, Compose: &Compose{Project: "shop-a-stack"}})
	_ = s.Upsert(Project{Name: "shop-b", Kind: KindDocker, Compose: &Compose{Project: "shop-b-stack"}})

	p, ok := s.FindByCompose("shop-b-stack")
	if !ok || p.Name != "shop-b" {
		t.Fatalf("FindByCompose = %+v, %v", p, ok)
	}
	if p, ok := s.FindByCompose("SHOP-A-STACK"); !ok || p.Name != "shop-a" {
		t.Fatalf("case-insensitive lookup = %+v, %v", p, ok)
	}
	if _, ok := s.FindByCompose("plain"); ok {
		t.Fatal("a project with no compose stack must not match by its name")
	}
	if _, ok := s.FindByCompose("nobody"); ok {
		t.Fatal("unknown compose project matched")
	}
}
