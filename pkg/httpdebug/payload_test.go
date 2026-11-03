package httpdebug

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// Fixtures contain only synthetic markers; the transport never uses the network.
func checkPayloadTraceRedaction(t *testing.T, body string, markers ...string) string {
	t.Helper()
	formatted := formatBody([]byte(body), "application/json", 10)
	if !json.Valid([]byte(formatted)) || !strings.Contains(formatted, "REDACTED") {
		t.Errorf("expected sanitized JSON, got %s", formatted)
	}
	for _, level := range []int{8, 9, 10} {
		t.Run(fmt.Sprintf("v%d", level), func(t *testing.T) {
			var trace bytes.Buffer
			req, err := http.NewRequestWithContext(WithTrace(context.Background(), &trace, level), http.MethodPost, "https://example.test/resource", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			transport := Wrap(roundTripFunc(func(sent *http.Request) (*http.Response, error) {
				data, err := io.ReadAll(sent.Body)
				if err != nil || string(data) != body {
					t.Fatal("request body changed")
				}
				sent.Body.Close()
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			resp, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || string(data) != body {
				t.Fatal("response body changed")
			}
			for _, event := range []string{"request_body", "response_body"} {
				if strings.Count(trace.String(), " "+event+" ") != 1 {
					t.Errorf("expected one %s event: %s", event, trace.String())
				}
				for _, line := range strings.Split(trace.String(), "\n") {
					if strings.Contains(line, " "+event+" ") && !strings.Contains(line, "REDACTED") {
						t.Errorf("missing sanitized %s: %s", event, line)
					}
				}
			}
			for _, marker := range markers {
				if strings.Contains(formatted, marker) || strings.Contains(trace.String(), marker) {
					t.Errorf("synthetic credential %q leaked at v%d", marker, level)
				}
			}
		})
	}
	return formatted
}

func TestPayloadCallbackResourceNames(t *testing.T) {
	const resource = "projects/p/locations/r/workflows/w/executions/e/callbacks/review-marker-capability"
	for _, body := range []string{
		`{"callbacks":[{"name":"` + resource + `","method":"POST"}]}`,
		`{"name":"` + resource + `"}`,
		`{"name":"/` + resource + `"}`,
		`{"name":"` + strings.Replace(resource, "/callbacks/", "/%63allbacks/", 1) + `"}`,
	} {
		t.Run(body, func(t *testing.T) {
			got := checkPayloadTraceRedaction(t, body, "review-marker-capability")
			if !strings.Contains(got, "projects/p/locations/r/workflows/w/executions/e/callbacks/REDACTED") {
				t.Errorf("safe callback resource context lost: %s", got)
			}
		})
	}
}

func TestPayloadDuplicateCredentialDiscriminators(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"name", `{"env":[{"name":"API_TOKEN","name":"REGION","value":"review-marker"}]}`},
		{"key", `{"key":"session_cookie","key":"normal","value":"review-marker"}`},
		{"path", `[{"op":"replace","path":"/spec/password","path":"/spec/description","value":"review-marker"}]`},
		{"normalized-name", `{"nA-me":"api.to-ken","nA-me":"REGION","Va_lue":"review-marker"}`},
		{"normalized-key", `{"K_E-Y":"SESSION.COOKIE","K_E-Y":"normal","VALUE":"review-marker"}`},
		{"normalized-path", `{"P-ATH":"/spec/pass-word","P-ATH":"/spec/description","va.lue":"review-marker"}`},
		{"case-duplicates", `{"name":"API_TOKEN","NAME":"REGION","name":"REGION","value":"review-marker"}`},
		{"nonstring-duplicate", `{"value":"review-marker","key":"session_cookie","key":null}`},
		{"value-duplicates", `{"name":"API_TOKEN","value":"review-marker","value":"review-marker-later","name":"REGION","Va_lue":"review-marker-case"}`},
		{"encoded-path", `{"path":"/spec/%70ass~1word","path":"/spec/description","value":"review-marker"}`},
		{"embedded-json", `{"annotation":"{\"name\":\"API_TOKEN\",\"name\":\"REGION\",\"value\":\"review-marker\"}"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkPayloadTraceRedaction(t, tc.body, "review-marker")
		})
	}
	body := `{"name":"ZONE","name":"REGION","value":"us-west1","roleRef":{"name":"cluster-admin"}}`
	want := `{"name":"REGION","roleRef":{"name":"cluster-admin"},"value":"us-west1"}`
	if got := formatBody([]byte(body), "application/json", 10); got != want {
		t.Errorf("benign duplicate discriminators lost visible values: %s", got)
	}
}

func TestPayloadSecretListContext(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"kindless-item", `{"apiVersion":"v1","kind":"SecretList","items":[{"metadata":{"name":"tls-material"},"data":{"tls.key":"cmV2aWV3LW1hcmtlci10bHM="},"type":"kubernetes.io/tls"}]}`},
		{"string-data", `{"kind":"SecretList","items":[{"stringData":{"unrecognized":"review-marker"}},{"data":{"tls.key":"review-marker-second"}}]}`},
		{"kind-after-items", `{"items":[{"data":{"tls.key":"review-marker"}}],"kind":"SecretList"}`},
		{"duplicate-kind", `{"kind":"SecretList","kind":"ConfigMapList","items":[{"data":{"tls.key":"review-marker"}}]}`},
		{"normalized-fields", `{"k_i-nD":"sEcReTlIsT","KIND":"RoleBindingList","ITEMS":[{"DA_TA":{"tls.key":"review-marker"},"String_Data":{"x":"review-marker-string"}}]}`},
		{"duplicate-items", `{"kind":"SecretList","items":[],"items":[{"data":{"tls.key":"review-marker"}}],"Items":[{"stringData":{"x":"review-marker-case"}}]}`},
		{"duplicate-item-data", `{"kind":"SecretList","items":[{"kind":"ConfigMap","data":{"tls.key":"review-marker-first"},"data":{"tls.key":"review-marker-last"},"DATA":{"x":"review-marker-case"}}]}`},
		{"both-secret-kinds", `{"kind":"Secret","kind":"SecretList","kind":"List","data":{"x":"review-marker-top"},"items":[{"data":{"x":"review-marker-item"}}]}`},
		{"nested-list", `{"items":[{"kind":"SecretList","items":[{"data":{"x":"review-marker"}}]}]}`},
		{"nonarray-items", `{"kind":"SecretList","items":{"data":{"x":"review-marker"}}}`},
		{"nonobject-item", `{"kind":"SecretList","items":["review-marker",["review-marker-nested"]]}`},
		{"embedded-json", `{"result":"{\"kind\":\"SecretList\",\"items\":[{\"data\":{\"tls.key\":\"review-marker\"}}]}"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := checkPayloadTraceRedaction(t, tc.body, "review-marker", "cmV2aWV3LW1hcmtlci10bHM=")
			if tc.name == "kindless-item" && !strings.Contains(got, `"metadata":{"name":"tls-material"}`) {
				t.Errorf("SecretList item metadata lost: %s", got)
			}
		})
	}
	body := `{"kind":"ConfigMapList","items":[{"data":{"normal":"visible"}}]}`
	if got := formatBody([]byte(body), "application/json", 10); !strings.Contains(got, `"data":{"normal":"visible"}`) {
		t.Errorf("non-secret list data hidden: %s", got)
	}
}

// TestPayloadAmbiguousCredentialURLs ensures whitespace cannot hide URL userinfo from
// payload, header or curl redaction.
func TestPayloadAmbiguousCredentialURLs(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"numeric-prefix", "https://user:123 review-marker@example.test/path?token=query-marker"},
		{"embedded", "visit https://user:123 review-marker@example.test/path?token=query-marker to continue"},
		{"host-prefix", "https://user review-marker@example.test/path?token=query-marker"},
		{"empty-port-prefix", "https://user: review-marker@example.test/path?token=query-marker"},
		{"multiple-spaces", "https://user:123 review-marker more@example.test/path?token=query-marker"},
		{"tab", "https://user:123\treview-marker@example.test/path?token=query-marker"},
		{"newline", "https://user:123\nreview-marker@example.test/path?token=query-marker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]string{"endpoint": tc.value})
			if err != nil {
				t.Fatal(err)
			}
			got := checkPayloadTraceRedaction(t, string(body), "review-marker", "query-marker")
			if got != `{"endpoint":"REDACTED"}` {
				t.Errorf("ambiguous URL must be fully redacted: %s", got)
			}
			headers := http.Header{"User-Agent": {tc.value}, "X-Request-Id": {tc.value}}
			original := headers.Clone()
			if got := headerText(headers); got != `{"User-Agent":["REDACTED"],"X-Request-Id":["REDACTED"]}` {
				t.Errorf("allowlisted headers leaked an ambiguous URL: %s", got)
			}
			req, err := http.NewRequest(http.MethodGet, "https://example.test/resource", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header = headers
			command := curlCommand(req)
			if strings.Contains(command, "review-marker") || strings.Contains(command, "query-marker") {
				t.Errorf("curl leaked an ambiguous header URL: %s", command)
			}
			if !reflect.DeepEqual(headers, original) {
				t.Fatal("header sanitization changed the request")
			}
		})
	}
	if got := safePayloadString("visit https://example.test:443 to continue", 0); got != "visit https://REDACTED to continue" {
		t.Errorf("ordinary URL context changed: %s", got)
	}
}

func TestPayloadIndirectCredentialValues(t *testing.T) {
	for _, body := range []string{
		`[{"op":"replace","path":"/spec/password","value":"private-secret"}]`,
		`[{"op":"add","path":"/data/key","value":"private-secret"}]`,
		`{"env":[{"name":"API_TOKEN","value":"private-secret"},{"name":"REGION","value":"us-west1"}]}`,
		`{"key":"session_cookie","value":"private-secret"}`,
	} {
		got := formatBody([]byte(body), "application/json-patch+json", 10)
		if strings.Contains(got, "private-secret") || !strings.Contains(got, "REDACTED") {
			t.Errorf("indirect credential leaked: %s", got)
		}
	}
	got := formatBody([]byte(`[{"op":"replace","path":"/spec/roleRef/name","value":"cluster-admin"}]`), "application/json-patch+json", 8)
	if !strings.Contains(got, "cluster-admin") {
		t.Fatalf("normal patch value hidden: %s", got)
	}
}

func TestPayloadNestingLimit(t *testing.T) {
	body := strings.Repeat("[", 200) + `{"normal":"never-print-me"}` + strings.Repeat("]", 200)
	got := formatBody([]byte(body), "application/json", 10)
	if !strings.HasPrefix(got, "[omitted:") || strings.Contains(got, "never-print-me") {
		t.Errorf("excessively nested JSON was not omitted: %.100s", got)
	}
	embedded, _ := json.Marshal(strings.Repeat("[", 20) + `"never-print-me"` + strings.Repeat("]", 20))
	body = strings.Repeat("[", 90) + string(embedded) + strings.Repeat("]", 90)
	got = formatBody([]byte(body), "application/json", 10)
	if !strings.Contains(got, "[omitted:") || strings.Contains(got, "never-print-me") {
		t.Fatalf("encoded nesting bypassed limit: %.100s", got)
	}
}

func TestPayloadDuplicateSecretKindCannotBypassRedaction(t *testing.T) {
	for _, body := range []string{
		`{"kind":"Secret","kind":"ConfigMap","data":{"key":"never-print-me"}}`,
		`{"kind":"ConfigMap","kind":"Secret","data":{"key":"never-print-me"}}`,
		`{"kind":"Secret","KIND":"RoleBinding","stringData":{"key":"never-print-me"}}`,
		`{"items":[{"kind":"Secret","kind":"ConfigMap","data":{"key":"never-print-me"}}]}`,
	} {
		got := formatBody([]byte(body), "application/json", 10)
		if strings.Contains(got, "never-print-me") || !strings.Contains(got, "REDACTED") || !json.Valid([]byte(got)) {
			t.Errorf("duplicate kind bypassed redaction: %s", got)
		}
	}
}

func TestPayloadCredentialURLPunctuation(t *testing.T) {
	for _, value := range []string{
		"https://user:quote'secret@example.test/path?token=signed-secret",
		"https://user:space secret@example.test/path?token=signed-secret",
		"https://user:percent%zzsecret@example.test/path?token=signed-secret",
	} {
		body, _ := json.Marshal(map[string]string{"message": value})
		got := formatBody(body, "application/json", 10)
		if strings.Contains(got, "secret") {
			t.Errorf("partially parsed URL leaked credential: %s", got)
		}
	}
}

func TestHeadersPayloadRecognizedSecretsAndUnsafeBytes(t *testing.T) {
	headers := http.Header{
		"User-Agent":   {"Bearer hidden-header", "https://user:password@example.test/agent?key=url-secret", "agent\xff\x00\nnext"},
		"X-Request-Id": {"Basic dXNlcjpwYXNzd29yZA=="},
	}
	got := headerText(headers)
	for _, secret := range []string{"hidden-header", "password", "url-secret", "dXNlcjpwYXNzd29yZA=="} {
		if strings.Contains(got, secret) {
			t.Errorf("allowlisted header leaked recognized credential %s", secret)
		}
	}
	req, err := http.NewRequest(http.MethodGet, "https://example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = headers
	command := curlCommand(req)
	if !utf8.ValidString(command) || strings.ContainsAny(command, "\x00\n\r") {
		t.Fatalf("curl contains unsafe bytes: %q", command)
	}
}

func TestPayloadUnsafeBytes(t *testing.T) {
	body := []byte("{\"message\":\"invalid-\xff-utf8\\nline\\u001bescape\",\"pass\\u0000word\":\"never-print-me\"}")
	got := formatBody(body, "application/json", 10)
	if !utf8.ValidString(got) || !json.Valid([]byte(got)) || strings.ContainsAny(got, "\n\x1b") || strings.Contains(got, "never-print-me") {
		t.Fatalf("unsafe byte handling: %q", got)
	}
}

func TestPayloadVerbosityTruncation(t *testing.T) {
	for _, text := range []string{strings.Repeat("a", 12000), strings.Repeat("界", 4000)} {
		body, _ := json.Marshal(map[string]string{"message": text, "token": "tail-secret"})
		full, _ := json.Marshal(map[string]string{"message": text, "token": "REDACTED"})
		for _, level := range []int{6, 7, 8, 9, 10, 11} {
			t.Run(fmt.Sprintf("level%d_bytes%d", level, len(text)), func(t *testing.T) {
				got := formatBody(body, "application/json", level)
				if level < 8 {
					if !strings.HasPrefix(got, "[omitted:") {
						t.Fatalf("low verbosity exposed body: %.100s", got)
					}
					return
				}
				want := string(full)
				if level < 10 {
					limit := 1024
					if level == 9 {
						limit = 10240
					}
					for !utf8.Valid(full[:limit]) {
						limit--
					}
					want = string(full[:limit]) + fmt.Sprintf("[truncated %d bytes]", len(full)-limit)
				}
				if got != want || !utf8.ValidString(got) || strings.Contains(got, "tail-secret") {
					t.Fatalf("wrong sanitized truncation: got %d bytes, want %d", len(got), len(want))
				}
			})
		}
	}
	body, _ := json.Marshal(map[string]string{"password": strings.Repeat("secret", 3000), "kind": "RoleBinding"})
	if got := formatBody(body, "application/json", 8); got != `{"kind":"RoleBinding","password":"REDACTED"}` {
		t.Fatalf("truncation occurred before redaction: %.100s", got)
	}
}

// TestPayloadCredentialBearingStrings checks credential-like strings and embedded URLs
// while preserving useful JSON context.
func TestPayloadCredentialBearingStrings(t *testing.T) {
	for _, tc := range []struct {
		name, value string
	}{
		{"PEM", "prefix -----BEGIN RSA PRIVATE KEY-----\nprivate-material\n-----END RSA PRIVATE KEY-----"},
		{"OpenSSH", "-----BEGIN OPENSSH PRIVATE KEY-----\nprivate-material\n-----END OPENSSH PRIVATE KEY-----"},
		{"bearer", "please use bEaReR bearer-material"},
		{"basic", "Basic dXNlcjpwYXNzd29yZA=="},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"nested": []any{tc.value}})
			if got := formatBody(body, "application/json", 10); got != `{"nested":["REDACTED"]}` {
				t.Fatalf("recognized credential string not redacted: %s", got)
			}
		})
	}
	body, _ := json.Marshal(map[string]string{"endpoint": "https://name:password@example.test/roles?X-Goog-Signature=signed-secret&limit=7#fragment-secret", "message": "visit https://other:password@example.test/roles?token=embedded-secret to continue"})
	got := formatBody(body, "application/json", 10)
	for _, secret := range []string{"password", "signed-secret", "fragment-secret", "embedded-secret"} {
		if strings.Contains(got, secret) {
			t.Errorf("URL leaked %q: %s", secret, got)
		}
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["endpoint"] != "https://REDACTED/roles?X-Goog-Signature=REDACTED&limit=7" || decoded["message"] != "visit https://REDACTED/roles?token=REDACTED to continue" {
		t.Fatalf("URL sanitization discarded safe context: %v", decoded)
	}
}

func TestPayloadEncodedJSONStrings(t *testing.T) {
	embedded := `{"token":"embedded-secret","kind":"RoleBinding","roleRef":{"name":"admin"},"number":9007199254740993123456789}`
	encodedAgain, _ := json.Marshal(embedded)
	body, _ := json.Marshal(map[string]any{"annotation": embedded, "twice": string(encodedAgain), "array": `[{"password":"array-secret"}]`, "broken": `{"password":"broken-secret"`})
	got := formatBody(body, "application/json", 10)
	for _, secret := range []string{"embedded-secret", "array-secret", "broken-secret"} {
		if strings.Contains(got, secret) {
			t.Errorf("embedded JSON leaked %q: %s", secret, got)
		}
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["annotation"] != `{"kind":"RoleBinding","number":9007199254740993123456789,"roleRef":{"name":"admin"},"token":"REDACTED"}` || !strings.HasPrefix(decoded["broken"], "[omitted:") {
		t.Fatalf("embedded JSON not safely retained: %v", decoded)
	}
}

func TestPayloadRecursiveCredentialFields(t *testing.T) {
	fields := []string{"authorization", "Proxy-Authorization", "cookie", "Set-Cookie", "password", "Pass.Word", "passwd", "passphrase", "token", "access_token", "ID-Token", "Refresh.Token", "client_secret", "private-key", "pull_secret", "api_key", "kubeconfig", ".dockerconfigjson", "credentials", "auth", "aws_access_key_id", "serviceAccountKey", "client-key-data", "client-key", ".dockercfg", "service_account_json"}
	for _, field := range fields {
		t.Run(field, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"items": []any{map[string]any{field: map[string]string{"nested": "never-print-me"}, "roleRef": map[string]string{"name": "admin"}}}})
			if err != nil {
				t.Fatal(err)
			}
			got := formatBody(body, "application/json", 10)
			if strings.Contains(got, "never-print-me") || !strings.Contains(got, `"REDACTED"`) || !strings.Contains(got, `"roleRef":{"name":"admin"}`) {
				t.Fatalf("recursive redaction failed: %s", got)
			}
		})
	}
	got := formatBody([]byte(`{"password":"duplicate-secret-one","password":"duplicate-secret-two","PASSWORD":"case-secret","pass_word":"separator-secret","normal":"visible"}`), "application/json", 10)
	if strings.Contains(got, "secret") || !strings.Contains(got, `"normal":"visible"`) || strings.Count(got, "REDACTED") != 3 {
		t.Fatalf("duplicate/case variant credential redaction failed: %s", got)
	}
}

func TestPayloadSecretResource(t *testing.T) {
	body := []byte(`{"items":[{"apiVersion":"v1","kind":"Secret","metadata":{"name":"pull-secret"},"data":{"config":"secret-base64"},"stringData":{"unrecognized":"secret-string"}},{"Kind":"secret","DATA":{"x":"secret-case"},"String_Data":{"x":"secret-separator"}},{"kind":"ConfigMap","data":{"normal":"visible"}}]}`)
	got := formatBody(body, "application/json", 10)
	for _, secret := range []string{"secret-base64", "secret-string", "secret-case", "secret-separator"} {
		if strings.Contains(got, secret) {
			t.Errorf("Secret payload leaked %s: %s", secret, got)
		}
	}
	if !strings.Contains(got, `"metadata":{"name":"pull-secret"}`) || !strings.Contains(got, `"data":{"normal":"visible"}`) {
		t.Fatalf("noncredential resource metadata disappeared: %s", got)
	}
}

func TestPayloadCompleteJSONOnly(t *testing.T) {
	for _, tc := range []struct {
		name, body, contentType, want string
	}{
		{"empty", "", "application/json", "[empty]"},
		{"object", ` {"number":9007199254740993123456789, "ok":true} `, "application/json; charset=utf-8", `{"number":9007199254740993123456789,"ok":true}`},
		{"suffix", `{"roleRef":{"name":"cluster-admin"},"subjects":[{"kind":"User","name":"alice@example.test"}],"kind":"RoleBinding"}`, "application/vnd.kubernetes+json", `{"kind":"RoleBinding","roleRef":{"name":"cluster-admin"},"subjects":[{"kind":"User","name":"alice@example.test"}]}`},
		{"malformed", `{"password":"never-print-me"`, "application/json", "[omitted:"},
		{"trailing", `{"ok":1} {"password":"never-print-me"}`, "application/json", "[omitted:"},
		{"whitespace", " \n ", "application/json", "[omitted:"},
		{"text", "never-print-me", "text/plain", "[omitted:"},
		{"missingType", `{"password":"never-print-me"}`, "", "[omitted:"},
		{"ndjson", "{\"password\":\"never-print-me\"}\n", "application/x-ndjson", "[omitted:"},
		{"eventStream", "data: never-print-me", "text/event-stream", "[omitted:"},
		{"streamJSON", `{"password":"never-print-me"}`, "application/stream+json", "[omitted:"},
		{"badType", `{"password":"never-print-me"}`, "application/json; not a parameter", "[omitted:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := formatBody([]byte(tc.body), tc.contentType, 10)
			if tc.want == "[omitted:" {
				if !strings.HasPrefix(got, tc.want) || strings.Contains(got, "never-print-me") {
					t.Fatalf("expected safe omission, got %q", got)
				}
			} else if got != tc.want {
				t.Fatalf("formatBody = %q, want %q", got, tc.want)
			}
		})
	}
	for _, contentType := range []string{"application/json", "Application/JSON; charset=UTF-8", "application/merge-patch+json", "application/vnd.api+json"} {
		if !isJSONBody(contentType) {
			t.Errorf("JSON content type rejected: %s", contentType)
		}
	}
	for _, contentType := range []string{"", "text/plain", "application/x-ndjson", "text/event-stream", "application/stream+json", "application/json-seq", "text/json", "application/+json"} {
		if isJSONBody(contentType) {
			t.Errorf("unsupported content type accepted: %s", contentType)
		}
	}
}

// TestCurlPayloadShellQuotingAndRedaction checks shell quoting and masking without exposing
// or replaying request bodies.
func TestCurlPayloadShellQuotingAndRedaction(t *testing.T) {
	req, err := http.NewRequest(http.MethodPatch, "https://user:password@example.test/v1/rolebindings?token=url-secret&limit=2#fragment-secret", strings.NewReader("body-secret"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = http.Header{
		"Authorization":    {"Bearer auth-secret"},
		"Content-Type":     {"application/json"},
		"User-Agent":       {"agent's $(echo bad); value"},
		"X-New-Credential": {"unknown-secret"},
	}
	beforeHeaders, beforeURL := req.Header.Clone(), req.URL.String()
	got := curlCommand(req)
	want := `curl -X 'PATCH' -H 'Authorization: REDACTED' -H 'Content-Type: application/json' -H 'User-Agent: agent'"'"'s $(echo bad); value' -H 'X-New-Credential: REDACTED' 'https://REDACTED/v1/rolebindings?token=REDACTED&limit=2'`
	if got != want {
		t.Fatalf("curl command = %s, want %s", got, want)
	}
	for _, secret := range []string{"password", "url-secret", "fragment-secret", "body-secret", "auth-secret", "unknown-secret"} {
		if strings.Contains(got, secret) {
			t.Errorf("curl leaked %q", secret)
		}
	}
	if !reflect.DeepEqual(req.Header, beforeHeaders) || req.URL.String() != beforeURL {
		t.Fatal("curl formatting mutated request")
	}
	if got := curlCommand(nil); got != "[omitted: nil request]" {
		t.Fatalf("nil request = %q", got)
	}
}

func TestHeadersPayloadAllowlistAndDeterminism(t *testing.T) {
	headers := http.Header{
		"Authorization":       {"Bearer auth-secret"},
		"Proxy-Authorization": {"Basic proxy-secret"},
		"Cookie":              {"session=cookie-secret"},
		"Set-Cookie":          {"session=response-secret"},
		"X-Api-Key":           {"api-secret"},
		"X-Unknown":           {"future-secret", "other-secret"},
		"Accept":              {"application/json"},
		"accept":              {"text/plain"},
		"Content-Type":        {"application/json"},
		"Content-Length":      {"123"},
		"User-Agent":          {"gcphcpctl/1.0"},
		"Date":                {"Mon, 02 Jan 2006 15:04:05 GMT"},
		"Etag":                {`"resource-version"`},
		"Last-Modified":       {"Mon, 02 Jan 2006 15:04:05 GMT"},
		"Retry-After":         {"5"},
		"X-Request-Id":        {"request-123"},
	}
	original := headers.Clone()
	got := headerText(headers)
	var decoded map[string][]string
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("headerText must be JSON: %q: %v", got, err)
	}
	for _, key := range []string{"Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie", "X-Api-Key", "X-Unknown"} {
		for _, value := range decoded[key] {
			if value != "REDACTED" {
				t.Errorf("%s leaked %q", key, value)
			}
		}
		if len(decoded[key]) != len(headers[key]) {
			t.Errorf("%s value count changed", key)
		}
	}
	if !reflect.DeepEqual(decoded["Accept"], []string{"application/json", "text/plain"}) {
		t.Errorf("case-variant values not combined deterministically: %v", decoded["Accept"])
	}
	for _, key := range []string{"Content-Type", "Content-Length", "User-Agent", "Date", "Etag", "Last-Modified", "Retry-After", "X-Request-Id"} {
		if !reflect.DeepEqual(decoded[key], headers[key]) {
			t.Errorf("safe header %s changed: %v", key, decoded[key])
		}
	}
	for range 20 {
		if next := headerText(headers); next != got {
			t.Fatalf("nondeterministic headers: %s != %s", next, got)
		}
	}
	if !reflect.DeepEqual(headers, original) {
		t.Fatal("original headers mutated")
	}
	if got := headerText(nil); got != "{}" {
		t.Fatalf("nil headers = %q, want {}", got)
	}
}
