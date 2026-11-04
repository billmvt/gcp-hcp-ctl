package httpdebug

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestTraceRedactsURLWithoutChangingRequest checks that tracing masks URL secrets without
// changing the outbound request.
func TestTraceRedactsURLWithoutChangingRequest(t *testing.T) {
	for _, raw := range []string{
		"https://private-user:private-password@example.com/resource?limit=10&access_token=private-token&api_key=private-key#private-fragment",
		"https://example.com/resource?X-Goog-Signature=private-signature&X-Goog-Credential=private-credential&pageSize=5",
		"https://example.com/resource?unknown=private-secret&token=private-first&token=private-second",
		"https://example.com/resource?access_token=private-secret;limit=10&watch=true",
		"https://example.com/resource?token=private-secret%zz",
		"https://example.com/resource?limit=private-secret&pretty=private-secret&alt=private-secret",
		"https://example.com/resource?ACCESS_TOKEN=private-secret&api%5Fkey=private-secret",
		"https://example.com/v1/projects/p/locations/r/workflows/w/executions/e/callbacks/private-callback",
	} {
		t.Run(raw, func(t *testing.T) {
			var trace bytes.Buffer
			req, err := http.NewRequestWithContext(WithTrace(context.Background(), &trace, 6), http.MethodGet, raw, nil)
			if err != nil {
				t.Fatal(err)
			}
			base := roundTripFunc(func(got *http.Request) (*http.Response, error) {
				if got != req || got.URL.String() != raw {
					t.Fatal("tracing mutated the request or its URL")
				}
				return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
			})
			if _, err := Wrap(base).RoundTrip(req); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(trace.String(), "private-") {
				t.Errorf("secret leaked: %s", trace.String())
			}
			if !strings.Contains(trace.String(), "https://REDACTED/") {
				t.Errorf("missing useful URL: %s", trace.String())
			}
		})
	}
}

func TestTracePreservesSafeQueryMetadata(t *testing.T) {
	var trace bytes.Buffer
	raw := "https://example.com/resource?watch=true&limit=10&pageSize=5&alt=json&unknown=secret"
	req, _ := http.NewRequestWithContext(WithTrace(context.Background(), &trace, 6), http.MethodGet, raw, nil)
	_, _ = Wrap(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 204, Body: http.NoBody}, nil
	})).RoundTrip(req)
	for _, want := range []string{"watch=true", "limit=10", "pageSize=5", "alt=json", "unknown=REDACTED"} {
		if !strings.Contains(trace.String(), want) {
			t.Errorf("missing %q: %s", want, trace.String())
		}
	}
}

func TestTraceDoesNotLeakTransportErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		label string
	}{
		{"generic", errors.New("private-server-prose"), "transport_error"},
		{"url", &url.Error{Op: "Get", URL: "https://private-user:private-pass@example.com?token=private-token", Err: io.EOF}, "transport_error"},
		{"canceled", context.Canceled, "canceled"},
		{"deadline", context.DeadlineExceeded, "timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var trace bytes.Buffer
			req, _ := http.NewRequestWithContext(WithTrace(context.Background(), &trace, 6), http.MethodGet, "https://example.com", nil)
			_, err := Wrap(roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, tc.err
			})).RoundTrip(req)
			if err != tc.err {
				t.Fatal("tracing changed the returned error")
			}
			if strings.Contains(trace.String(), "private-") || !strings.Contains(trace.String(), "error="+tc.label) {
				t.Errorf("unsafe or missing error label: %s", trace.String())
			}
		})
	}
}
