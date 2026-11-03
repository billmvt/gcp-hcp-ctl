package httpdebug

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTraceRequestMetadataWithoutChangingPayloads checks metadata-only tracing without
// altering authentication or payloads.
func TestTraceRequestMetadataWithoutChangingPayloads(t *testing.T) {
	var trace bytes.Buffer
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(trace.String(), "start") {
			t.Error("request must be logged before it is sent")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "private-request-body" || r.Header.Get("Authorization") != "Bearer private-token" {
			t.Error("tracing changed the request")
		}
		w.Header().Set("Set-Cookie", "private-cookie")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "private-response-body")
	}))
	defer server.Close()

	ctx := WithTrace(context.Background(), &trace, 6)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/resource", strings.NewReader("private-request-body"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer private-token")
	client := &http.Client{Transport: Wrap(server.Client().Transport)}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "private-response-body" {
		t.Fatalf("response body = %q, %v", body, err)
	}
	for _, want := range []string{"request=1", "start", `method="POST"`, "http://REDACTED/resource", "complete", "status=201", "elapsed="} {
		if !strings.Contains(trace.String(), want) {
			t.Errorf("trace missing %q: %s", want, trace.String())
		}
	}
	if strings.Count(trace.String(), "\n") != 2 || strings.Contains(trace.String(), "private-") {
		t.Errorf("trace must contain only two metadata lines: %s", trace.String())
	}
}
