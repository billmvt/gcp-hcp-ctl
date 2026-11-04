package platformapi

import (
	"testing"

	"github.com/openshift-online/gcp-hcp-ctl/pkg/auth"
	"k8s.io/client-go/rest"
)

// TestClientConstructionErrorsOmitEndpoints protects both catalog and namespaced
// constructors from echoing endpoint hosts, userinfo or query credentials.
func TestClientConstructionErrorsOmitEndpoints(t *testing.T) {
	for _, tc := range []struct{ endpoint, want string }{
		{"http://internal.example.test:8443", "API endpoint must use HTTPS"},
		{"http://probe-user:probe-password@internal.example.test?token=probe-token", "API endpoint must use HTTPS"},
		{"https://internal.example.test/%zz?token=probe-token", "creating REST client: invalid API endpoint configuration"},
	} {
		t.Run(tc.endpoint, func(t *testing.T) {
			if _, err := NewPlatformRoleClient(tc.endpoint, &auth.TokenSource{}); err == nil || err.Error() != tc.want {
				t.Errorf("catalog construction error = %v, want %q", err, tc.want)
			}
			if _, err := NewClient(tc.endpoint, "project", &auth.TokenSource{}); err == nil || err.Error() != tc.want {
				t.Errorf("namespaced construction error = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestTestClientSkipsThrottling keeps local pagination tests fast while ensuring
// production clients still receive client-go's default rate limiter.
func TestTestClientSkipsThrottling(t *testing.T) {
	local, err := NewClientForTest("http://127.0.0.1:1234", "project")
	if err != nil {
		t.Fatal(err)
	}
	if local.restClient.(*rest.RESTClient).GetRateLimiter() != nil {
		t.Error("local test client retains request throttling")
	}
	production, err := NewClient("https://api.example.test", "project", &auth.TokenSource{})
	if err != nil {
		t.Fatal(err)
	}
	if production.restClient.(*rest.RESTClient).GetRateLimiter() == nil {
		t.Error("production request throttling was disabled")
	}
}
