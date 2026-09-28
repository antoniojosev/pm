package detector

import (
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

// Trimmed from real `ss -H -t -l -n -p` output.
const ssSample = `LISTEN 0      4096                 127.0.0.53%lo:53    0.0.0.0:*
LISTEN 0      511                        0.0.0.0:5173  0.0.0.0:*
LISTEN 0      4096                     127.0.0.1:7437  0.0.0.0:* users:(("engram",pid=13876,fd=9))
LISTEN 0      4096                             *:3000        *:* users:(("node",pid=201,fd=22))
LISTEN 0      4096                          [::]:3000     [::]:* users:(("node",pid=201,fd=23))
LISTEN 0      128                        0.0.0.0:8000  0.0.0.0:* users:(("python",pid=300,fd=5),("python",pid=301,fd=5))
garbage line
`

func TestParseSS(t *testing.T) {
	got := parseSS(ssSample)
	want := []listener{
		{Port: 53},                          // no visible pid
		{Port: 5173},                        // no visible pid
		{Port: 7437, PIDs: []int{13876}},    // single owner
		{Port: 3000, PIDs: []int{201}},      // v4 + v6 dedupe to one entry
		{Port: 8000, PIDs: []int{300, 301}}, // prefork: two pids on one port
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseSS =\n%+v\nwant\n%+v", got, want)
	}
}

func TestPortFromAddr(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"127.0.0.1:18000", 18000},
		{"*:5173", 5173},
		{"[::]:3000", 3000},
		{"127.0.0.53%lo:53", 53},
		{"0.0.0.0:*", 0},
		{"noport", 0},
		{"", 0},
	}
	for _, tc := range tests {
		if got := portFromAddr(tc.in); got != tc.want {
			t.Errorf("portFromAddr(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParsePIDs(t *testing.T) {
	tests := []struct {
		in   string
		want []int
	}{
		{`users:(("node",pid=201,fd=22))`, []int{201}},
		{`users:(("python",pid=300,fd=5),("python",pid=301,fd=5))`, []int{300, 301}},
		{`LISTEN 0 511 0.0.0.0:5173 0.0.0.0:*`, nil},
	}
	for _, tc := range tests {
		if got := parsePIDs(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parsePIDs(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestUnitFromCgroup(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		wantUnit string
		wantName string
	}{
		{"transient service", "0::/user.slice/user-1000.slice/user@1000.service/app.slice/pm-web.service\n", "pm-web.service", "web"},
		{"scope", "0::/user.slice/user@1000.service/pm-my.app_2.scope\n", "pm-my.app_2.scope", "my.app_2"},
		{"cgroup v1 multi-line", "12:pids:/user.slice\n1:name=systemd:/user.slice/pm-api.service\n0::/init.scope\n", "pm-api.service", "api"},
		{"foreign unit", "0::/user.slice/user@1000.service/app.slice/code.service\n", "", ""},
		{"init scope", "0::/init.scope\n", "", ""},
		{"empty", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			unit, name := unitFromCgroup(tc.content)
			if unit != tc.wantUnit || name != tc.wantName {
				t.Fatalf("unitFromCgroup = (%q, %q), want (%q, %q)", unit, name, tc.wantUnit, tc.wantName)
			}
		})
	}
}

func TestMarkerFromEnviron(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"present", "HOME=/home/x\x00PM_PROJECT=web\x00PATH=/bin\x00", "web"},
		{"first entry", "PM_PROJECT=api\x00HOME=/x\x00", "api"},
		{"absent", "HOME=/home/x\x00PATH=/bin\x00", ""},
		{"prefix must be exact", "XPM_PROJECT=web\x00PM_PROJECT_OLD=x\x00", ""},
		{"empty value", "PM_PROJECT=\x00", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := markerFromEnviron(tc.content); got != tc.want {
				t.Fatalf("markerFromEnviron = %q, want %q", got, tc.want)
			}
		})
	}
}

// fakeRun scripts the output of external commands by binary name.
func fakeRun(t *testing.T, outputs map[string]string) {
	t.Helper()
	prev := run
	run = func(name string, _ ...string) (string, error) {
		out, ok := outputs[name]
		if !ok {
			return "", exec.ErrNotFound
		}
		return out, nil
	}
	t.Cleanup(func() { run = prev })
}

// spawnMarked starts a child carrying PM_PROJECT so attribution can be
// exercised against a real /proc entry that belongs to this test.
func spawnMarked(t *testing.T, project string) int {
	t.Helper()
	if _, err := os.Stat("/proc/self/environ"); err != nil {
		t.Skip("procfs not available")
	}
	if b, _ := os.ReadFile("/proc/self/cgroup"); unitRe.Match(b) {
		t.Skip("test process itself runs inside a pm unit; cgroup attribution would be ambiguous")
	}
	cmd := exec.Command("sleep", "30")
	cmd.Env = append(os.Environ(), "PM_PROJECT="+project)
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	// /proc/<pid>/environ and cmdline populate a moment after exec.
	for i := 0; i < 40 && procMarker(cmd.Process.Pid) == ""; i++ {
		time.Sleep(25 * time.Millisecond)
	}
	return cmd.Process.Pid
}

func TestAttributeByMarker(t *testing.T) {
	pid := spawnMarked(t, "demo")
	d := Detector{}

	inst := d.attribute(4000, pid)
	if inst.Project != "demo" || inst.Source != SourceMarker {
		t.Fatalf("attribute = %+v, want project demo via marker", inst)
	}
	if inst.PID != pid || inst.Port != 4000 || inst.Cmd != "sleep 30" {
		t.Fatalf("attribute = %+v", inst)
	}
	if inst.Cwd == "" {
		t.Fatal("cwd should be resolved from /proc")
	}
}

func TestAttributeByCwdFallback(t *testing.T) {
	// The test binary itself: no pm cgroup, no marker → falls back to cwd.
	if b, _ := os.ReadFile("/proc/self/cgroup"); unitRe.Match(b) {
		t.Skip("test process runs inside a pm unit")
	}
	self := os.Getpid()
	cwd, _ := os.Getwd()

	d := Detector{DirToName: func(dir string) (string, string, bool) {
		if dir == cwd {
			return "pm-tests", "pm-pm-tests.service", true
		}
		return "", "", false
	}}
	inst := d.attribute(9000, self)
	if inst.Project != "pm-tests" || inst.Source != SourceCwd || inst.Unit != "pm-pm-tests.service" {
		t.Fatalf("attribute = %+v", inst)
	}

	// no resolver → stays unknown
	inst = Detector{}.attribute(9000, self)
	if inst.Project != "" || inst.Source != SourceUnknown {
		t.Fatalf("attribute without DirToName = %+v", inst)
	}
}

func TestAttributeMissingPid(t *testing.T) {
	inst := Detector{}.attribute(1, 1<<30)
	if inst.Source != SourceUnknown || inst.Cmd != "" || inst.Cwd != "" || inst.Project != "" {
		t.Fatalf("attribute of missing pid = %+v", inst)
	}
}

func TestSnapshotMergesNativeAndDocker(t *testing.T) {
	pid := spawnMarked(t, "web")
	fakeRun(t, map[string]string{
		"ss": `LISTEN 0 511 0.0.0.0:5173 0.0.0.0:* users:(("node",pid=` + itoa(pid) + `,fd=22))
LISTEN 0 4096 127.0.0.53%lo:53 0.0.0.0:*
`,
		"docker": `{"ID":"abc","Names":"shop-db-1","Ports":"0.0.0.0:5432->5432/tcp, :::5432->5432/tcp","Labels":"com.docker.compose.project=shop,com.docker.compose.service=db"}
`,
	})

	insts, err := Detector{}.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(insts) != 3 {
		t.Fatalf("expected 3 instances, got %d: %+v", len(insts), insts)
	}
	if insts[0].Project != "web" || insts[0].Port != 5173 || insts[0].Source != SourceMarker {
		t.Fatalf("native instance = %+v", insts[0])
	}
	if insts[1].Port != 53 || insts[1].PID != 0 || insts[1].Source != SourceUnknown {
		t.Fatalf("root-owned instance = %+v", insts[1])
	}
	if insts[2].Project != "shop" || insts[2].Port != 5432 || !insts[2].Docker || insts[2].Container != "abc" {
		t.Fatalf("docker instance = %+v", insts[2])
	}
}

func TestSnapshotToleratesMissingTools(t *testing.T) {
	fakeRun(t, map[string]string{})
	insts, err := Detector{}.Snapshot()
	if err != nil || len(insts) != 0 {
		t.Fatalf("Snapshot without ss/docker = %v, %v", insts, err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
