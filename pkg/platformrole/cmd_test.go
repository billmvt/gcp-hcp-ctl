package platformrole

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/openshift-online/gcp-hcp-ctl/pkg/config"
	"github.com/openshift-online/gcp-hcp-ctl/pkg/discovery"
	"github.com/openshift-online/gcp-hcp-ctl/pkg/internal/access"
	"github.com/openshift-online/gcp-hcp-ctl/pkg/platformapi"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

type fakeResolver struct {
	calls int
	cfg   *config.Config
	err   error
}

// Region records discovery inputs and returns a synthetic endpoint or the configured error.
func (r *fakeResolver) Region(_ context.Context, cfg *config.Config) (discovery.RegionInfo, error) {
	r.calls++
	r.cfg = cfg
	return discovery.RegionInfo{PlatformAPIEndpoint: "https://api.example.com"}, r.err
}

// command creates an isolated command tree with a loopback catalog client and captured
// output.
func command(t *testing.T, server string, resolver regionResolver) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	root := &cobra.Command{Use: "test", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("project", "", "")
	root.PersistentFlags().String("api-endpoint", "", "")
	root.PersistentFlags().String("environment", "", "")
	root.PersistentFlags().String("region", "", "")
	root.PersistentFlags().StringP("output", "o", "text", "")
	root.AddCommand(newCommand(resolver))
	if server != "" {
		c, err := platformapi.NewClientForTest(server, "")
		if err != nil {
			t.Fatal(err)
		}
		root.SetContext(access.WithClient(t.Context(), c))
	}
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(&bytes.Buffer{})
	return root, out
}

// TestCommandCatalogOutput checks complete permissions and minimal metadata in text, JSON
// and YAML output.
func TestCommandCatalogOutput(t *testing.T) {
	role := `{"apiVersion":"gcp.managed.openshift.io/v1","kind":"PlatformRole","metadata":{"name":"future-role","creationTimestamp":"2026-01-01T00:00:00Z","namespace":"internal","resourceVersion":"99","uid":"secret-uid","labels":{"internal":"secret-label"},"annotations":{"argocd.argoproj.io/tracking-id":"secret-gitops"},"managedFields":[{"manager":"secret-manager"}],"finalizers":["secret-finalizer"],"ownerReferences":[{"apiVersion":"v1","kind":"Secret","name":"secret-owner","uid":"secret-owner"}]},"spec":{"permissions":["cluster.get","cluster.list","rolebinding.create"]}}`
	for _, verb := range []string{"list", "get"} {
		for _, format := range []string{"text", "json", "yaml"} {
			t.Run(verb+format, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					want := "/apis/gcp.managed.openshift.io/v1/platformroles"
					if verb == "get" {
						want += "/future-role"
					}
					if r.Method != "GET" || r.URL.Path != want {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					}
					w.Header().Set("Content-Type", "application/json")
					if verb == "list" {
						fmt.Fprintf(w, `{"apiVersion":"gcp.managed.openshift.io/v1","kind":"PlatformRoleList","items":[%s]}`, role)
					} else {
						fmt.Fprint(w, role)
					}
				}))
				defer server.Close()
				root, out := command(t, server.URL, &fakeResolver{})
				args := []string{"platformrole", verb, "--api-endpoint", server.URL, "-o", format}
				if verb == "get" {
					args = append(args, "future-role")
				}
				root.SetArgs(args)
				if err := root.Execute(); err != nil {
					t.Fatal(err)
				}
				result := out.String()
				for _, secret := range []string{"secret-", "managedFields", "Namespace:", "internal", "argocd"} {
					if strings.Contains(result, secret) {
						t.Fatalf("unnecessary metadata %q leaked: %s", secret, result)
					}
				}
				if format == "text" {
					if verb == "list" {
						for _, want := range []string{"NAME", "PERMISSIONS", "AGE", "future-role", "3"} {
							if !strings.Contains(result, want) {
								t.Errorf("missing %q: %s", want, result)
							}
						}
					} else {
						for _, want := range []string{"Name:", "Age:", "cluster.get", "cluster.list", "rolebinding.create"} {
							if !strings.Contains(result, want) {
								t.Errorf("missing %q: %s", want, result)
							}
						}
					}
					return
				}
				data := []byte(result)
				if format == "yaml" {
					var err error
					data, err = yaml.YAMLToJSON(data)
					if err != nil {
						t.Fatal(err)
					}
				}
				var object map[string]any
				if err := json.Unmarshal(data, &object); err != nil {
					t.Fatal(err)
				}
				if object["apiVersion"] != "gcp.managed.openshift.io/v1" {
					t.Fatal(object)
				}
				if verb == "list" {
					if object["kind"] != "PlatformRoleList" {
						t.Fatal(object)
					}
					object = object["items"].([]any)[0].(map[string]any)
				}
				if object["kind"] != "PlatformRole" {
					t.Fatal(object)
				}
				metadata := object["metadata"].(map[string]any)
				if len(metadata) != 2 || metadata["name"] != "future-role" || metadata["creationTimestamp"] != "2026-01-01T00:00:00Z" {
					t.Fatalf("metadata: %v", metadata)
				}
				if len(object["spec"].(map[string]any)["permissions"].([]any)) != 3 {
					t.Fatal(object)
				}
			})
		}
	}
}

// TestCommandDiscovery checks explicit-endpoint precedence and project-independent
// discovery failures.
func TestCommandDiscovery(t *testing.T) {
	for _, explicit := range []bool{true, false} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			resolver := &fakeResolver{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"apiVersion":"gcp.managed.openshift.io/v1","kind":"PlatformRoleList","items":[]}`)
			}))
			defer server.Close()
			root, _ := command(t, server.URL, resolver)
			args := []string{"platformrole", "list", "--environment", "int", "--region", "us-central1"}
			if explicit {
				resolver.err = errors.New("discovery unavailable")
				args = append(args, "--api-endpoint", "https://explicit.example.com")
			}
			root.SetArgs(args)
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if explicit {
				if resolver.calls != 0 {
					t.Fatal("explicit endpoint fetched discovery/OIDC")
				}
			} else if resolver.calls != 1 || resolver.cfg.Environment != "int" || resolver.cfg.Region != "us-central1" || resolver.cfg.Project != "" {
				t.Fatalf("incorrect discovery: %#v", resolver)
			}
		})
	}
	root, _ := command(t, "", &fakeResolver{err: errors.New("discovery unavailable")})
	root.SetArgs([]string{"platformrole", "list"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "discovery unavailable") {
		t.Fatalf("discovery error: %v", err)
	}
}

// TestCommandReadOnlyAndArgs rejects mutation commands and invalid catalog arguments.
func TestCommandReadOnlyAndArgs(t *testing.T) {
	group := NewPlatformRoleCmd()
	if len(group.Commands()) != 2 {
		t.Fatalf("unexpected commands: %v", group.Commands())
	}
	for _, args := range [][]string{{"platformrole", "get"}, {"platformrole", "get", ""}, {"platformrole", "get", "a", "b"}, {"platformrole", "list", "extra"}, {"platformrole", "create"}, {"platformrole", "delete", "a"}} {
		root, _ := command(t, "", &fakeResolver{})
		root.SetArgs(args)
		if err := root.Execute(); err == nil {
			t.Fatalf("accepted invalid args: %v", args)
		}
	}
}

// TestPrepareRoleDoesNotMutateResponse checks that output cleanup deep-copies permissions
// and metadata.
func TestPrepareRoleDoesNotMutateResponse(t *testing.T) {
	role := &platformapi.PlatformRole{ObjectMeta: metav1.ObjectMeta{Name: "reader", Annotations: map[string]string{"argocd": "internal"}}, Spec: platformapi.PlatformRoleSpec{Permissions: []string{"cluster.get"}}}
	before := role.DeepCopy()
	prepared := prepareRole(role)
	prepared.Spec.Permissions[0] = "changed"
	if !reflect.DeepEqual(role, before) {
		t.Fatal("output mutated original response")
	}
}

// TestEmptyStructuredCatalog checks that empty structured lists contain an array rather
// than null.
func TestEmptyStructuredCatalog(t *testing.T) {
	for _, format := range []string{"json", "yaml"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"apiVersion":"gcp.managed.openshift.io/v1","kind":"PlatformRoleList"}`)
		}))
		root, out := command(t, server.URL, &fakeResolver{})
		root.SetArgs([]string{"platformrole", "list", "--api-endpoint", server.URL, "-o", format})
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		server.Close()
		if !strings.Contains(out.String(), "[]") {
			t.Fatalf("empty items must be array: %s", out)
		}
	}
}

// TestCommandSafeErrors checks that catalog failures reveal fixed labels rather than server
// prose or grant advice.
func TestCommandSafeErrors(t *testing.T) {
	for _, verb := range []string{"list", "get"} {
		for status, label := range map[int]string{401: "not authenticated", 403: "permission denied", 404: "not found"} {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				fmt.Fprint(w, `{"message":"SECRET create a RoleBinding to fix Cedar"}`)
			}))
			root, out := command(t, server.URL, &fakeResolver{})
			args := []string{"platformrole", verb, "--api-endpoint", server.URL}
			if verb == "get" {
				args = append(args, "target")
			}
			root.SetArgs(args)
			err := root.Execute()
			server.Close()
			if err == nil || !strings.Contains(err.Error(), label) || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "RoleBinding") || strings.Contains(err.Error(), "Cedar") {
				t.Fatalf("unsafe command error: %v", err)
			}
			if out.Len() != 0 {
				t.Fatalf("error emitted successful output: %s", out)
			}
		}
	}
}

// TestListErrorHasOneOperationPrefix checks pagination and HTTP errors have one CLI prefix,
// no partial output and preserved status.
func TestListErrorHasOneOperationPrefix(t *testing.T) {
	for _, tc := range []struct {
		name      string
		wantError string
		wantCalls int
		status    int
	}{
		{name: "repeated token", wantError: "listing platform roles: repeated pagination token", wantCalls: 2},
		{name: "page limit", wantError: "listing platform roles: pagination limit exceeded", wantCalls: 100},
		{name: "permission denied", wantError: "listing platform roles: permission denied", wantCalls: 1, status: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				if tc.status != 0 || calls > tc.wantCalls {
					status := tc.status
					if status == 0 {
						t.Error("unexpected request beyond pagination bound")
						status = http.StatusInternalServerError
					}
					w.WriteHeader(status)
					return
				}
				token := "SECRET-repeated-token"
				if tc.name == "page limit" {
					token = fmt.Sprintf("SECRET-token-%d", calls)
				}
				page := platformapi.PlatformRoleList{ListMeta: metav1.ListMeta{Continue: token}}
				page.APIVersion = "gcp.managed.openshift.io/v1"
				page.Kind = "PlatformRoleList"
				if err := json.NewEncoder(w).Encode(page); err != nil {
					t.Errorf("encoding catalog page: %v", err)
				}
			}))
			defer server.Close()
			root, out := command(t, server.URL, &fakeResolver{})
			root.SetArgs([]string{"platformrole", "list", "--api-endpoint", server.URL})
			err := root.Execute()
			if err == nil || err.Error() != tc.wantError {
				t.Fatalf("error = %v, want %q", err, tc.wantError)
			}
			if tc.status != 0 {
				var httpErr *platformapi.HTTPError
				if !errors.As(err, &httpErr) || httpErr.StatusCode() != tc.status {
					t.Fatalf("HTTP error status not preserved: %v", err)
				}
			}
			if calls != tc.wantCalls || out.Len() != 0 {
				t.Fatalf("calls=%d, want %d; unexpected successful output=%q", calls, tc.wantCalls, out.String())
			}
		})
	}
}
