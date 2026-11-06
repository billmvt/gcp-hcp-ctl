package cluster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openshift-online/gcp-hcp-ctl/pkg/config"
	"github.com/openshift-online/gcp-hcp-ctl/pkg/platformapi"
	gcpv1 "github.com/openshift-online/gecko/platform-api/api/public/v1"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestClusterStatus(t *testing.T) {
	t.Run("When there are no conditions it should return Pending", func(t *testing.T) {
		c := &gcpv1.Cluster{
			Status: gcpv1.ClusterStatus{},
		}
		if got := clusterStatus(c); got != "Pending" {
			t.Errorf("expected 'Pending', got %q", got)
		}
	})

	t.Run("When HostedClusterAvailable is True it should return Ready", func(t *testing.T) {
		c := &gcpv1.Cluster{
			Status: gcpv1.ClusterStatus{
				Conditions: []metav1.Condition{
					{Type: "HostedClusterAvailable", Status: metav1.ConditionTrue},
				},
			},
		}
		if got := clusterStatus(c); got != "Ready" {
			t.Errorf("expected 'Ready', got %q", got)
		}
	})

	t.Run("When HostedClusterAvailable is absent it should return Progressing", func(t *testing.T) {
		c := &gcpv1.Cluster{
			Status: gcpv1.ClusterStatus{
				Conditions: []metav1.Condition{
					{Type: "SomeOtherCondition", Status: metav1.ConditionTrue},
				},
			},
		}
		if got := clusterStatus(c); got != "Progressing" {
			t.Errorf("expected 'Progressing', got %q", got)
		}
	})

	t.Run("When HostedClusterAvailable is False it should return Progressing", func(t *testing.T) {
		c := &gcpv1.Cluster{
			Status: gcpv1.ClusterStatus{
				Conditions: []metav1.Condition{
					{Type: "HostedClusterAvailable", Status: metav1.ConditionFalse, Reason: "NotAvailable"},
				},
			},
		}
		if got := clusterStatus(c); got != "Progressing" {
			t.Errorf("expected 'Progressing', got %q", got)
		}
	})

	t.Run("When DeletionTimestamp is set it should return Deleting", func(t *testing.T) {
		now := metav1.NewTime(time.Now())
		c := &gcpv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now},
			Status: gcpv1.ClusterStatus{
				Conditions: []metav1.Condition{
					{Type: "HostedClusterAvailable", Status: metav1.ConditionTrue},
				},
			},
		}
		if got := clusterStatus(c); got != "Deleting" {
			t.Errorf("expected 'Deleting', got %q", got)
		}
	})

	t.Run("When conditions exist but no HostedClusterAvailable it should return Progressing", func(t *testing.T) {
		c := &gcpv1.Cluster{
			Status: gcpv1.ClusterStatus{
				Conditions: []metav1.Condition{
					{Type: "SomeOtherCondition", Status: metav1.ConditionTrue},
				},
			},
		}
		if got := clusterStatus(c); got != "Progressing" {
			t.Errorf("expected 'Progressing', got %q", got)
		}
	})
}

func TestClusterStatusDetail(t *testing.T) {
	t.Run("When Ready it should return just Ready without parenthetical", func(t *testing.T) {
		c := &gcpv1.Cluster{
			Status: gcpv1.ClusterStatus{
				Conditions: []metav1.Condition{
					{Type: "HostedClusterAvailable", Status: metav1.ConditionTrue},
				},
			},
		}
		if got := clusterStatusDetail(c); got != "Ready" {
			t.Errorf("expected 'Ready', got %q", got)
		}
	})

	t.Run("When Pending it should return just Pending without parenthetical", func(t *testing.T) {
		c := &gcpv1.Cluster{
			Status: gcpv1.ClusterStatus{},
		}
		if got := clusterStatusDetail(c); got != "Pending" {
			t.Errorf("expected 'Pending', got %q", got)
		}
	})

	t.Run("When HostedClusterAvailable is False with message it should return Progressing with detail", func(t *testing.T) {
		c := &gcpv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Generation: 1},
			Status: gcpv1.ClusterStatus{
				Conditions: []metav1.Condition{
					{Type: "HostedClusterAvailable", Status: metav1.ConditionFalse, Reason: "NotAvailable", Message: "Waiting for controllers", ObservedGeneration: 1},
				},
			},
		}
		got := clusterStatusDetail(c)
		if got != "Progressing (Waiting for controllers)" {
			t.Errorf("expected 'Progressing (Waiting for controllers)', got %q", got)
		}
	})

	t.Run("When HostedClusterAvailable is False with reason but no message it should show reason", func(t *testing.T) {
		c := &gcpv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Generation: 1},
			Status: gcpv1.ClusterStatus{
				Conditions: []metav1.Condition{
					{Type: "HostedClusterAvailable", Status: metav1.ConditionFalse, Reason: "NotAvailable", ObservedGeneration: 1},
				},
			},
		}
		got := clusterStatusDetail(c)
		if got != "Progressing (NotAvailable)" {
			t.Errorf("expected 'Progressing (NotAvailable)', got %q", got)
		}
	})

	t.Run("When HostedClusterAvailable is False with reason equal to type and no message it should omit the parenthetical", func(t *testing.T) {
		// Mirrors gecko's actual behavior: hc_controller.go hard-codes
		// Reason to the literal condition Type ("HostedClusterAvailable")
		// and leaves Message empty, which previously rendered as the
		// useless "Progressing (HostedClusterAvailable)".
		c := &gcpv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Generation: 1},
			Status: gcpv1.ClusterStatus{
				Conditions: []metav1.Condition{
					{Type: "HostedClusterAvailable", Status: metav1.ConditionFalse, Reason: "HostedClusterAvailable", ObservedGeneration: 1},
				},
			},
		}
		got := clusterStatusDetail(c)
		if got != "Progressing" {
			t.Errorf("expected 'Progressing', got %q", got)
		}
	})

	t.Run("When HostedClusterAvailable is False and observed generation lags it should show controller reconciling detail", func(t *testing.T) {
		c := &gcpv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Generation: 2},
			Status: gcpv1.ClusterStatus{
				Conditions: []metav1.Condition{
					{Type: "HostedClusterAvailable", Status: metav1.ConditionFalse, Reason: "NotAvailable", Message: "old message", ObservedGeneration: 1},
				},
			},
		}
		got := clusterStatusDetail(c)
		if got != "Progressing (controller reconciling generation 2)" {
			t.Errorf("expected 'Progressing (controller reconciling generation 2)', got %q", got)
		}
	})

	t.Run("When Deleting it should return just Deleting without parenthetical", func(t *testing.T) {
		now := metav1.NewTime(time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC))
		c := &gcpv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now},
			Status:     gcpv1.ClusterStatus{},
		}
		got := clusterStatusDetail(c)
		if got != "Deleting" {
			t.Errorf("expected 'Deleting', got %q", got)
		}
	})
}

func TestReleaseVersion(t *testing.T) {
	t.Run("When release version is set it should return it", func(t *testing.T) {
		c := &gcpv1.Cluster{
			Spec: gcpv1.ClusterSpec{
				Release: gcpv1.ReleaseSpec{Version: "4.22.0"},
			},
		}
		if got := releaseVersion(c); got != "4.22.0" {
			t.Errorf("expected '4.22.0', got %q", got)
		}
	})

	t.Run("When version is empty it should return <none>", func(t *testing.T) {
		c := &gcpv1.Cluster{
			Spec: gcpv1.ClusterSpec{},
		}
		if got := releaseVersion(c); got != "<none>" {
			t.Errorf("expected '<none>', got %q", got)
		}
	})
}

func TestFindCondition(t *testing.T) {
	t.Run("When condition exists it should return a pointer to it", func(t *testing.T) {
		conditions := []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionTrue},
			{Type: "Available", Status: metav1.ConditionFalse},
		}
		got := meta.FindStatusCondition(conditions, "Available")
		if got == nil {
			t.Fatal("expected non-nil condition")
		}
		if got.Status != metav1.ConditionFalse {
			t.Errorf("expected False, got %q", got.Status)
		}
	})

	t.Run("When condition does not exist it should return nil", func(t *testing.T) {
		conditions := []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionTrue},
		}
		if got := meta.FindStatusCondition(conditions, "Missing"); got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})

	t.Run("When conditions list is empty it should return nil", func(t *testing.T) {
		if got := meta.FindStatusCondition(nil, "Ready"); got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})
}

const leakedResponse = `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","message":"Bearer synthetic-token","details":{"causes":[{"message":"Authorization: Bearer synthetic-token alice@example.invalid https://user:password@example.invalid/?token=synthetic-token"}]}}`

func clusterTestClient(t *testing.T, code int, body string) *platformapi.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	client, err := platformapi.NewClientForTest(server.URL, "my-project")
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func executeClusterTestCommand(t *testing.T, client *platformapi.Client, ctx context.Context, args ...string) (error, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	root := &cobra.Command{Use: "gcphcpctl", SilenceUsage: true}
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetContext(ctx)
	group := NewClusterCmd(&config.Config{})
	group.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		cmd.SetContext(context.WithValue(cmd.Context(), clientKey, client))
		return nil
	}
	root.AddCommand(group)
	root.SetArgs(append([]string{"cluster"}, args...))
	err := root.Execute()
	return err, stdout.String(), stderr.String()
}

func assertSafeClusterError(t *testing.T, err error, stdout, stderr, want string) {
	t.Helper()
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
	if stdout != "" {
		t.Errorf("unexpected stdout: %q", stdout)
	}
	if stderr != "Error: "+want+"\n" {
		t.Errorf("stderr = %q, want %q", stderr, "Error: "+want+"\n")
	}
	for _, s := range []string{"synthetic-token", "Authorization:", "alice@example.invalid", "user:password", "Platform API", "GET clusters", "HTTP 40"} {
		if strings.Contains(fmt.Sprintf("%+v %v %s", err, err, stderr), s) {
			t.Errorf("leaked %q", s)
		}
	}
}

func TestClusterCommandErrorBoundary(t *testing.T) {
	for _, tc := range []struct {
		name       string
		code       int
		body, want string
		args       []string
	}{
		{"list unauthorized", 401, leakedResponse, "listing clusters: not authenticated", []string{"list"}},
		{"list forbidden", 403, leakedResponse, "listing clusters: permission denied", []string{"list"}},
		{"set upgrade policy unauthorized", 401, leakedResponse, "setting upgrade policy: not authenticated", []string{"set-upgrade-policy", "test-cluster", "--start-time=2026-10-10T02:00:00Z", "--duration-seconds=14400", "--day=saturday"}},
		{"set upgrade policy forbidden", 403, leakedResponse, "setting upgrade policy: permission denied", []string{"set-upgrade-policy", "test-cluster", "--start-time=2026-10-10T02:00:00Z", "--duration-seconds=14400", "--day=saturday"}},
		{"get missing", 404, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound"}`, `looking up cluster "missing" in project "my-project": not found`, []string{"get", "missing"}},
		{"login missing", 404, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound"}`, `resolving cluster endpoint: looking up cluster "missing" in project "my-project": not found`, []string{"login", "missing"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := clusterTestClient(t, tc.code, tc.body)
			args := append([]string(nil), tc.args...)
			var kubeconfigPath string
			if tc.name == "login missing" {
				kubeconfigPath = filepath.Join(t.TempDir(), "must-not-exist")
				args = append(args, "--kubeconfig", kubeconfigPath)
			}
			err, out, stderr := executeClusterTestCommand(t, client, context.Background(), args...)
			assertSafeClusterError(t, err, out, stderr, tc.want)
			t.Logf("stderr: %q", stderr)
			if kubeconfigPath != "" {
				if _, statErr := os.Stat(kubeconfigPath); !os.IsNotExist(statErr) {
					t.Errorf("lookup failure wrote kubeconfig or stat failed: %v", statErr)
				}
			}
		})
	}
}

func TestClusterLoginInheritsExpiredParentDeadline(t *testing.T) {
	client := clusterTestClient(t, 200, `{}`)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	start := time.Now()
	kubeconfigPath := filepath.Join(t.TempDir(), "must-not-exist")
	err, out, stderr := executeClusterTestCommand(t, client, ctx, "login", "missing", "--kubeconfig", kubeconfigPath)
	assertSafeClusterError(t, err, out, stderr, `resolving cluster endpoint: looking up cluster "missing" in project "my-project": request timed out`)
	t.Logf("stderr: %q", stderr)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("deadline sentinel lost: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("login waited instead of honoring inherited deadline")
	}
	if _, statErr := os.Stat(kubeconfigPath); !os.IsNotExist(statErr) {
		t.Errorf("timeout wrote kubeconfig or stat failed: %v", statErr)
	}
}

func TestDecorateCreateError(t *testing.T) {
	client := clusterTestClient(t, 500, `{}`)
	_, uncertain := client.Clusters().Create(context.Background(), client.Namespace(), &gcpv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"}})
	if uncertain == nil || !platformapi.IsUncertainOutcome(uncertain) {
		t.Fatal("expected uncertain Platform API error")
	}
	certainClient := clusterTestClient(t, 403, leakedResponse)
	_, certain := certainClient.Clusters().Get(context.Background(), certainClient.Namespace(), "missing")
	for _, tc := range []struct {
		name        string
		err         error
		setup, note bool
	}{
		{"setup uncertain", uncertain, true, true},
		{"setup certain", certain, true, false},
		{"config uncertain", uncertain, false, false},
		{"success", nil, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := decorateCreateError(tc.err, tc.setup)
			if tc.err == nil {
				if got != nil {
					t.Errorf("nil became %v", got)
				}
				return
			}
			if !strings.HasPrefix(got.Error(), "creating cluster: ") {
				t.Errorf("missing context: %v", got)
			}
			if strings.Contains(got.Error(), "inspect the cluster and provisioned IAM/network resources") != tc.note {
				t.Errorf("note presence = %v: %v", tc.note, got)
			}
			if tc.err == uncertain && strings.Count(got.Error(), "; check the resource before retrying") != 1 {
				t.Errorf("generic advisory missing or duplicated: %v", got)
			}
			if platformapi.IsUncertainOutcome(got) != platformapi.IsUncertainOutcome(tc.err) {
				t.Errorf("predicate changed: %v", got)
			}
			var h *platformapi.HTTPError
			if !errors.As(got, &h) {
				t.Errorf("HTTP error inaccessible: %v", got)
			}
		})
	}
}
