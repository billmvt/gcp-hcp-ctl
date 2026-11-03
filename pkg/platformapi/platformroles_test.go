package platformapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openshift-online/gcp-hcp-ctl/pkg/auth"
	"k8s.io/client-go/rest"
)

// TestPlatformRoleReadsAreRootScoped checks that project configuration never adds a
// namespace to catalog paths.
func TestPlatformRoleReadsAreRootScoped(t *testing.T) {
	for _, project := range []string{"", "project-a", "project-b"} {
		t.Run(project, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet || strings.Contains(r.URL.Path, "/namespaces/") {
					t.Errorf("unexpected scoped request: %s %s", r.Method, r.URL)
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/apis/gcp.managed.openshift.io/v1/platformroles":
					fmt.Fprint(w, `{"apiVersion":"gcp.managed.openshift.io/v1","kind":"PlatformRoleList","items":[{"metadata":{"name":"future-role"},"spec":{"permissions":["cluster.get","role.list"]}}]}`)
				case "/apis/gcp.managed.openshift.io/v1/platformroles/future-role":
					fmt.Fprint(w, `{"apiVersion":"gcp.managed.openshift.io/v1","kind":"PlatformRole","metadata":{"name":"future-role"},"spec":{"permissions":["cluster.get","role.list"]}}`)
				default:
					t.Errorf("unexpected URL: %s", r.URL)
				}
			}))
			defer server.Close()
			client, err := NewClientForTest(server.URL, project)
			if err != nil {
				t.Fatal(err)
			}
			list, err := client.PlatformRoles().List(t.Context())
			if err != nil || len(list.Items) != 1 || list.Kind != "PlatformRoleList" {
				t.Fatalf("list: %#v %v", list, err)
			}
			role, err := client.PlatformRoles().Get(t.Context(), "future-role")
			if err != nil || role.Kind != "PlatformRole" || len(role.Spec.Permissions) != 2 {
				t.Fatalf("get: %#v %v", role, err)
			}
			if calls != 2 {
				t.Fatalf("requests = %d", calls)
			}
		})
	}
}

// TestPlatformRolePagination checks page aggregation, empty lists, repeated tokens and
// later-page failures.
func TestPlatformRolePagination(t *testing.T) {
	for _, mode := range []string{"pages", "empty", "repeated", "failure"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls > 3 {
					t.Error("unbounded pagination")
					w.WriteHeader(500)
					return
				}
				if calls == 1 && r.URL.Query().Get("continue") != "" {
					t.Error("unexpected initial token")
				}
				if calls > 1 && r.URL.Query().Get("continue") != "next" {
					t.Errorf("continuation = %q", r.URL.Query().Get("continue"))
				}
				w.Header().Set("Content-Type", "application/json")
				if mode == "empty" {
					fmt.Fprint(w, `{"kind":"PlatformRoleList","apiVersion":"gcp.managed.openshift.io/v1"}`)
					return
				}
				if calls > 1 && mode == "failure" {
					w.WriteHeader(http.StatusForbidden)
					fmt.Fprint(w, `{"message":"SECRET"}`)
					return
				}
				token := "next"
				if calls > 1 && mode == "pages" {
					token = ""
				}
				fmt.Fprintf(w, `{"kind":"PlatformRoleList","apiVersion":"gcp.managed.openshift.io/v1","metadata":{"resourceVersion":"123","continue":%q,"remainingItemCount":1},"items":[{"metadata":{"name":"role-%d"},"spec":{"permissions":["cluster.get"]}}]}`, token, calls)
			}))
			defer server.Close()
			c, err := NewClientForTest(server.URL, "ignored")
			if err != nil {
				t.Fatal(err)
			}
			list, err := c.PlatformRoles().List(t.Context())
			switch mode {
			case "pages":
				if err != nil || len(list.Items) != 2 || list.Continue != "" || list.RemainingItemCount != nil || list.ResourceVersion != "123" {
					t.Fatalf("pagination: %#v %v", list, err)
				}
			case "empty":
				if err != nil || list.Items == nil || len(list.Items) != 0 {
					t.Fatalf("empty list: %#v %v", list, err)
				}
			case "repeated":
				if err == nil || err.Error() != "repeated pagination token" || calls != 2 {
					t.Fatalf("loop protection: calls=%d %v", calls, err)
				}
			case "failure":
				if err == nil || list != nil || !strings.Contains(err.Error(), "permission denied") || strings.Contains(err.Error(), "SECRET") {
					t.Fatalf("page error: %#v %v", list, err)
				}
			}
		})
	}
}

// TestPlatformRoleSafeFailures checks fixed diagnostics and HTTP status preservation for
// hostile responses.
func TestPlatformRoleSafeFailures(t *testing.T) {
	for _, get := range []bool{false, true} {
		for status, label := range map[int]string{401: "not authenticated", 403: "permission denied", 404: "not found", 200: "invalid response"} {
			for _, body := range []string{`{"kind":"Status","apiVersion":"v1","message":"SECRET create a RoleBinding"}`, `<html>SECRET</html>`, `{"kind":"Cluster","apiVersion":"gcp.managed.openshift.io/v1"}`} {
				t.Run(fmt.Sprintf("get=%t/status=%d/%s", get, status, body), func(t *testing.T) {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(status)
						fmt.Fprint(w, body)
					}))
					defer server.Close()
					c, err := NewClientForTest(server.URL, "")
					if err != nil {
						t.Fatal(err)
					}
					if get {
						_, err = c.PlatformRoles().Get(t.Context(), "target")
					} else {
						_, err = c.PlatformRoles().List(t.Context())
					}
					if err == nil || !strings.Contains(err.Error(), label) || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "RoleBinding") {
						t.Fatalf("unsafe failure: %v", err)
					}
					if status != 200 {
						var httpErr *HTTPError
						if !errors.As(err, &httpErr) || httpErr.StatusCode() != status {
							t.Fatalf("missing HTTP status: %v", err)
						}
					}
				})
			}
		}
	}
}

// TestPlatformRoleCancellation checks that cancelled catalog reads do not send HTTP
// requests.
func TestPlatformRoleCancellation(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer server.Close()
	c, err := NewClientForTest(server.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.PlatformRoles().List(ctx); err == nil {
		t.Fatal("list ignored cancellation")
	}
	if _, err := c.PlatformRoles().Get(ctx, "target"); err == nil {
		t.Fatal("get ignored cancellation")
	}
	if calls != 0 {
		t.Fatalf("cancelled requests sent: %d", calls)
	}
}

// TestPlatformRoleConstructor checks authentication and HTTPS requirements without
// weakening namespaced clients.
func TestPlatformRoleConstructor(t *testing.T) {
	token := auth.NewTokenSource(auth.PlatformAPIAudience)
	if _, err := NewPlatformRoleClient("https://api.example.com", token); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPlatformRoleClient("http://api.example.com", token); err == nil {
		t.Fatal("accepted insecure endpoint")
	}
	if _, err := NewPlatformRoleClient("https://api.example.com", nil); err == nil {
		t.Fatal("accepted missing authentication")
	}
	if _, err := NewClient("https://api.example.com", "", token); err == nil {
		t.Fatal("namespaced client no longer requires project")
	}
}

// TestPlatformRolePaginationLimit checks completion at the page ceiling and safe failure
// without a request beyond it.
func TestPlatformRolePaginationLimit(t *testing.T) {
	// Keep this boundary aligned with the documented 100-page catalog limit.
	const pageLimit = 100
	for _, complete := range []bool{true, false} {
		t.Run(fmt.Sprintf("complete=%t", complete), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls > pageLimit {
					t.Error("client exceeded pagination request limit")
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				if r.Method != http.MethodGet || r.URL.Path != "/apis/gcp.managed.openshift.io/v1/platformroles" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				want := ""
				if calls > 1 {
					want = fmt.Sprintf("SECRET-token-%d", calls-1)
				}
				if got := r.URL.Query().Get("continue"); got != want {
					t.Errorf("continuation = %q, want %q", got, want)
				}
				token := fmt.Sprintf("SECRET-token-%d", calls)
				if complete && calls == pageLimit {
					token = ""
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"apiVersion":"gcp.managed.openshift.io/v1","kind":"PlatformRoleList","metadata":{"continue":%q,"remainingItemCount":1},"items":[{"metadata":{"name":"role-%d"},"spec":{"permissions":["cluster.get"]}}]}`, token, calls)
			}))
			defer server.Close()
			// Exercise the page boundary without spending time on client-side
			// QPS throttling against this local test server.
			config := clientRESTConfig(server.URL)
			config.QPS = -1
			restClient, err := rest.RESTClientFor(config)
			if err != nil {
				t.Fatal(err)
			}
			c := &Client{restClient: restClient}
			list, err := c.PlatformRoles().List(t.Context())
			if complete {
				if err != nil {
					t.Fatal(err)
				}
				if len(list.Items) != pageLimit || list.Items[0].Name != "role-1" || list.Items[pageLimit-1].Name != "role-100" || list.Continue != "" || list.RemainingItemCount != nil {
					t.Fatalf("incomplete boundary result: %#v", list)
				}
			} else {
				if list != nil || err == nil || err.Error() != "pagination limit exceeded" {
					t.Fatalf("expected fixed limit error without partial data, got list=%#v err=%v", list, err)
				}
				if strings.Contains(err.Error(), "SECRET") {
					t.Fatal("pagination token leaked")
				}
			}
			if calls != pageLimit {
				t.Errorf("requests=%d, want %d", calls, pageLimit)
			}
		})
	}
}
