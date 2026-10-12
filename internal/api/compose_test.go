package api

import (
	"slices"
	"testing"

	"github.com/antoniojosev/pm/internal/registry"
	"github.com/antoniojosev/pm/internal/runner"
	"github.com/antoniojosev/pm/internal/testfake"
)

func composeRun(t *testing.T, name, project, dir string) runner.RunSpec {
	t.Helper()
	return runner.RunSpec{
		Name: name, Dir: dir, Cmd: []string{"docker", "compose", "up"}, Owner: "wt-" + name,
		Compose: &registry.Compose{File: "compose.yml", Project: project},
	}
}

// The criterion of the step: two runs of one compose file, with different names,
// live side by side. Each gets its own compose project (-p and
// COMPOSE_PROJECT_NAME), and `docker ps` containers are told apart by their
// com.docker.compose.project label, so each pm row sees only its own stack.
func TestTwoRunsOfOneComposeFileCoexist(t *testing.T) {
	svc, sd := runService(t)
	svc.Run.Snap = nil // the real detector, with the fake docker on PATH
	dir := t.TempDir() // the same repo directory for both: only the names differ

	// nothing runs yet: docker lists no containers
	sd.DockerPS(t)
	if _, err := svc.StartRun(composeRun(t, "shop-a", "shop-a-stack", dir)); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, err := svc.StartRun(composeRun(t, "shop-b", "shop-b-stack", dir)); err != nil {
		t.Fatalf("second run of the same compose file: %v", err)
	}
	if !sd.Active("pm-shop-a.service") || !sd.Active("pm-shop-b.service") {
		t.Fatal("both units must be running")
	}

	calls := sd.Calls("systemd-run")
	if len(calls) != 2 {
		t.Fatalf("systemd-run calls = %v", calls)
	}
	for i, project := range []string{"shop-a-stack", "shop-b-stack"} {
		args := calls[i]
		if !slices.Contains(args, "--setenv=COMPOSE_PROJECT_NAME="+project) {
			t.Errorf("run %d lacks COMPOSE_PROJECT_NAME=%s: %v", i, project, args)
		}
		sep := slices.Index(args, "--")
		if got := args[sep+1:]; !slices.Equal(got, []string{"docker", "compose", "-f", "compose.yml", "-p", project, "up"}) {
			t.Errorf("run %d command = %v", i, got)
		}
	}

	// docker now reports each stack's container under its compose project label
	sd.DockerPS(t,
		testfake.Container{ID: "a1", Names: "shop-a-stack-web-1", Ports: "0.0.0.0:18001->80/tcp", ComposeProject: "shop-a-stack"},
		testfake.Container{ID: "b1", Names: "shop-b-stack-web-1", Ports: "0.0.0.0:18002->80/tcp", ComposeProject: "shop-b-stack"},
	)
	a, b := rowByName(t, svc, "shop-a"), rowByName(t, svc, "shop-b")
	if a.Status != "up" || a.Port != 18001 || !a.Docker || a.Kind != "docker" || a.Owner != "wt-shop-a" {
		t.Errorf("shop-a row = %+v", a)
	}
	if b.Status != "up" || b.Port != 18002 || !b.Docker || b.Kind != "docker" || b.Owner != "wt-shop-b" {
		t.Errorf("shop-b row = %+v", b)
	}
	rows, _ := svc.PS()
	for _, r := range rows {
		if r.Status == "unclaimed" {
			t.Errorf("a container of a named stack was left unclaimed: %+v", r)
		}
	}

	// stopping one leaves the other alone
	if err := svc.Stop("shop-a"); err != nil {
		t.Fatal(err)
	}
	if sd.Active("pm-shop-a.service") || !sd.Active("pm-shop-b.service") {
		t.Fatal("stopping shop-a must not touch shop-b")
	}
}

// The same compose file under one name twice is still refused: the name is the
// unit, the compose project does not change that.
func TestRunNameStillCollidesWhateverTheComposeProject(t *testing.T) {
	svc, _ := runService(t)
	dir := t.TempDir()
	if _, err := svc.StartRun(composeRun(t, "shop", "stack-1", dir)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartRun(composeRun(t, "shop", "stack-2", dir)); err == nil {
		t.Fatal("a live run's name was taken again")
	}
}

// A registered project is attributed through its compose project too.
func TestRegisteredComposeProjectIsAttributedByLabel(t *testing.T) {
	svc, sd := runService(t)
	svc.Run.Snap = nil
	if err := svc.Add(registry.Project{
		Name: "shop", Dir: t.TempDir(), Cmd: []string{"docker", "compose", "up"},
		Compose: &registry.Compose{Project: "shop-wt2"},
	}); err != nil {
		t.Fatal(err)
	}
	if p, _ := svc.Get("shop"); p.Kind != registry.KindDocker {
		t.Fatalf("kind = %q: a compose stack is a docker project", p.Kind)
	}
	sd.DockerPS(t, testfake.Container{ID: "s1", Names: "shop-wt2-web-1", Ports: "0.0.0.0:18080->80/tcp", ComposeProject: "shop-wt2"})
	if r := rowByName(t, svc, "shop"); r.Status != "up" || r.Port != 18080 || !r.Docker {
		t.Fatalf("row = %+v", r)
	}
}

func TestAddRejectsAComposeStackOnANativeProject(t *testing.T) {
	svc := newTestService(t)
	err := svc.Add(registry.Project{Name: "web", Kind: registry.KindNative, Compose: &registry.Compose{Project: "web"}})
	if err == nil {
		t.Fatal("a native project took a compose stack")
	}
	if _, ok := svc.Get("web"); ok {
		t.Fatal("a rejected project was stored")
	}
}
