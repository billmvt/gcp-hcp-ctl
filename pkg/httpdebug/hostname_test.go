package httpdebug

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// Host redaction must cover DNS names, IP literals, ports and relative URLs
// without changing resource paths or the caller's URL.
func TestSafeURLRedactsAuthority(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"https://internal.example.test:8443/resource?limit=5", "https://REDACTED/resource?limit=5"},
		{"http://192.0.2.1:8080/resource", "http://REDACTED/resource"},
		{"https://[2001:db8::1]:8443/resource", "https://REDACTED/resource"},
		{"//internal.example.test/resource", "//REDACTED/resource"},
		{"/resource?watch=true", "/resource?watch=true"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			u, err := url.Parse(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			before := *u
			if got := safeURL(u); got != tc.want {
				t.Errorf("safeURL = %q, want %q", got, tc.want)
			}
			if !reflect.DeepEqual(*u, before) {
				t.Fatal("redaction mutated URL")
			}
		})
	}
}

// All URL-bearing trace events share the same authority-redaction policy.
func TestTraceRedactsHostsAcrossEvents(t *testing.T) {
	const host = "internal.example.test"
	const endpoint = "https://" + host + ":8443/resource"
	const payload = `{"endpoint":"` + endpoint + `","message":"visit ` + endpoint + `"}`
	for level := 6; level <= 10; level++ {
		t.Run(fmt.Sprint(level), func(t *testing.T) {
			var trace bytes.Buffer
			req, err := http.NewRequestWithContext(WithTrace(context.Background(), &trace, level), http.MethodPost, endpoint, strings.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", "agent "+endpoint)
			originalHeaders := req.Header.Clone()
			base := roundTripFunc(func(sent *http.Request) (*http.Response, error) {
				if sent.URL.String() != endpoint || sent.Host != host+":8443" || !reflect.DeepEqual(sent.Header, originalHeaders) {
					t.Fatal("tracing changed destination or headers")
				}
				data, err := io.ReadAll(sent.Body)
				if err != nil || string(data) != payload {
					t.Fatalf("request body changed: %q, %v", data, err)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {endpoint}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
			})
			resp, err := Wrap(base).RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			if err != nil || string(data) != payload {
				t.Fatalf("response body changed: %q, %v", data, err)
			}
			if resp.Header.Get("X-Request-Id") != endpoint {
				t.Fatal("response header changed")
			}
			if strings.Contains(trace.String(), host) || strings.Contains(trace.String(), ":8443") {
				t.Fatalf("trace leaked authority: %s", trace.String())
			}
			for event, minimum := range map[string]int{"start": 6, "complete": 6, "request_headers": 7, "response_headers": 8, "request_body": 8, "response_body": 8, "curl": 9, "timing": 9} {
				if level >= minimum && !strings.Contains(trace.String(), " "+event+" ") {
					t.Errorf("missing %s event: %s", event, trace.String())
				}
			}
			if !strings.Contains(trace.String(), "https://REDACTED/resource") {
				t.Errorf("missing redacted URL: %s", trace.String())
			}
		})
	}
}

// TestSchemeRelativePayloadURLs sanitizes authorities and credentials in both
// whole-value and embedded scheme-relative URLs, including malformed references.
func TestSchemeRelativePayloadURLs(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"//private.example:8443/path", "//REDACTED/path"},
		{"  //private.example:8443/path  ", "  //REDACTED/path  "},
		{"visit //private.example:8443/path to continue", "visit //REDACTED/path to continue"},
		{"//192.0.2.1:8080/path", "//REDACTED/path"},
		{"//[2001:db8::1]:8443/path", "//REDACTED/path"},
		{"//probe-user:probe-password@private.example:8443/path?token=probe-token&limit=5#probe-fragment", "//REDACTED/path?token=REDACTED&limit=5"},
		{"//private.example/path?token=probe-token%zz", "//REDACTED/path?REDACTED"},
		{"//private.example/%zz", "REDACTED"},
		{"//probe-user:123 probe-password@private.example/path", "REDACTED"},
		{"visit //probe-user:123 probe-password@private.example/path to continue", "REDACTED"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			if got := safePayloadString(tc.raw, 0); got != tc.want {
				t.Fatalf("sanitized URL = %q, want %q", got, tc.want)
			}
			body, err := json.Marshal(map[string]string{"endpoint": tc.raw})
			if err != nil {
				t.Fatal(err)
			}
			checkPayloadTraceRedaction(t, string(body), "private.example", ":8443", "192.0.2.1", "2001:db8::1", "probe-user", "probe-password", "probe-token", "probe-fragment")
			headers := http.Header{"User-Agent": {tc.raw}}
			before := headers.Clone()
			req, err := http.NewRequest(http.MethodGet, "https://api.example.test/path", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header = headers
			for _, diagnostic := range []string{headerText(headers), curlCommand(req)} {
				for _, marker := range []string{"private.example", ":8443", "192.0.2.1", "2001:db8::1", "probe-user", "probe-password", "probe-token", "probe-fragment"} {
					if strings.Contains(diagnostic, marker) {
						t.Errorf("header/curl diagnostic leaked %q: %s", marker, diagnostic)
					}
				}
			}
			if !reflect.DeepEqual(headers, before) {
				t.Fatal("trace sanitization mutated headers")
			}
		})
	}
}
