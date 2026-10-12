package runner

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/antoniojosev/pm/internal/registry"
)

func TestApplyCompose(t *testing.T) {
	c := &registry.Compose{File: "deploy/compose.yml", Project: "shop-qa"}
	tests := []struct {
		name    string
		cmd     []string
		c       *registry.Compose
		wantCmd []string
		wantEnv []string
	}{
		{
			name: "no compose changes nothing", cmd: []string{"docker", "compose", "up"}, c: nil,
			wantCmd: []string{"docker", "compose", "up"},
		},
		{
			name: "docker compose gets -f and -p right after compose", cmd: []string{"docker", "compose", "up", "-d"}, c: c,
			wantCmd: []string{"docker", "compose", "-f", "deploy/compose.yml", "-p", "shop-qa", "up", "-d"},
			wantEnv: []string{"COMPOSE_PROJECT_NAME=shop-qa"},
		},
		{
			name: "without a file only -p", cmd: []string{"docker", "compose", "up"}, c: &registry.Compose{Project: "shop-qa"},
			wantCmd: []string{"docker", "compose", "-p", "shop-qa", "up"},
			wantEnv: []string{"COMPOSE_PROJECT_NAME=shop-qa"},
		},
		{
			name: "docker-compose v1", cmd: []string{"docker-compose", "up"}, c: c,
			wantCmd: []string{"docker-compose", "-f", "deploy/compose.yml", "-p", "shop-qa", "up"},
			wantEnv: []string{"COMPOSE_PROJECT_NAME=shop-qa"},
		},
		{
			name: "absolute path to docker", cmd: []string{"/usr/bin/docker", "compose", "up"}, c: &registry.Compose{Project: "p"},
			wantCmd: []string{"/usr/bin/docker", "compose", "-p", "p", "up"},
			wantEnv: []string{"COMPOSE_PROJECT_NAME=p"},
		},
		{
			name: "another command only gets the environment", cmd: []string{"make", "up"}, c: c,
			wantCmd: []string{"make", "up"},
			wantEnv: []string{"COMPOSE_PROJECT_NAME=shop-qa", "COMPOSE_FILE=deploy/compose.yml"},
		},
		{
			name: "docker without compose is not a compose call", cmd: []string{"docker", "run", "x"}, c: &registry.Compose{Project: "p"},
			wantCmd: []string{"docker", "run", "x"},
			wantEnv: []string{"COMPOSE_PROJECT_NAME=p"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			orig := slices.Clone(tc.cmd)
			gotCmd, gotEnv := applyCompose(tc.cmd, tc.c)
			if !reflect.DeepEqual(gotCmd, tc.wantCmd) {
				t.Errorf("cmd = %v, want %v", gotCmd, tc.wantCmd)
			}
			if !slices.Equal(gotEnv, tc.wantEnv) {
				t.Errorf("env = %v, want %v", gotEnv, tc.wantEnv)
			}
			if !slices.Equal(tc.cmd, orig) {
				t.Errorf("the input command was modified: %v", tc.cmd)
			}
		})
	}
}

// systemdRunCalls returns the arguments of every systemd-run invocation.
func (f *fakeExec) systemdRunCalls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if c[0] == "systemd-run" {
			out = append(out, c[1:])
		}
	}
	return out
}

// commandOf is what follows the "--" in a systemd-run call.
func commandOf(args []string) []string {
	i := slices.Index(args, "--")
	if i < 0 {
		return nil
	}
	return args[i+1:]
}

func TestLaunchSpecComposeNamesTheStack(t *testing.T) {
	r, f := newTestRunner(t)
	spec := specFor(t)
	spec.Cmd = []string{"docker", "compose", "up"}
	spec.Compose = &registry.Compose{File: "compose.yml", Project: "shop-qa"}
	spec.Env = map[string]string{"A": "1"}
	spec.Owner = "wt-qa"

	if _, err := r.LaunchSpec(spec); err != nil {
		t.Fatalf("LaunchSpec: %v", err)
	}
	args := f.argsOf("systemd-run")
	if got := commandOf(args); !slices.Equal(got, []string{"docker", "compose", "-f", "compose.yml", "-p", "shop-qa", "up"}) {
		t.Errorf("command = %v", got)
	}
	for _, want := range []string{"--setenv=COMPOSE_PROJECT_NAME=shop-qa", "--setenv=A=1", "--setenv=PM_PROJECT=x"} {
		if !contains(args, want) {
			t.Errorf("systemd-run lacks %q: %v", want, args)
		}
	}

	// the record is a docker entry that remembers the stack, and the command as given
	p, ok := r.Store.Get("x")
	if !ok || !p.IsRun() || p.Kind != registry.KindDocker || p.Compose == nil || *p.Compose != *spec.Compose {
		t.Fatalf("stored = %+v, %v", p, ok)
	}
	if !slices.Equal(p.Cmd, spec.Cmd) {
		t.Errorf("stored cmd = %v, want the one given", p.Cmd)
	}
	if p.Compose == spec.Compose {
		t.Error("the stored compose shares memory with the caller's spec")
	}
}

func TestLaunchSpecComposeWithAnotherCommandSetsTheEnvironment(t *testing.T) {
	r, f := newTestRunner(t)
	spec := specFor(t)
	spec.Cmd = []string{"make", "up"}
	spec.Compose = &registry.Compose{File: "stack.yml", Project: "shop-qa"}
	if _, err := r.LaunchSpec(spec); err != nil {
		t.Fatal(err)
	}
	args := f.argsOf("systemd-run")
	if got := commandOf(args); !slices.Equal(got, []string{"make", "up"}) {
		t.Errorf("command = %v", got)
	}
	for _, want := range []string{"--setenv=COMPOSE_PROJECT_NAME=shop-qa", "--setenv=COMPOSE_FILE=stack.yml"} {
		if !contains(args, want) {
			t.Errorf("systemd-run lacks %q: %v", want, args)
		}
	}
}

func TestLaunchSpecWithoutComposeIsUntouched(t *testing.T) {
	r, f := newTestRunner(t)
	spec := specFor(t)
	spec.Cmd = []string{"docker", "compose", "up"} // a compose command, but no stack named
	if _, err := r.LaunchSpec(spec); err != nil {
		t.Fatal(err)
	}
	args := f.argsOf("systemd-run")
	if got := commandOf(args); !slices.Equal(got, spec.Cmd) {
		t.Errorf("command = %v", got)
	}
	for _, a := range args {
		if strings.Contains(a, "COMPOSE_") {
			t.Errorf("compose variable without a compose stack: %q", a)
		}
	}
	if p, _ := r.Store.Get("x"); p.Kind != registry.KindNative || p.Compose != nil {
		t.Errorf("stored = %+v", p)
	}
}

// The criterion of the step, at the runner: one compose file, two names.
func TestTwoRunsOfOneComposeFileGetTheirOwnProjects(t *testing.T) {
	r, f := newTestRunner(t)
	dir := t.TempDir() // two worktrees of a repo would differ here; the file name is the same
	for _, name := range []string{"shop-a", "shop-b"} {
		_, err := r.LaunchSpec(RunSpec{
			Name: name, Dir: dir, Cmd: []string{"docker", "compose", "up"},
			Compose: &registry.Compose{File: "compose.yml", Project: name + "-stack"},
		})
		if err != nil {
			t.Fatalf("run %s: %v", name, err)
		}
	}
	calls := f.systemdRunCalls()
	if len(calls) != 2 {
		t.Fatalf("systemd-run calls = %v", calls)
	}
	for i, name := range []string{"shop-a", "shop-b"} {
		want := []string{"docker", "compose", "-f", "compose.yml", "-p", name + "-stack", "up"}
		if got := commandOf(calls[i]); !slices.Equal(got, want) {
			t.Errorf("%s command = %v, want %v", name, got, want)
		}
		if !contains(calls[i], "--unit=pm-"+name+".service") || !contains(calls[i], "--setenv=COMPOSE_PROJECT_NAME="+name+"-stack") {
			t.Errorf("%s call = %v", name, calls[i])
		}
	}
	for _, name := range []string{"shop-a", "shop-b"} {
		if p, ok := r.Store.Get(name); !ok || p.Compose == nil || p.Compose.Project != name+"-stack" {
			t.Errorf("%s stored = %+v, %v", name, p, ok)
		}
	}
}

func TestRunSpecNormalizeCompose(t *testing.T) {
	good := RunSpec{Name: "x", Dir: t.TempDir(), Cmd: []string{"docker", "compose", "up"},
		Compose: &registry.Compose{Project: "shop-qa", File: "compose.yml"}}
	tests := []struct {
		name string
		mut  func(*RunSpec)
		want string // "" = valid
	}{
		{"valid", func(*RunSpec) {}, ""},
		{"project only", func(s *RunSpec) { s.Compose = &registry.Compose{Project: "p"} }, ""},
		{"no project", func(s *RunSpec) { s.Compose = &registry.Compose{File: "c.yml"} }, "compose.project is required"},
		{"bad project", func(s *RunSpec) { s.Compose = &registry.Compose{Project: "Shop QA"} }, "invalid compose project"},
		{"limits need a native process", func(s *RunSpec) { s.Limits.Memory = "512M" }, "docker"},
		{"env may not set the project", func(s *RunSpec) { s.Env = map[string]string{"COMPOSE_PROJECT_NAME": "other"} }, "reserved with compose"},
		{"env may not set the file when one is given", func(s *RunSpec) { s.Env = map[string]string{"COMPOSE_FILE": "other.yml"} }, "reserved with compose.file"},
		{"env may set the file when none is given", func(s *RunSpec) {
			s.Compose = &registry.Compose{Project: "p"}
			s.Env = map[string]string{"COMPOSE_FILE": "other.yml"}
		}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := good
			tc.mut(&s)
			_, err := s.Normalize()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestInvalidComposeSpecTouchesNothing(t *testing.T) {
	r, f := newTestRunner(t)
	spec := specFor(t)
	spec.Compose = &registry.Compose{Project: "NOT valid"}
	if _, err := r.LaunchSpec(spec); err == nil {
		t.Fatal("expected an error")
	}
	if len(f.calls) != 0 || len(r.Store.List()) != 0 {
		t.Fatalf("an invalid spec had effects: calls %v, store %v", f.calls, r.Store.List())
	}
}

// A registered docker project with a compose stack is started the same way.
func TestStartComposeProject(t *testing.T) {
	r, f := newTestRunner(t)
	dir := t.TempDir()
	if err := r.Store.Upsert(registry.Project{
		Name: "shop", Dir: dir, Kind: registry.KindDocker, Tier: registry.TierEphemeral,
		Cmd: []string{"docker", "compose", "up"}, Compose: &registry.Compose{File: "compose.yml", Project: "shop-wt2"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Start("shop", LaunchOpts{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	args := f.argsOf("systemd-run")
	if got := commandOf(args); !slices.Equal(got, []string{"docker", "compose", "-f", "compose.yml", "-p", "shop-wt2", "up"}) {
		t.Errorf("command = %v", got)
	}
	if !contains(args, "--setenv=COMPOSE_PROJECT_NAME=shop-wt2") {
		t.Errorf("systemd-run lacks the project variable: %v", args)
	}
}

func TestWriteUnitCarriesTheComposeProject(t *testing.T) {
	r, _ := newTestRunner(t)
	p := registry.Project{Name: "shop", Dir: t.TempDir(), Kind: registry.KindDocker, Tier: registry.TierService,
		Cmd: []string{"docker", "compose", "up"}, Compose: &registry.Compose{Project: "shop-wt2"}}
	cmd, env := applyCompose(p.Cmd, p.Compose)
	if err := r.writeUnit(p, cmd, env); err != nil {
		t.Fatal(err)
	}
	raw, err := readFile(r.unitPath("shop"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Environment=COMPOSE_PROJECT_NAME=shop-wt2", "ExecStart=docker compose -p shop-wt2 up"} {
		if !strings.Contains(raw, want) {
			t.Errorf("unit lacks %q:\n%s", want, raw)
		}
	}
}
