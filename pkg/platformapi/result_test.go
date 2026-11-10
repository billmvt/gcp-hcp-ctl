package platformapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	gcpv1 "github.com/openshift-online/gecko/platform-api/api/public/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

const (
	clusterJSON      = `{"apiVersion":"gcp.managed.openshift.io/v1","kind":"Cluster","metadata":{"name":"test-cluster"}}`
	nodePoolJSON     = `{"apiVersion":"gcp.managed.openshift.io/v1","kind":"NodePool","metadata":{"name":"test-pool"}}`
	versionJSON      = `{"apiVersion":"gcp.managed.openshift.io/v1","kind":"Version","metadata":{"name":"4.22.13"},"spec":{"channelGroups":["stable"]}}`
	clusterListJSON  = `{"apiVersion":"gcp.managed.openshift.io/v1","kind":"ClusterList","items":[` + clusterJSON + `]}`
	nodePoolListJSON = `{"apiVersion":"gcp.managed.openshift.io/v1","kind":"NodePoolList","items":[` + nodePoolJSON + `]}`
)

// testRESTClient uses the real client-go request/result/decoder path without
// credentials. Callers supply response shapes through the handler.
func testRESTClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := NewClientForTest(server.URL, "test-project")
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func testRESTClientForConfig(t *testing.T, config *rest.Config) *Client {
	t.Helper()
	restClient, err := rest.RESTClientFor(config)
	if err != nil {
		t.Fatalf("creating REST client: %v", err)
	}
	return &Client{restClient: restClient, project: "test-project"}
}

func assertHTTPError(t *testing.T, err error, label string, code int, method, resource, name string, uncertain bool) {
	t.Helper()
	if err == nil || err.Error() != label {
		t.Fatalf("error = %v, want %q", err, label)
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("%T is not an HTTPError", err)
	}
	if httpErr.StatusCode() != code || httpErr.Method() != method || httpErr.Resource() != resource || httpErr.Name() != name {
		t.Errorf("metadata = (%d, %s, %s, %s), want (%d, %s, %s, %s)",
			httpErr.StatusCode(), httpErr.Method(), httpErr.Resource(), httpErr.Name(), code, method, resource, name)
	}
	if IsUncertainOutcome(err) != uncertain {
		t.Errorf("IsUncertainOutcome(%v) = %v, want %v", err, IsUncertainOutcome(err), uncertain)
	}
}

func TestLiveHTTPClassification(t *testing.T) {
	tests := []struct {
		name, contentType, body, want string
		code                          int
		call                          func(*Client) error
		method, resource, target      string
		transport                     bool
	}{
		{"cluster 401", "application/json", `{"error":"unauthorized","message":"secret"}`, "not authenticated", 401,
			func(c *Client) error {
				_, err := c.Clusters().Get(context.Background(), "test-project", "test-cluster")
				return err
			}, "GET", "clusters", "test-cluster", false},
		{"ESPv2-style 401", "application/json", `{"code":401,"message":"Jwt is missing"}`, "not authenticated", 401,
			func(c *Client) error { _, err := c.Clusters().List(context.Background(), "test-project"); return err }, "GET", "clusters", "", false},
		{"nodepool 403 generic", "application/json", `{"error":"forbidden"}`, "permission denied", 403,
			func(c *Client) error {
				_, err := c.NodePools().Get(context.Background(), "test-project", "test-pool")
				return err
			}, "GET", "nodepools", "test-pool", false},
		{"cluster 403 Status", "application/json", `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","message":"secret"}`, "permission denied", 403,
			func(c *Client) error { _, err := c.Clusters().List(context.Background(), "test-project"); return err }, "GET", "clusters", "", false},
		{"version 401 Status", "application/json", `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Unauthorized","message":"private diagnostic"}`, "not authenticated", 401,
			func(c *Client) error { _, err := c.Versions().Get(context.Background(), "4.22.13"); return err }, "GET", "versions", "4.22.13", false},
		{"version 403 Status", "application/json", `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","message":"private diagnostic"}`, "permission denied", 403,
			func(c *Client) error { _, err := c.Versions().Get(context.Background(), "4.22.13"); return err }, "GET", "versions", "4.22.13", false},
		{"version 404 Status", "application/json", `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","message":"private diagnostic"}`, "not found", 404,
			func(c *Client) error { _, err := c.Versions().Get(context.Background(), "4.22.13"); return err }, "GET", "versions", "4.22.13", false},
		{"malformed JSON retains status", "application/json", `{"error":`, "permission denied", 403,
			func(c *Client) error { _, err := c.NodePools().List(context.Background(), "test-project"); return err }, "GET", "nodepools", "", false},
		{"unsupported content type loses status", "application/xml", `<error>secret</error>`, "request failed", 403,
			func(c *Client) error {
				_, err := c.Clusters().Get(context.Background(), "test-project", "test-cluster")
				return err
			}, "GET", "clusters", "test-cluster", true},
		{"malformed content type loses status", "application/json; =bad", `{"error":"forbidden"}`, "request failed", 403,
			func(c *Client) error {
				_, err := c.Clusters().Get(context.Background(), "test-project", "test-cluster")
				return err
			}, "GET", "clusters", "test-cluster", true},
		{"delete 404", "application/json", `{"error":"not_found"}`, "not found", 404,
			func(c *Client) error { return c.NodePools().Delete(context.Background(), "test-project", "test-pool") }, "DELETE", "nodepools", "test-pool", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := testRESTClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != tt.method || !strings.Contains(r.URL.Path, "/"+tt.resource) {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", tt.contentType)
				w.WriteHeader(tt.code)
				_, _ = w.Write([]byte(tt.body))
			})
			err := tt.call(client)
			if tt.transport {
				if err == nil || err.Error() != tt.want {
					t.Fatalf("error = %v, want %q", err, tt.want)
				}
				var transportErr *TransportError
				var httpErr *HTTPError
				if !errors.As(err, &transportErr) || errors.As(err, &httpErr) {
					t.Fatalf("error type = %T, want only TransportError", err)
				}
				if IsUncertainOutcome(err) {
					t.Fatal("read/delete must not be uncertain")
				}
				return
			}
			assertHTTPError(t, err, tt.want, tt.code, tt.method, tt.resource, tt.target, false)
		})
	}
}

func TestVersionGetSuccess(t *testing.T) {
	client := testRESTClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/versions/4.22.13") {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(versionJSON))
	})
	version, err := client.Versions().Get(context.Background(), "4.22.13")
	if err != nil {
		t.Fatalf("getting version: %v", err)
	}
	if version.Name != "4.22.13" || len(version.Spec.ChannelGroups) != 1 || version.Spec.ChannelGroups[0] != "stable" {
		t.Errorf("unexpected version: %#v", version)
	}
}

func TestSuccessfulStatusDecodeFailures(t *testing.T) {
	tests := []struct {
		name, contentType, body, want string
		call                          func(*Client) error
		uncertain                     bool
	}{
		{"GET malformed", "application/json", `{"kind":"Cluster",`, "invalid response",
			func(c *Client) error {
				obj, err := c.Clusters().Get(context.Background(), "test-project", "test-cluster")
				if obj != nil {
					t.Error("returned partially decoded object")
				}
				return err
			}, false},
		{"GET empty", "application/json", "", "invalid response",
			func(c *Client) error {
				_, err := c.Clusters().Get(context.Background(), "test-project", "test-cluster")
				return err
			}, false},
		{"GET unexpected kind", "application/json", nodePoolJSON, "invalid response",
			func(c *Client) error {
				_, err := c.Clusters().Get(context.Background(), "test-project", "test-cluster")
				return err
			}, false},
		{"GET unsupported success content type", "application/xml", `<cluster>secret</cluster>`, "invalid response",
			func(c *Client) error {
				_, err := c.Clusters().Get(context.Background(), "test-project", "test-cluster")
				return err
			}, false},
		{"GET failure Status in 2xx", "application/json", `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"secret"}`, "invalid response",
			func(c *Client) error {
				_, err := c.Clusters().Get(context.Background(), "test-project", "test-cluster")
				return err
			}, false},
		{"cluster POST truncated", "application/json", `{"kind":"Cluster","metadata":`, "server reported success, but its response was invalid" + checkBeforeRetrying,
			func(c *Client) error {
				obj, err := c.Clusters().Create(context.Background(), "test-project", &gcpv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"}})
				if obj != nil {
					t.Error("returned partially decoded object")
				}
				return err
			}, true},
		{"nodepool POST malformed", "application/json", `{nope`, "server reported success, but its response was invalid" + checkBeforeRetrying,
			func(c *Client) error {
				_, err := c.NodePools().Create(context.Background(), "test-project", &gcpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "test-pool"}})
				return err
			}, true},
		{"nodepool PATCH malformed", "application/json", `{nope`, "server reported success, but its response was invalid",
			func(c *Client) error {
				_, err := c.NodePools().Patch(context.Background(), "test-project", "test-pool", []byte(`{"spec":{"nodeCount":2}}`))
				return err
			}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			client := testRESTClient(t, func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", tt.contentType)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tt.body))
			})
			err := tt.call(client)
			if err == nil || err.Error() != tt.want {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			var decodeErr *responseDecodeError
			var httpErr *HTTPError
			if !errors.As(err, &decodeErr) || errors.As(err, &httpErr) || IsUncertainOutcome(err) != tt.uncertain {
				t.Fatalf("decode error classification = %T, uncertain=%v", err, IsUncertainOutcome(err))
			}
			if requests.Load() != 1 {
				t.Errorf("requests = %d, want 1", requests.Load())
			}
		})
	}
}

func TestUncertainWriteOutcomes(t *testing.T) {
	tests := []struct {
		name, method, body, want string
		code                     int
		uncertain                bool
	}{
		{"POST 500", "POST", `{}`, "server error" + checkBeforeRetrying, 500, true},
		{"POST 409", "POST", `{"error":"already_exists"}`, "already exists" + checkBeforeRetrying, 409, true},
		{"POST 503", "POST", `{}`, "service unavailable" + checkBeforeRetrying, 503, true},
		{"POST 429", "POST", `{}`, "too many requests", 429, false},
		{"PATCH 500", "PATCH", `{}`, "server error", 500, false},
		{"PATCH 409", "PATCH", `{}`, "conflict", 409, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			client := testRESTClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != tt.method {
					t.Errorf("method = %s, want %s", r.Method, tt.method)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "0") // client-go would otherwise retry writes.
				w.WriteHeader(tt.code)
				_, _ = w.Write([]byte(tt.body))
			})
			var err error
			if tt.method == "POST" {
				_, err = client.Clusters().Create(context.Background(), "test-project", &gcpv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"}})
			} else {
				_, err = client.NodePools().Patch(context.Background(), "test-project", "test-pool", []byte(`{}`))
			}
			resource, name := "clusters", "test-cluster"
			if tt.method == "PATCH" {
				resource, name = "nodepools", "test-pool"
			}
			assertHTTPError(t, err, tt.want, tt.code, tt.method, resource, name, tt.uncertain)
			if requests.Load() != 1 {
				t.Errorf("requests = %d, want 1", requests.Load())
			}
		})
	}
}

type failingTransport struct {
	requests *atomic.Int32
	err      error
}

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.requests.Add(1)
	return nil, f.err
}

func TestTransportFailures(t *testing.T) {
	tests := []struct {
		name, method, want string
		cause              error
		uncertain          bool
	}{
		{"POST deadline", "POST", "request timed out" + checkBeforeRetrying, context.DeadlineExceeded, true},
		{"POST token failure", "POST", "request failed" + checkBeforeRetrying, errors.New("token source secret https://example.invalid/?token=secret"), true},
		{"PATCH deadline", "PATCH", "request timed out", context.DeadlineExceeded, false},
		{"GET network failure", "GET", "request failed", errors.New("secret URL"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			client := testRESTClientForConfig(t, &rest.Config{
				Host: "https://example.invalid", APIPath: "/apis",
				ContentConfig: rest.ContentConfig{GroupVersion: &gcpv1.GroupVersion, NegotiatedSerializer: codecs.WithoutConversion()},
				Transport:     failingTransport{requests: &requests, err: tt.cause},
			})
			var err error
			switch tt.method {
			case "POST":
				_, err = client.Clusters().Create(context.Background(), "test-project", &gcpv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"}})
			case "PATCH":
				_, err = client.NodePools().Patch(context.Background(), "test-project", "test-pool", []byte(`{}`))
			case "GET":
				_, err = client.Clusters().List(context.Background(), "test-project")
			}
			var transportErr *TransportError
			var httpErr *HTTPError
			if err == nil || err.Error() != tt.want || !errors.As(err, &transportErr) || errors.As(err, &httpErr) {
				t.Fatalf("error = %v (%T), want transport %q", err, err, tt.want)
			}
			if IsUncertainOutcome(err) != tt.uncertain {
				t.Errorf("uncertain = %v, want %v", IsUncertainOutcome(err), tt.uncertain)
			}
			if errors.Is(tt.cause, context.DeadlineExceeded) && !errors.Is(err, context.DeadlineExceeded) {
				t.Error("deadline sentinel lost through wrapper")
			}
			if requests.Load() != 1 {
				t.Errorf("requests = %d, want 1", requests.Load())
			}
		})
	}
}

func TestSuccessfulMethodsAndPUTHelper(t *testing.T) {
	var requests atomic.Int32
	client := testRESTClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(r.URL.Path, "/namespaces/test-project/") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var body string
		switch {
		case strings.HasSuffix(r.URL.Path, "/clusters") && r.Method == http.MethodGet:
			body = clusterListJSON
		case strings.HasSuffix(r.URL.Path, "/nodepools") && r.Method == http.MethodGet:
			body = nodePoolListJSON
		case strings.Contains(r.URL.Path, "/clusters"):
			body = clusterJSON
		case strings.Contains(r.URL.Path, "/nodepools"):
			body = nodePoolJSON
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(body))
	})
	ctx := context.Background()
	cluster, err := client.Clusters().Create(ctx, "test-project", &gcpv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"}})
	if err != nil || cluster == nil || cluster.Name != "test-cluster" {
		t.Fatalf("cluster create = %v, %v", cluster, err)
	}
	cluster, err = client.Clusters().Get(ctx, "test-project", "test-cluster")
	if err != nil || cluster == nil || cluster.Name != "test-cluster" {
		t.Fatalf("cluster get = %v, %v", cluster, err)
	}
	clusters, err := client.Clusters().List(ctx, "test-project")
	if err != nil || clusters == nil || len(clusters.Items) != 1 {
		t.Fatalf("cluster list = %v, %v", clusters, err)
	}
	if err = client.Clusters().Delete(ctx, "test-project", "test-cluster"); err != nil {
		t.Fatalf("cluster delete: %v", err)
	}
	pool, err := client.NodePools().Create(ctx, "test-project", &gcpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "test-pool"}})
	if err != nil || pool == nil || pool.Name != "test-pool" {
		t.Fatalf("nodepool create = %v, %v", pool, err)
	}
	pool, err = client.NodePools().Get(ctx, "test-project", "test-pool")
	if err != nil || pool == nil || pool.Name != "test-pool" {
		t.Fatalf("nodepool get = %v, %v", pool, err)
	}
	pools, err := client.NodePools().List(ctx, "test-project")
	if err != nil || pools == nil || len(pools.Items) != 1 {
		t.Fatalf("nodepool list = %v, %v", pools, err)
	}
	pool, err = client.NodePools().Patch(ctx, "test-project", "test-pool", []byte(`{}`))
	if err != nil || pool == nil || pool.Name != "test-pool" {
		t.Fatalf("nodepool patch = %v, %v", pool, err)
	}
	if err = client.NodePools().Delete(ctx, "test-project", "test-pool"); err != nil {
		t.Fatalf("nodepool delete: %v", err)
	}
	if requests.Load() != 9 {
		t.Errorf("requests = %d, want 9", requests.Load())
	}

	// No production PUT exists yet, but the common boundary accepts it.
	response := client.restClient.Put().Namespace("test-project").Resource("clusters").Name("test-cluster").Body(&gcpv1.Cluster{}).Do(ctx)
	if err := normalizeResult(response, http.MethodPut, "clusters", "test-cluster", false); err != nil {
		t.Errorf("PUT helper: %v", err)
	}
	if requests.Load() != 10 {
		t.Errorf("requests = %d, want 10", requests.Load())
	}
}

func TestUncertainPredicateThroughCommandWrapper(t *testing.T) {
	wrapped := fmt.Errorf("creating cluster: %w", &uncertainOutcomeError{cause: newTransportError(context.DeadlineExceeded)})
	if !IsUncertainOutcome(wrapped) {
		t.Fatal("predicate lost through command wrapper")
	}
	if IsUncertainOutcome(fmt.Errorf("listing clusters: %w", newTransportError(context.DeadlineExceeded))) {
		t.Fatal("read failure marked uncertain")
	}
}

func TestPUTHelperFailureIsIdempotent(t *testing.T) {
	var requests atomic.Int32
	client := testRESTClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal_error"}`))
	})
	response := client.restClient.Put().MaxRetries(0).Namespace("test-project").Resource("clusters").Name("test-cluster").Body(&gcpv1.Cluster{}).Do(context.Background())
	err := normalizeResult(response, http.MethodPut, "clusters", "test-cluster", false)
	assertHTTPError(t, err, "server error", 500, http.MethodPut, "clusters", "test-cluster", false)
	if requests.Load() != 1 {
		t.Errorf("requests = %d, want 1", requests.Load())
	}
}

// Every production REST method must reach the same typed error boundary. The
// complementary response-shape matrix below deliberately varies the methods.
func TestAllLiveMethodsNormalizeFailure(t *testing.T) {
	tests := []struct {
		name, method, resource, target string
		call                           func(*testing.T, *Client) error
	}{
		{"cluster create", "POST", "clusters", "test-cluster", func(t *testing.T, c *Client) error {
			obj, err := c.Clusters().Create(context.Background(), c.Namespace(), &gcpv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"}})
			if obj != nil {
				t.Error("failed create returned object")
			}
			return err
		}},
		{"cluster get", "GET", "clusters", "test-cluster", func(t *testing.T, c *Client) error {
			obj, err := c.Clusters().Get(context.Background(), c.Namespace(), "test-cluster")
			if obj != nil {
				t.Error("failed get returned object")
			}
			return err
		}},
		{"cluster list", "GET", "clusters", "", func(t *testing.T, c *Client) error {
			obj, err := c.Clusters().List(context.Background(), c.Namespace())
			if obj != nil {
				t.Error("failed list returned object")
			}
			return err
		}},
		{"cluster delete", "DELETE", "clusters", "test-cluster", func(_ *testing.T, c *Client) error {
			return c.Clusters().Delete(context.Background(), c.Namespace(), "test-cluster")
		}},
		{"nodepool create", "POST", "nodepools", "test-pool", func(t *testing.T, c *Client) error {
			obj, err := c.NodePools().Create(context.Background(), c.Namespace(), &gcpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "test-pool"}})
			if obj != nil {
				t.Error("failed create returned object")
			}
			return err
		}},
		{"nodepool get", "GET", "nodepools", "test-pool", func(t *testing.T, c *Client) error {
			obj, err := c.NodePools().Get(context.Background(), c.Namespace(), "test-pool")
			if obj != nil {
				t.Error("failed get returned object")
			}
			return err
		}},
		{"nodepool list", "GET", "nodepools", "", func(t *testing.T, c *Client) error {
			obj, err := c.NodePools().List(context.Background(), c.Namespace())
			if obj != nil {
				t.Error("failed list returned object")
			}
			return err
		}},
		{"nodepool patch", "PATCH", "nodepools", "test-pool", func(t *testing.T, c *Client) error {
			obj, err := c.NodePools().Patch(context.Background(), c.Namespace(), "test-pool", []byte(`{}`))
			if obj != nil {
				t.Error("failed patch returned object")
			}
			return err
		}},
		{"nodepool delete", "DELETE", "nodepools", "test-pool", func(_ *testing.T, c *Client) error {
			return c.NodePools().Delete(context.Background(), c.Namespace(), "test-pool")
		}},
		{"cluster patch", "PATCH", "clusters", "test-cluster", func(t *testing.T, c *Client) error {
			obj, err := c.Clusters().Patch(context.Background(), c.Namespace(), "test-cluster", []byte(`{}`))
			if obj != nil {
				t.Error("failed patch returned object")
			}
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			client := testRESTClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != tt.method || !strings.Contains(r.URL.Path, "/"+tt.resource) {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"forbidden"}`))
			})
			err := fmt.Errorf("command context: %w", tt.call(t, client))
			var got *HTTPError
			if !errors.As(err, &got) || got.StatusCode() != 403 || got.Method() != tt.method || got.Resource() != tt.resource || got.Name() != tt.target || err.Error() != "command context: permission denied" || IsUncertainOutcome(err) {
				t.Fatalf("classification = %v (%T), metadata = %+v", err, err, got)
			}
			if requests.Load() != 1 {
				t.Errorf("requests = %d, want one", requests.Load())
			}
		})
	}
}

type failingTokenSource struct{ err error }

func (f failingTokenSource) Token(context.Context) (string, string, error) { return "", "", f.err }

type countingTransport struct{ requests *atomic.Int32 }

func (c countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.requests.Add(1)
	return nil, errors.New("unexpected outbound request")
}

func TestPreRequestTokenFailureHasNoHTTPAttempt(t *testing.T) {
	var requests atomic.Int32
	cfg := clientRESTConfig("https://example.invalid")
	cfg.Transport = countingTransport{requests: &requests}
	cfg.WrapTransport = func(base http.RoundTripper) http.RoundTripper {
		return &tokenTransport{base: base, tokenSource: failingTokenSource{err: errors.New("token source synthetic-token https://user:password@example.invalid")}}
	}
	client := testRESTClientForConfig(t, cfg)
	_, err := client.Clusters().Get(context.Background(), client.Namespace(), "test-cluster")
	var transport *TransportError
	var httpErr *HTTPError
	if err == nil || err.Error() != "request failed" || !errors.As(err, &transport) || errors.As(err, &httpErr) || requests.Load() != 0 {
		t.Fatalf("token failure = %v, base requests = %d", err, requests.Load())
	}
}

func TestTestClientRejectsNonlocalEndpoint(t *testing.T) {
	for _, endpoint := range []string{"http://example.invalid", "https://127.0.0.1:1234", "http://user:password@example.invalid"} {
		if client, err := NewClientForTest(endpoint, "test-project"); err == nil || client != nil || strings.Contains(err.Error(), "password") {
			t.Errorf("endpoint %q: client = %v, error = %v", endpoint, client, err)
		}
	}
}

func TestBothCreatesTransportFailures(t *testing.T) {
	for _, resource := range []string{"clusters", "nodepools"} {
		t.Run(resource, func(t *testing.T) {
			var requests atomic.Int32
			client := testRESTClientForConfig(t, &rest.Config{
				Host: "https://example.invalid", APIPath: "/apis",
				ContentConfig: clientRESTConfig("https://example.invalid").ContentConfig,
				Transport:     failingTransport{requests: &requests, err: errors.New("synthetic-token URL https://user:password@example.invalid")},
			})
			var err error
			if resource == "clusters" {
				_, err = client.Clusters().Create(context.Background(), client.Namespace(), &gcpv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"}})
			} else {
				_, err = client.NodePools().Create(context.Background(), client.Namespace(), &gcpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "test-pool"}})
			}
			var transport *TransportError
			var httpErr *HTTPError
			if err == nil || err.Error() != "request failed"+checkBeforeRetrying || !errors.As(err, &transport) || errors.As(err, &httpErr) || !IsUncertainOutcome(err) || requests.Load() != 1 {
				t.Fatalf("transport result = %v, attempts = %d", err, requests.Load())
			}
		})
	}
}

func TestCanceledRequestAndInvalidSuccessBodyAreSafe(t *testing.T) {
	client := testRESTClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"Cluster","metadata":` + `"Bearer synthetic-token"`))
	})
	_, err := client.Clusters().Get(context.Background(), client.Namespace(), "test-cluster")
	var decode *responseDecodeError
	var httpErr *HTTPError
	if err == nil || err.Error() != "invalid response" || !errors.As(err, &decode) || errors.As(err, &httpErr) {
		t.Fatalf("invalid 2xx result = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.Clusters().Get(ctx, client.Namespace(), "test-cluster")
	var transport *TransportError
	if err == nil || err.Error() != "request canceled" || !errors.As(err, &transport) || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled result = %v", err)
	}
}

func TestLiveResponseShapeMatrix(t *testing.T) {
	const secrets = "Bearer synthetic-token Authorization: Bearer synthetic-token alice@example.invalid https://user:password@example.invalid/path?token=synthetic-token"
	tests := []struct {
		name                    string
		code                    int
		contentType, body, want string
		transport               bool
	}{
		{"401 generic", 401, "application/json", `{"error":"forbidden","message":"` + secrets + `"}`, "not authenticated", false},
		{"401 status", 401, "application/json", `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","message":"` + secrets + `"}`, "not authenticated", false},
		{"403 contradictory", 403, "application/json", `{"error":"unauthorized","message":"` + secrets + `"}`, "permission denied", false},
		{"400 invalid", 400, "application/json", `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Invalid","message":"` + secrets + `","details":{"causes":[{"message":"` + secrets + `"}]}}`, "invalid request", false},
		{"404 unknown token", 404, "application/json", `{"error":"` + secrets + `"}`, "not found", false},
		{"409 already exists", 409, "application/json", `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"AlreadyExists","message":"` + secrets + `"}`, "already exists", false},
		{"409 conflict", 409, "application/json", `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Conflict"}`, "conflict", false},
		{"422 invalid", 422, "application/json", `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Invalid"}`, "invalid request", false},
		{"429 status", 429, "application/json", `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"TooManyRequests"}`, "too many requests", false},
		{"500 status", 500, "application/json", `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"InternalError"}`, "server error", false},
		{"503 status", 503, "application/json", `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"ServiceUnavailable"}`, "service unavailable", false},
		{"504 status", 504, "application/json", `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Timeout"}`, "request timed out", false},
		{"unknown status reason", 500, "application/json", `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"SecretReason","message":"` + secrets + `"}`, "server error", false},
		{"mismatched token", 404, "application/json", `{"error":"forbidden"}`, "not found", false},
		{"nested token", 500, "application/json", `{"nested":{"error":"internal_error"}}`, "server error", false},
		{"arbitrary string", 500, "application/json", `"` + secrets + `"`, "server error", false},
		{"empty body", 500, "application/json", ``, "server error", false},
		{"malformed body", 500, "application/json", `{"error":`, "server error", false},
		{"xml forbidden", 403, "application/xml", `<error>` + secrets + `</error>`, "request failed", true},
		{"malformed content type", 403, "application/json; =bad", `{"error":"forbidden"}`, "request failed", true},
		{"malformed JSON forbidden", 403, "application/json", `{"error":`, "permission denied", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := testRESTClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				w.WriteHeader(tt.code)
				_, _ = w.Write([]byte(tt.body))
			})
			_, err := client.Clusters().Get(context.Background(), client.Namespace(), "test-cluster")
			if err == nil || err.Error() != tt.want {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			for _, rendered := range []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err)} {
				for _, secret := range []string{"synthetic-token", "Authorization:", "alice@example.invalid", "user:password"} {
					if strings.Contains(rendered, secret) {
						t.Errorf("secret in rendered error: %q", rendered)
					}
				}
				for _, forbidden := range []string{"Platform API", "GET clusters", "HTTP 4", "HTTP 5"} {
					if strings.Contains(rendered, forbidden) {
						t.Errorf("unexpected context in %q", rendered)
					}
				}
			}
			var httpErr *HTTPError
			var transportErr *TransportError
			if tt.transport {
				if !errors.As(err, &transportErr) || errors.As(err, &httpErr) {
					t.Fatalf("want transport, got %T", err)
				}
			} else if !errors.As(err, &httpErr) || httpErr.StatusCode() != tt.code || errors.As(err, &transportErr) {
				t.Fatalf("want HTTP status %d, got %T", tt.code, err)
			}
		})
	}
}

func TestCreatesUncertainMatrix(t *testing.T) {
	for _, resource := range []string{"clusters", "nodepools"} {
		for _, tc := range []struct {
			name       string
			code       int
			body, want string
			uncertain  bool
		}{
			{"conflict", 409, `{}`, "already exists" + checkBeforeRetrying, true},
			{"server", 503, `{}`, "service unavailable" + checkBeforeRetrying, true},
			{"decode", 200, `{`, "server reported success, but its response was invalid" + checkBeforeRetrying, true},
			{"unauthorized", 401, `{}`, "not authenticated", false},
			{"forbidden", 403, `{}`, "permission denied", false},
			{"not found", 404, `{}`, "not found", false},
			{"invalid", 422, `{}`, "invalid request", false},
			{"rate limit", 429, `{}`, "too many requests", false},
		} {
			t.Run(resource+"/"+tc.name, func(t *testing.T) {
				var requests atomic.Int32
				client := testRESTClient(t, func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if r.Method != "POST" {
						t.Errorf("method = %s", r.Method)
					}
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(tc.code)
					_, _ = w.Write([]byte(tc.body))
				})
				var err error
				switch resource {
				case "clusters":
					_, err = client.Clusters().Create(context.Background(), client.Namespace(), &gcpv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"}})
				case "nodepools":
					_, err = client.NodePools().Create(context.Background(), client.Namespace(), &gcpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "test-pool"}})
			}
				if err == nil || err.Error() != tc.want || IsUncertainOutcome(err) != tc.uncertain {
					t.Fatalf("error = %v, uncertain = %v", err, IsUncertainOutcome(err))
				}
				var h *HTTPError
				if tc.code != 200 && (!errors.As(err, &h) || h.StatusCode() != tc.code || h.Method() != "POST" || h.Resource() != resource) {
					t.Errorf("missing typed POST metadata: %v", err)
				}
				if requests.Load() != 1 {
					t.Errorf("requests = %d", requests.Load())
				}
			})
		}
	}
}
