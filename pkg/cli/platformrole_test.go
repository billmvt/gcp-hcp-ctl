package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openshift-online/gcp-hcp-ctl/pkg/internal/access"
	"github.com/openshift-online/gcp-hcp-ctl/pkg/platformapi"
)

// TestPlatformRoleRootConfiguration checks project-independent catalog requests with flag,
// environment and file configuration.
func TestPlatformRoleRootConfiguration(t *testing.T) {
	for _, source := range []string{"none", "flag", "environment", "config"} {
		for _, project := range []string{"project-a", "project-b"} {
			for _, verb := range []string{"list", "get"} {
				t.Run(source+project+verb, func(t *testing.T) {
					t.Setenv("GCPHCPCTL_PROJECT", "")
					t.Setenv("GCPHCPCTL_API_ENDPOINT", "")
					t.Setenv("GCPHCPCTL_REGION", "")
					t.Setenv("GCPHCPCTL_ENVIRONMENT", "")
					t.Setenv("GCPHCPCTL_OUTPUT", "")
					calls := 0
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls++
						want := "/apis/gcp.managed.openshift.io/v1/platformroles"
						if verb == "get" {
							want += "/future-role"
						}
						if r.Method != "GET" || r.URL.Path != want {
							t.Errorf("project changed catalog request: %s %s", r.Method, r.URL)
						}
						w.Header().Set("Content-Type", "application/json")
						if verb == "list" {
							fmt.Fprint(w, `{"apiVersion":"gcp.managed.openshift.io/v1","kind":"PlatformRoleList","items":[]}`)
						} else {
							fmt.Fprint(w, `{"apiVersion":"gcp.managed.openshift.io/v1","kind":"PlatformRole","metadata":{"name":"future-role"},"spec":{"permissions":["cluster.get"]}}`)
						}
					}))
					defer server.Close()
					path := filepath.Join(t.TempDir(), "config.yaml")
					data := "api_endpoint: " + server.URL + "\noutput: json\n"
					if source == "config" {
						data += "project: " + project + "\n"
					}
					if err := os.WriteFile(path, []byte(data), 0600); err != nil {
						t.Fatal(err)
					}
					if source == "environment" {
						t.Setenv("GCPHCPCTL_PROJECT", project)
						t.Setenv("GCPHCPCTL_API_ENDPOINT", server.URL)
					}
					root := newRootCmd()
					c, err := platformapi.NewClientForTest(server.URL, project)
					if err != nil {
						t.Fatal(err)
					}
					root.SetContext(access.WithClient(t.Context(), c))
					var out, stderr bytes.Buffer
					root.SetOut(&out)
					root.SetErr(&stderr)
					args := []string{"platformrole", verb, "--config", path}
					if verb == "get" {
						args = append(args, "future-role")
					}
					if source == "flag" {
						args = append(args, "--project", project, "--api-endpoint", server.URL)
					}
					root.SetArgs(args)
					if err := root.Execute(); err != nil {
						t.Fatal(err)
					}
					if calls != 1 || !strings.Contains(out.String(), "apiVersion") || stderr.Len() != 0 {
						t.Fatalf("calls=%d out=%s stderr=%s", calls, &out, &stderr)
					}
				})
			}
		}
	}
}

// TestPlatformRoleRootReadOnlyWiring checks catalog command registration and inherited
// output flags.
func TestPlatformRoleRootReadOnlyWiring(t *testing.T) {
	for _, verb := range []string{"list", "get"} {
		cmd, remaining, err := newRootCmd().Find([]string{"platformrole", verb})
		if err != nil || len(remaining) != 0 || cmd.Name() != verb || cmd.Parent().Name() != "platformrole" {
			t.Fatalf("%s: %v", verb, err)
		}
		if cmd.Flags().Lookup("output") != nil {
			t.Fatal("catalog command shadows inherited output")
		}
	}
}
