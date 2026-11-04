package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/openshift-online/gcp-hcp-ctl/pkg/internal/access"
	"github.com/openshift-online/gcp-hcp-ctl/pkg/platformapi"
	gcpv1 "github.com/openshift-online/gecko/platform-api/api/public/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

// Exercise a real command, REST encoding/decoding and the shared client transport
// together; only credentials and the remote server are replaced for this test.
func TestVerbosityPlatformCommand(t *testing.T) {
	t.Setenv("GCPHCPCTL_ENVIRONMENT", "")
	t.Setenv("GCPHCPCTL_REGION", "")
	t.Setenv("GCPHCPCTL_VERBOSITY", "")
	for _, tc := range []struct {
		name   string
		level  int
		status int
	}{
		{"default level", 0, 200},
		{"low level", 1, 200},
		{"below HTTP threshold", 5, 200},
		{"HTTP metadata", 6, 200},
		{"request headers", 7, 200},
		{"response headers and body", 8, 200},
		{"curl and timing", 9, 200},
		{"untruncated body", 10, 200},
		{"traced forbidden", 6, 403},
		{"body level forbidden", 8, 403},
		{"high level forbidden", 10, 403},
		{"quiet forbidden", 0, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const path = "/apis/gcp.managed.openshift.io/v1/namespaces/customer/roles"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != path {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Probe-Header", "private-response-header")
				w.Header().Set("Set-Cookie", "session=private-cookie")
				w.WriteHeader(tc.status)
				if tc.status == http.StatusOK {
					fmt.Fprint(w, `{"apiVersion":"gcp.managed.openshift.io/v1","kind":"RoleList","metadata":{"resourceVersion":"visible-body-field"},"items":[]}`)
				} else {
					fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"private-server-prose","reason":"Forbidden","code":403}`)
				}
			}))
			defer server.Close()
			client, err := platformapi.NewClientForTest(server.URL, "customer")
			if err != nil {
				t.Fatal(err)
			}
			root := newRootCmd()
			var out, stderr bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&stderr)
			root.SetArgs([]string{"role", "list", "--config", filepath.Join(t.TempDir(), "missing.yaml"), "--project", "customer", "--api-endpoint", server.URL, "--output", "json", "--v=" + strconv.Itoa(tc.level)})
			checkKlog := unchangedKlog(t)
			err = root.ExecuteContext(access.WithClient(context.Background(), client))
			checkKlog()
			if tc.status == http.StatusOK {
				if err != nil || !json.Valid(out.Bytes()) {
					t.Fatalf("JSON stdout damaged: %q; error: %v", out.String(), err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "permission denied") || strings.Contains(err.Error(), "private-") || out.Len() != 0 {
				t.Fatalf("error normalization changed: %v; stdout: %q", err, out.String())
			}
			assertProgressiveTrace(t, stderr.String(), tc.level)
			if tc.level >= 6 {
				for _, want := range []string{`method="GET"`, "http://REDACTED" + path, "status=" + strconv.Itoa(tc.status), "elapsed="} {
					if !strings.Contains(stderr.String(), want) {
						t.Errorf("missing %q in trace: %s", want, stderr.String())
					}
				}
			}
			if strings.Contains(stderr.String(), "private-") {
				t.Fatalf("trace leaked credentials, unknown headers or error prose: %s", stderr.String())
			}
			if tc.level >= 8 {
				if tc.status == http.StatusOK {
					body := traceBody(t, stderr.String(), "response_body")
					if !json.Valid([]byte(body)) || !strings.Contains(body, "visible-body-field") {
						t.Errorf("decoded successful JSON response missing from trace: %s", body)
					}
				} else if line := traceEvent(t, stderr.String(), "response_body"); !strings.Contains(line, "omitted") {
					t.Errorf("error response needs an explicit omission: %s", line)
				}
			} else if strings.Contains(stderr.String(), "visible-body-field") {
				t.Errorf("body visible below level 8: %s", stderr.String())
			}
		})
	}
}

// This is the user's RoleBinding create path, with a synthetic principal and
// a local server. The injected test client never discovers or acquires credentials.
func TestVerbosityRoleBindingCreate(t *testing.T) {
	t.Setenv("GCPHCPCTL_ENVIRONMENT", "")
	t.Setenv("GCPHCPCTL_REGION", "")
	t.Setenv("GCPHCPCTL_VERBOSITY", "")
	const path = "/apis/gcp.managed.openshift.io/v1/namespaces/customer/rolebindings"
	const note = "Note: authorization changes may take a moment to propagate globally.\n"
	binding := &gcpv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "gcp.managed.openshift.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: "test-rb", Namespace: "customer"},
		Spec: gcpv1.RoleBindingSpec{
			Subject: "mickeymouse@example.com",
			RoleRef: gcpv1.RoleRef{Kind: "PlatformRole", Name: "cluster-admin", APIGroup: "gcp.managed.openshift.io"},
		},
	}
	requestJSON, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	response := binding.DeepCopy()
	response.ResourceVersion = "created-resource-version"
	responseJSON, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			var baseline string
			for _, level := range []int{0, 5, 6, 7, 8, 9, 10} {
				t.Run(strconv.Itoa(level), func(t *testing.T) {
					var calls atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						if r.Method != http.MethodPost || r.URL.Path != path || r.URL.RawQuery != "" {
							t.Errorf("unexpected request: %s %s", r.Method, r.URL)
						}
						if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "" {
							t.Errorf("unexpected request headers: %v", r.Header)
						}
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						assertEqualJSON(t, body, requestJSON)
						w.Header().Set("Content-Type", "application/json")
						w.Header().Set("X-Private", "private-response-header")
						w.Header().Set("Set-Cookie", "session=private-cookie")
						w.WriteHeader(http.StatusCreated)
						_, _ = w.Write(responseJSON)
					}))
					defer server.Close()
					client, err := platformapi.NewClientForTest(server.URL, "customer")
					if err != nil {
						t.Fatal(err)
					}
					root := newRootCmd()
					var out, stderr bytes.Buffer
					root.SetOut(&out)
					root.SetErr(&stderr)
					root.SetArgs([]string{"rolebinding", "create", "test-rb", "--subject=mickeymouse@example.com", "--platform-role=cluster-admin", "--project=customer", "--api-endpoint=" + server.URL, "--config", filepath.Join(t.TempDir(), "missing.yaml"), "--output=" + format, "--v=" + strconv.Itoa(level)})
					checkKlog := unchangedKlog(t)
					err = root.ExecuteContext(access.WithClient(context.Background(), client))
					checkKlog()
					if err != nil {
						t.Fatal(err)
					}
					if calls.Load() != 1 {
						t.Errorf("create made %d requests, want exactly one POST", calls.Load())
					}
					if format == "json" {
						assertEqualJSON(t, out.Bytes(), responseJSON)
					} else {
						for _, want := range []string{"test-rb", binding.Spec.Subject, binding.Spec.RoleRef.Name, note} {
							if !strings.Contains(out.String(), want) {
								t.Errorf("stdout missing %q: %s", want, out.String())
							}
						}
					}
					if level == 0 {
						baseline = out.String()
					} else if out.String() != baseline {
						t.Errorf("verbosity changed stdout:\ngot %q\nwant %q", out.String(), baseline)
					}
					trace := stderr.String()
					if format == "json" {
						if !strings.HasSuffix(trace, note) {
							t.Fatalf("authorization note missing from stderr: %s", trace)
						}
						trace = strings.TrimSuffix(trace, note)
					}
					assertProgressiveTrace(t, trace, level)
					if level >= 6 {
						for _, want := range []string{`method="POST"`, "http://REDACTED" + path, "status=201"} {
							if !strings.Contains(trace, want) {
								t.Errorf("trace missing %q: %s", want, trace)
							}
						}
					}
					if level >= 8 {
						assertEqualJSON(t, []byte(traceBody(t, trace, "request_body")), requestJSON)
						// Execution has decoded the response, which consumes it through EOF.
						assertEqualJSON(t, []byte(traceBody(t, trace, "response_body")), responseJSON)
					} else {
						for _, value := range []string{binding.Spec.Subject, "roleRef", binding.Spec.RoleRef.Name, response.ResourceVersion} {
							if strings.Contains(trace, value) {
								t.Errorf("level %d exposed body value %q: %s", level, value, trace)
							}
						}
					}
					if strings.Contains(trace, "private-") {
						t.Errorf("trace leaked credentials or unknown header values: %s", trace)
					}
				})
			}
		})
	}
}

func assertProgressiveTrace(t *testing.T, trace string, level int) {
	t.Helper()
	if level < 6 {
		if trace != "" {
			t.Errorf("quiet command produced trace: %s", trace)
		}
		return
	}
	for event, threshold := range map[string]int{"start": 6, "complete": 6, "request_headers": 7, "response_headers": 8, "response_body": 8, "curl": 9, "timing": 9} {
		want := 0
		if level >= threshold {
			want = 1
		}
		if count := strings.Count(trace, " "+event+" "); count != want {
			t.Errorf("level %d: got %d %s events, want %d: %s", level, count, event, want, trace)
		}
	}
	if level < 8 && strings.Contains(trace, " request_body ") {
		t.Errorf("level %d exposed request body: %s", level, trace)
	}
	if level == 6 && strings.Count(trace, "\n") != 2 {
		t.Errorf("level 6 must have exactly one start and completion: %s", trace)
	}
}

func traceEvent(t *testing.T, trace, event string) string {
	t.Helper()
	var matches []string
	for _, line := range strings.Split(trace, "\n") {
		if strings.Contains(line, " "+event+" ") {
			matches = append(matches, line)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("want one %s event, got %d: %s", event, len(matches), trace)
	}
	return matches[0]
}

func traceBody(t *testing.T, trace, event string) string {
	t.Helper()
	line := traceEvent(t, trace, event)
	_, quoted, ok := strings.Cut(line, " body=")
	if !ok {
		t.Fatalf("%s has no quoted body field: %s", event, line)
	}
	// UnquoteChar finds the end of a Go-quoted field even if more fields follow.
	if len(quoted) == 0 || quoted[0] != '"' {
		t.Fatalf("%s body is not quoted: %s", event, line)
	}
	rest := quoted[1:]
	for len(rest) > 0 && rest[0] != '"' {
		_, _, tail, err := strconv.UnquoteChar(rest, '"')
		if err != nil {
			t.Fatalf("invalid quoted body: %v: %s", err, line)
		}
		rest = tail
	}
	if len(rest) == 0 {
		t.Fatalf("unterminated quoted body: %s", line)
	}
	body, err := strconv.Unquote(quoted[:len(quoted)-len(rest)+1])
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func assertEqualJSON(t *testing.T, got, want []byte) {
	t.Helper()
	var gotObject, wantObject any
	if err := json.Unmarshal(got, &gotObject); err != nil {
		t.Errorf("invalid actual JSON: %v: %s", err, got)
		return
	}
	if err := json.Unmarshal(want, &wantObject); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotObject, wantObject) {
		t.Errorf("JSON mismatch:\ngot  %s\nwant %s", got, want)
	}
}

func unchangedKlog(t *testing.T) func() {
	t.Helper()
	before := make(map[klog.Level]bool)
	for _, level := range []klog.Level{0, 5, 6, 7, 8, 9, 10} {
		before[level] = klog.V(level).Enabled()
	}
	return func() {
		t.Helper()
		for level, enabled := range before {
			if klog.V(level).Enabled() != enabled {
				t.Errorf("CLI verbosity changed global klog verbosity at level %d", level)
			}
		}
	}
}
