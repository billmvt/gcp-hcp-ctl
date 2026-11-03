package cli

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type platformRoleTraceTransport func(*http.Request) (*http.Response, error)

// RoundTrip delegates to the synthetic catalog transport without using the network.
func (f platformRoleTraceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Exercise the real root command and authenticated catalog constructor. Only
// gcloud and the network transport are replaced; no real credentials are read.
func TestPlatformRoleTraceRedactsHostname(t *testing.T) {
	for _, key := range []string{"GCPHCPCTL_ENVIRONMENT", "GCPHCPCTL_REGION", "GCPHCPCTL_VERBOSITY", "GCPHCPCTL_PROJECT", "GCPHCPCTL_API_ENDPOINT"} {
		t.Setenv(key, "")
	}
	dir := t.TempDir()
	adc := filepath.Join(dir, "adc.json")
	if err := os.WriteFile(adc, []byte(`{"type":"authorized_user"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", adc)
	const fakeGcloud = `#!/bin/sh
case "$*" in
  "auth print-identity-token") printf '%s\n' 'synthetic-trace-token' ;;
  "config get-value account") printf '%s\n' 'synthetic@example.test' ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "gcloud"), []byte(fakeGcloud), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	const authority = "internal-platform.example.test:8443"
	const endpoint = "https://" + authority
	for _, verb := range []string{"list", "get"} {
		for level := 6; level <= 10; level++ {
			t.Run(fmt.Sprintf("%s/v%d", verb, level), func(t *testing.T) {
				calls := 0
				http.DefaultTransport = platformRoleTraceTransport(func(req *http.Request) (*http.Response, error) {
					calls++
					path := "/apis/gcp.managed.openshift.io/v1/platformroles"
					if verb == "get" {
						path += "/reader"
					}
					if req.URL.Host != authority || req.Host != authority || req.URL.Scheme != "https" || req.URL.Path != path || req.Method != http.MethodGet || req.Header.Get("Authorization") != "Bearer synthetic-trace-token" {
						t.Fatal("request destination, routing or authentication changed")
					}
					role := `{"apiVersion":"gcp.managed.openshift.io/v1","kind":"PlatformRole","metadata":{"name":"reader","annotations":{"endpoint":"` + endpoint + `"}},"spec":{"permissions":["cluster.get"]}}`
					body := role
					if verb == "list" {
						body = `{"apiVersion":"gcp.managed.openshift.io/v1","kind":"PlatformRoleList","items":[` + role + `]}`
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {endpoint}}, Body: io.NopCloser(strings.NewReader(body))}, nil
				})
				root := newRootCmd()
				var out, stderr bytes.Buffer
				root.SetOut(&out)
				root.SetErr(&stderr)
				args := []string{"platformrole", verb, "--api-endpoint", endpoint, "--config", filepath.Join(dir, "missing.yaml"), "--output", "json", "--v=" + strconv.Itoa(level)}
				if verb == "get" {
					args = append(args, "reader")
				}
				root.SetArgs(args)
				if err := root.Execute(); err != nil {
					t.Fatal(err)
				}
				if calls != 1 || !strings.Contains(out.String(), "cluster.get") {
					t.Fatalf("request or output changed: calls=%d output=%s", calls, out.String())
				}
				if strings.Contains(stderr.String(), "internal-platform") || strings.Contains(stderr.String(), ":8443") || strings.Contains(stderr.String(), "synthetic-trace-token") {
					t.Fatalf("trace exposed hostname, port or token: %s", stderr.String())
				}
				assertProgressiveTrace(t, stderr.String(), level)
				if !strings.Contains(stderr.String(), "https://REDACTED/apis/gcp.managed.openshift.io/v1/platformroles") {
					t.Errorf("missing sanitized catalog URL: %s", stderr.String())
				}
			})
		}
	}
}

// TestPlatformRoleInvalidEndpointDiagnostic exercises Execute's real stderr
// output without credentials, discovery or an HTTP attempt, including with tracing.
func TestPlatformRoleInvalidEndpointDiagnostic(t *testing.T) {
	for _, key := range []string{"GCPHCPCTL_ENVIRONMENT", "GCPHCPCTL_REGION", "GCPHCPCTL_PROJECT", "GCPHCPCTL_API_ENDPOINT", "GCPHCPCTL_VERBOSITY"} {
		t.Setenv(key, "")
	}
	dir := t.TempDir()
	adc := filepath.Join(dir, "adc.json")
	if err := os.WriteFile(adc, []byte(`{"type":"authorized_user"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", adc)
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = platformRoleTraceTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid endpoint caused an HTTP request")
		return nil, nil
	})
	for _, tc := range []struct{ endpoint, want string }{
		{"http://internal-platform.example.test:8443", "API endpoint must use HTTPS"},
		{"http://probe-user:probe-password@internal-platform.example.test?token=probe-token", "API endpoint must use HTTPS"},
		{"https://internal-platform.example.test/%zz?token=probe-token", "creating REST client: invalid API endpoint configuration"},
	} {
		for _, verb := range []string{"list", "get"} {
			for _, level := range []int{0, 6, 10} {
				t.Run(fmt.Sprintf("%s/%s/v%d", tc.endpoint, verb, level), func(t *testing.T) {
					previousArgs, previousStderr := os.Args, os.Stderr
					r, w, err := os.Pipe()
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { os.Args, os.Stderr = previousArgs, previousStderr; r.Close(); w.Close() })
					os.Stderr = w
					os.Args = []string{"gcphcpctl", "platformrole", verb, "--api-endpoint", tc.endpoint, "--config", filepath.Join(dir, "missing.yaml"), "--v=" + strconv.Itoa(level)}
					if verb == "get" {
						os.Args = append(os.Args, "reader")
					}
					commandErr := Execute()
					if err := w.Close(); err != nil {
						t.Fatal(err)
					}
					diagnostic, err := io.ReadAll(r)
					if err != nil {
						t.Fatal(err)
					}
					if commandErr == nil || commandErr.Error() != tc.want || string(diagnostic) != tc.want+"\n" {
						t.Fatalf("error=%v, stderr=%q; want fixed diagnostic %q", commandErr, diagnostic, tc.want)
					}
				})
			}
		}
	}
}
