package httpdebug

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// isJSONBody accepts complete JSON documents, not streaming JSON protocols.
func isJSONBody(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType == "application/stream+json" {
		return false
	}
	return mediaType == "application/json" ||
		(strings.HasPrefix(mediaType, "application/") &&
			len(mediaType) > len("application/+json") && strings.HasSuffix(mediaType, "+json"))
}

// formatBody sanitizes a complete document before applying verbosity limits.
// A truncated result is a UTF-8 JSON prefix plus an explicit omission marker,
// not a complete JSON document. The marker counts bytes of sanitized output.
func formatBody(data []byte, contentType string, level int) string {
	if len(data) == 0 {
		return "[empty]"
	}
	if level < 8 {
		return "[omitted: verbosity below 8]"
	}
	if !isJSONBody(contentType) {
		return "[omitted: unsupported content type]"
	}
	value, ok := decodeBodyJSON(data)
	if !ok {
		return "[omitted: malformed JSON]"
	}
	result, err := json.Marshal(sanitizeJSON(value, 0))
	if err != nil {
		return "[omitted: invalid JSON]"
	}
	limit := len(result)
	if level == 8 {
		limit = 1024
	} else if level == 9 {
		limit = 10240
	}
	if len(result) > limit {
		for !utf8.RuneStart(result[limit]) {
			limit--
		}
		return string(result[:limit]) + fmt.Sprintf("[truncated %d bytes]", len(result)-limit)
	}
	return string(result)
}

func decodeBodyJSON(data []byte) (any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := readBodyJSONValue(decoder, 0)
	if err != nil {
		return nil, false
	}
	_, err = decoder.Token()
	return value, err == io.EOF
}

// Read objects without losing sensitivity to duplicate JSON keys. Once any
// discriminator identifies credentials, later duplicates cannot undo redaction.
func readBodyJSONValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 100 {
		return nil, errors.New("JSON nesting limit exceeded")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, container := token.(json.Delim)
	if !container {
		return token, nil
	}
	switch delim {
	case '{':
		result := make(map[string]any)
		secret := false
		secretList := false
		sensitiveValue := false
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("invalid JSON object key")
			}
			value, err := readBodyJSONValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			if kind, ok := value.(string); ok && normalizedField(key) == "kind" {
				secret = secret || strings.EqualFold(kind, "Secret")
				secretList = secretList || strings.EqualFold(kind, "SecretList")
			}
			sensitiveValue = sensitiveValue || credentialValueDiscriminator(key, value)
			result[key] = value
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		if secret {
			redactSecretData(result)
		}
		if secretList || sensitiveValue {
			for key := range result {
				field := normalizedField(key)
				if sensitiveValue && field == "value" {
					result[key] = "REDACTED"
				}
				if secretList && field == "items" {
					items, ok := result[key].([]any)
					if !ok {
						result[key] = "REDACTED"
						continue
					}
					for i, item := range items {
						if fields, ok := item.(map[string]any); ok {
							redactSecretData(fields)
						} else {
							items[i] = "REDACTED"
						}
					}
				}
			}
		}
		return result, nil
	case '[':
		result := make([]any, 0)
		for decoder.More() {
			value, err := readBodyJSONValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
		_, err := decoder.Token()
		return result, err
	default:
		return nil, errors.New("unexpected JSON delimiter")
	}
}

// SecretList items often omit kind; the enclosing list still makes their data
// sensitive, even if an item claims another kind or repeats a data field.
func redactSecretData(fields map[string]any) {
	for key := range fields {
		if field := normalizedField(key); field == "data" || field == "stringdata" {
			fields[key] = "REDACTED"
		}
	}
}

func sanitizeJSON(value any, depth int) any {
	if depth > 100 {
		return "[omitted: JSON nesting limit]"
	}
	switch typed := value.(type) {
	case map[string]any:
		sensitiveValue := indirectCredentialValue(typed)
		for key, item := range typed {
			field := normalizedField(key)
			if credentialField(field) || (sensitiveValue && field == "value") {
				typed[key] = "REDACTED"
			} else {
				typed[key] = sanitizeJSON(item, depth+1)
			}
		}
	case []any:
		for i, item := range typed {
			typed[i] = sanitizeJSON(item, depth+1)
		}
	case string:
		return safePayloadString(typed, depth)
	}
	return value
}

var (
	privateKeyText = regexp.MustCompile(`(?i)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	authText       = regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+\S+`)
	// Scheme-relative references have authorities and credentials too.
	payloadURLText = regexp.MustCompile(`(?i)(?:[a-z][a-z0-9+.-]*:)?//\S+`)
	// A whitespace-delimited URL prefix can parse as a host/port even when
	// the remaining text is userinfo. Never retain that ambiguous suffix.
	ambiguousURLUserinfo = regexp.MustCompile(`(?i)(?:[a-z][a-z0-9+.-]*:)?//[^/?#]*\s[^/?#]*@`)
)

// safePayloadString masks credentials and URL authorities, including scheme-relative
// references, before payload and allowlisted-header values reach trace output.
func safePayloadString(value string, depth int) string {
	if privateKeyText.MatchString(value) || authText.MatchString(value) || ambiguousURLUserinfo.MatchString(value) {
		return "REDACTED"
	}
	trimmed := strings.TrimSpace(value)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, `"`) {
		embedded, ok := decodeBodyJSON([]byte(trimmed))
		if !ok {
			return "[omitted: malformed embedded JSON]"
		}
		result, err := json.Marshal(sanitizeJSON(embedded, depth+1))
		if err != nil {
			return "[omitted: invalid embedded JSON]"
		}
		return string(result)
	}
	// ListCallbacks returns relative resource names, not just absolute URLs.
	// Parse those paths too so encoded callback segments use the same mask.
	if strings.HasPrefix(trimmed, "projects/") || strings.HasPrefix(trimmed, "/projects/") {
		u, err := url.Parse(trimmed)
		if err != nil {
			return "REDACTED"
		}
		if strings.Contains(u.Path, "/callbacks/") {
			return safeURL(u)
		}
	}
	invalidURL := false
	result := payloadURLText.ReplaceAllStringFunc(value, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			invalidURL = true
			return "REDACTED"
		}
		return safeURL(u)
	})
	if invalidURL {
		return "REDACTED"
	}
	return result
}

func normalizedField(key string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, key)
}

// JSON Patch and name/value pairs identify credentials outside the value key.
func indirectCredentialValue(fields map[string]any) bool {
	for key, item := range fields {
		if credentialValueDiscriminator(key, item) {
			return true
		}
	}
	return false
}

func credentialValueDiscriminator(key string, item any) bool {
	value, ok := item.(string)
	if !ok {
		return false
	}
	switch normalizedField(key) {
	case "name", "key":
		return credentialField(normalizedField(value))
	case "path":
		path, err := url.PathUnescape(value)
		if err != nil {
			return true
		}
		for _, segment := range strings.Split(path, "/") {
			segment = strings.ReplaceAll(strings.ReplaceAll(segment, "~1", "/"), "~0", "~")
			field := normalizedField(segment)
			if credentialField(field) || field == "data" || field == "stringdata" {
				return true
			}
		}
	}
	return false
}

func credentialField(field string) bool {
	if field == "auth" || field == "auths" {
		return true
	}
	for _, sensitive := range []string{"authorization", "cookie", "password", "passwd", "passphrase", "token", "secret", "privatekey", "clientkey", "apikey", "accesskey", "signingkey", "serviceaccountkey", "serviceaccountjson", "credential", "kubeconfig", "dockerconfig", "dockercfg"} {
		if strings.Contains(field, sensitive) {
			return true
		}
	}
	return false
}

// curlCommand describes the request without consuming, including, or replaying
// its body. Credentials remain masked even at the highest verbosity.
func curlCommand(req *http.Request) string {
	if req == nil {
		return "[omitted: nil request]"
	}
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	parts := []string{"curl", "-X", shellQuote(method)}
	headers := safeHeaders(req.Header)
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		for _, value := range headers[key] {
			parts = append(parts, "-H", shellQuote(key+": "+value))
		}
	}
	parts = append(parts, shellQuote(safeURL(req.URL)))
	return strings.Join(parts, " ")
}

func shellQuote(value string) string {
	// Shell arguments cannot contain NUL. Replace invalid UTF-8 and controls as
	// well, so a diagnostic remains one line even for an invalid request.
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return unicode.ReplacementChar
		}
		return r
	}, value)
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// headerText emits compact, deterministic JSON without modifying the headers.
// New or unrecognized headers are redacted rather than trusting a denylist.
func headerText(h http.Header) string {
	data, _ := json.Marshal(safeHeaders(h))
	return string(data)
}

func safeHeaders(h http.Header) http.Header {
	result := make(http.Header)
	keys := make([]string, 0, len(h))
	for key := range h {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		name := http.CanonicalHeaderKey(key)
		if _, exists := result[name]; !exists {
			result[name] = []string{}
		}
		for _, value := range h[key] {
			if !safeHeaderName(name) {
				value = "REDACTED"
			} else {
				value = safePayloadString(value, 0)
			}
			result[name] = append(result[name], value)
		}
	}
	return result
}

func safeHeaderName(name string) bool {
	switch strings.ToLower(name) {
	case "accept", "accept-encoding", "accept-language", "content-type",
		"content-length", "content-encoding", "user-agent", "date", "etag",
		"last-modified", "retry-after", "request-id", "x-request-id",
		"x-correlation-id", "x-goog-request-id", "audit-id":
		return true
	default:
		return false
	}
}
