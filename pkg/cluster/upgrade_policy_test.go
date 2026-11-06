package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openshift-online/gcp-hcp-ctl/pkg/platformapi"
	gcpv1 "github.com/openshift-online/gecko/platform-api/api/public/v1"
)

func TestSetUpgradePolicyExclusions(t *testing.T) {
	for _, tc := range []struct {
		name        string
		flags       []string
		wantPresent bool
		wantCount   int
	}{
		{name: "preserve omitted exclusions"},
		{name: "replace exclusions", flags: []string{"--exclusion=holiday,2026-12-20T00:00:00Z,2027-01-03T00:00:00Z"}, wantPresent: true, wantCount: 1},
		{name: "clear exclusions", flags: []string{"--clear-exclusions"}, wantPresent: true},
		{name: "false clear preserves exclusions", flags: []string{"--clear-exclusions=false"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodPatch || r.URL.Path != "/apis/gcp.managed.openshift.io/v1/namespaces/my-project/clusters/upgrade-poc" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				if got := r.Header.Get("Content-Type"); got != "application/merge-patch+json" {
					t.Errorf("Content-Type = %q", got)
				}
				var patch struct {
					Spec struct {
						Maintenance map[string]json.RawMessage `json:"maintenance"`
					} `json:"spec"`
				}
				if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
					t.Errorf("decoding patch: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				raw, present := patch.Spec.Maintenance["exclusions"]
				if present != tc.wantPresent {
					t.Errorf("exclusions present = %v, want %v", present, tc.wantPresent)
				}
				if present {
					var exclusions []gcpv1.ControlPlaneMaintenanceExclusion
					if err := json.Unmarshal(raw, &exclusions); err != nil {
						t.Errorf("decoding exclusions: %v", err)
					}
					if len(exclusions) != tc.wantCount {
						t.Errorf("exclusions count = %d, want %d", len(exclusions), tc.wantCount)
					}
					if tc.wantCount == 0 && string(raw) != "[]" {
						t.Errorf("clear patch = %s, want []", raw)
					}
					if tc.wantCount == 1 && len(exclusions) == 1 && exclusions[0].Name != "holiday" {
						t.Errorf("exclusion name = %q", exclusions[0].Name)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"apiVersion":"gcp.managed.openshift.io/v1","kind":"Cluster","metadata":{"name":"upgrade-poc"}}`))
			}))
			t.Cleanup(server.Close)
			client, err := platformapi.NewClientForTest(server.URL, "my-project")
			if err != nil {
				t.Fatal(err)
			}
			args := append([]string{"set-upgrade-policy", "upgrade-poc", "--start-time=2026-10-10T02:00:00Z", "--duration-seconds=14400", "--day=saturday"}, tc.flags...)
			if err, _, _ := executeClusterTestCommand(t, client, context.Background(), args...); err != nil {
				t.Fatalf("setting policy: %v", err)
			}
			if requests != 1 {
				t.Errorf("requests = %d, want 1", requests)
			}
		})
	}
}

func TestSetUpgradePolicyRejectsConflictingExclusionFlags(t *testing.T) {
	err, _, _ := executeClusterTestCommand(t, nil, context.Background(),
		"set-upgrade-policy", "upgrade-poc", "--start-time=2026-10-10T02:00:00Z", "--duration-seconds=14400", "--day=saturday",
		"--clear-exclusions", "--exclusion=holiday,2026-12-20T00:00:00Z,2027-01-03T00:00:00Z")
	if err == nil || !strings.Contains(err.Error(), "none of the others can be") {
		t.Fatalf("error = %v, want mutually exclusive flag error", err)
	}
}

func TestBuildUpgradePolicy(t *testing.T) {
	opts := upgradePolicyOptions{
		startTime:       "2026-10-03T02:00:00Z",
		durationSeconds: 14400,
		frequency:       "weekly",
		days:            []string{"saturday"},
		exclusions:      []string{"holiday-freeze,2026-12-20T00:00:00Z,2027-01-03T00:00:00Z"},
	}

	maintenance, err := opts.build()
	if err != nil {
		t.Fatalf("building policy: %v", err)
	}

	if maintenance.Window == nil {
		t.Fatal("expected window to be set")
	}
	if maintenance.Window.DurationSeconds != 14400 {
		t.Errorf("expected duration 14400, got %d", maintenance.Window.DurationSeconds)
	}
	if maintenance.Exclusions == nil || len(*maintenance.Exclusions) != 1 {
		t.Fatalf("expected one exclusion")
	}
	if (*maintenance.Exclusions)[0].Name != "holiday-freeze" {
		t.Errorf("expected exclusion name %q, got %q", "holiday-freeze", (*maintenance.Exclusions)[0].Name)
	}
}

func TestBuildUpgradePolicyRecurrence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		frequency string
		days      []string
		wantError string
	}{
		{name: "all weekdays", frequency: "weekly", days: []string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"}},
		{name: "unsupported frequency", frequency: "daily", days: []string{"monday"}, wantError: "--frequency must be weekly"},
		{name: "empty frequency", days: []string{"monday"}, wantError: "--frequency must be weekly"},
		{name: "missing days", frequency: "weekly", wantError: "at least one --day is required"},
		{name: "abbreviated day", frequency: "weekly", days: []string{"mon"}, wantError: `invalid day "mon"`},
		{name: "uppercase day", frequency: "weekly", days: []string{"Monday"}, wantError: `invalid day "Monday"`},
		{name: "empty day", frequency: "weekly", days: []string{""}, wantError: `invalid day ""`},
		{name: "duplicate day", frequency: "weekly", days: []string{"monday", "friday", "monday"}, wantError: `duplicate day "monday"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := upgradePolicyOptions{startTime: "2026-10-10T02:00:00Z", durationSeconds: 14400, frequency: tc.frequency, days: tc.days}
			maintenance, err := opts.build()
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error = %v, want %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := maintenance.Window.Recurrence; got.Frequency != tc.frequency || strings.Join(got.DaysOfWeek, ",") != strings.Join(tc.days, ",") {
				t.Errorf("unexpected recurrence: %+v", got)
			}
		})
	}
}

func TestBuildUpgradePolicyRejectsInvalidDuration(t *testing.T) {
	for _, tc := range []struct {
		name     string
		duration int32
	}{
		{"zero", 0},
		{"below minimum", 59},
		{"above maximum", 86401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := upgradePolicyOptions{
				startTime:       "2026-10-03T02:00:00Z",
				durationSeconds: tc.duration,
				frequency:       "weekly",
				days:            []string{"saturday"},
			}
			if _, err := opts.build(); err == nil {
				t.Fatalf("expected duration %d to fail", tc.duration)
			}
		})
	}
}

func TestBuildUpgradePolicyRejectsInvalidExclusion(t *testing.T) {
	opts := upgradePolicyOptions{
		startTime:       "2026-10-03T02:00:00Z",
		durationSeconds: 14400,
		frequency:       "weekly",
		days:            []string{"saturday"},
		exclusions:      []string{"not-a-valid-exclusion"},
	}

	if _, err := opts.build(); err == nil {
		t.Fatal("expected invalid exclusion to fail")
	}
}

func TestPrintUpgradePolicyYAML(t *testing.T) {
	opts := &upgradePolicyOptions{
		startTime:       "2026-10-03T02:00:00Z",
		durationSeconds: 14400,
		frequency:       "weekly",
		days:            []string{"saturday"},
	}
	maintenance, err := opts.build()
	if err != nil {
		t.Fatalf("building policy: %v", err)
	}

	cluster := &gcpv1.Cluster{}
	cluster.Name = "upgrade-poc"
	cluster.APIVersion = gcpv1.GroupVersion.String()
	cluster.Kind = "Cluster"
	cluster.Spec.Maintenance = maintenance

	var buf bytes.Buffer
	if err := printUpgradePolicy(&buf, cluster, "yaml"); err != nil {
		t.Fatalf("printing policy: %v", err)
	}

	got := buf.String()
	// API-shaped JSON tag names must appear, not Go field names.
	for _, want := range []string{
		"apiVersion: gcp.managed.openshift.io/v1",
		"kind: Cluster",
		"durationSeconds: 14400",
		"daysOfWeek:",
		"startTime:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	// Go-derived field names must not appear.
	for _, unwanted := range []string{"typemeta:", "objectmeta:", "durationseconds:", "daysofweek:", "starttime:"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("output contains Go field name %q:\n%s", unwanted, got)
		}
	}
}

func TestPrintUpgradePolicyText(t *testing.T) {
	opts := &upgradePolicyOptions{
		startTime:       "2026-10-03T02:00:00Z",
		durationSeconds: 14400,
		frequency:       "weekly",
		days:            []string{"saturday"},
	}
	maintenance, err := opts.build()
	if err != nil {
		t.Fatalf("building policy: %v", err)
	}

	cluster := &gcpv1.Cluster{}
	cluster.Name = "upgrade-poc"
	cluster.Spec.Maintenance = maintenance

	var buf bytes.Buffer
	if err := printUpgradePolicy(&buf, cluster, "text"); err != nil {
		t.Fatalf("printing policy: %v", err)
	}

	got := buf.String()
	for _, want := range []string{
		"Cluster: upgrade-poc",
		"Start Time:",
		"Duration: 14400 seconds",
		"Frequency: weekly",
		"Days: saturday",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}
