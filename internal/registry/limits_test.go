package registry

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLimitsValidate(t *testing.T) {
	tests := []struct {
		name    string
		l       Limits
		wantErr string // substring; "" means valid
	}{
		{"empty is valid", Limits{}, ""},
		{"megabytes", Limits{Memory: "512M"}, ""},
		{"gigabytes", Limits{Memory: "2G"}, ""},
		{"plain bytes", Limits{Memory: "1048576"}, ""},
		{"memory percentage", Limits{Memory: "50%"}, ""},
		{"memory infinity", Limits{Memory: "infinity"}, ""},
		{"cpu one core", Limits{CPU: "100%"}, ""},
		{"cpu fractional", Limits{CPU: "12.5%"}, ""},
		{"cpu more than a core", Limits{CPU: "150%"}, ""},
		{"both", Limits{Memory: "256M", CPU: "50%"}, ""},
		{"memory lowercase unit", Limits{Memory: "512m"}, "invalid memory limit"},
		{"memory unknown unit", Limits{Memory: "5X"}, "invalid memory limit"},
		{"memory words", Limits{Memory: "lots"}, "invalid memory limit"},
		{"memory zero", Limits{Memory: "0"}, "invalid memory limit"},
		{"memory zero with unit", Limits{Memory: "0M"}, "invalid memory limit"},
		{"memory newline can not reach a unit file", Limits{Memory: "1G\nExecStartPre=/bin/evil"}, "invalid memory limit"},
		{"cpu without percent", Limits{CPU: "150"}, "invalid cpu limit"},
		{"cpu zero", Limits{CPU: "0%"}, "invalid cpu limit"},
		{"cpu negative", Limits{CPU: "-5%"}, "invalid cpu limit"},
		{"cpu newline", Limits{CPU: "50%\nUser=root"}, "invalid cpu limit"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.l.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Validate(%+v) = %v, want nil", tc.l, err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("Validate(%+v) = %v, want %q", tc.l, err, tc.wantErr)
			}
		})
	}
}

func TestLimitsMerge(t *testing.T) {
	base := Limits{Memory: "1G", CPU: "100%"}
	if got := base.Merge(Limits{}); got != base {
		t.Fatalf("empty override changed the base: %+v", got)
	}
	if got := base.Merge(Limits{Memory: "2G"}); got != (Limits{Memory: "2G", CPU: "100%"}) {
		t.Fatalf("memory override = %+v", got)
	}
	if got := (Limits{}).Merge(Limits{CPU: "50%"}); got != (Limits{CPU: "50%"}) {
		t.Fatalf("override on empty base = %+v", got)
	}
}

func TestLimitsJSON(t *testing.T) {
	// A project without limits keeps the stored/API shape it always had.
	b, _ := json.Marshal(Project{Name: "web"})
	if strings.Contains(string(b), "limits") {
		t.Fatalf("empty limits must be omitted: %s", b)
	}

	b, _ = json.Marshal(Project{Name: "web", Limits: Limits{Memory: "256M"}})
	if !strings.Contains(string(b), `"limits":{"memory":"256M"}`) {
		t.Fatalf("limits not serialised as expected: %s", b)
	}

	var p Project
	if err := json.Unmarshal([]byte(`{"name":"web","limits":{"memory":"1G","cpu":"150%"}}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.Limits != (Limits{Memory: "1G", CPU: "150%"}) {
		t.Fatalf("decoded limits = %+v", p.Limits)
	}
}

func TestUpsertRejectsInvalidLimits(t *testing.T) {
	s, err := Open(tempPaths(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert(Project{Name: "web", Limits: Limits{Memory: "lots"}}); err == nil {
		t.Fatal("Upsert accepted an invalid memory limit")
	}
	if _, ok := s.Get("web"); ok {
		t.Fatal("a rejected project was stored")
	}
	if err := s.Upsert(Project{Name: "web", Limits: Limits{Memory: "256M", CPU: "50%"}}); err != nil {
		t.Fatalf("Upsert valid limits: %v", err)
	}
	got, _ := s.Get("web")
	if got.Limits != (Limits{Memory: "256M", CPU: "50%"}) {
		t.Fatalf("stored limits = %+v", got.Limits)
	}
}
