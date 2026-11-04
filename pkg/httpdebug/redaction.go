package httpdebug

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
)

// safeURL retains schemes and resource paths, but masks URL authorities (hosts
// and ports) and removes userinfo and fragments. It never mutates the request.
// Query values are redacted by default: only bounded numeric parameters and
// selected boolean/enum values are retained. Unknown future credentials therefore
// do not need to be added to a denylist before they are safe to trace.
func safeURL(original *url.URL) string {
	if original == nil || original.Opaque != "" {
		return "REDACTED"
	}
	u := *original
	u.User = nil
	if u.Host != "" {
		u.Host = "REDACTED"
	}
	u.Fragment = ""
	u.RawFragment = ""
	// Workflow callback identifiers are capability-like, unlike resource names.
	if before, _, found := strings.Cut(u.Path, "/callbacks/"); found {
		u.Path = before + "/callbacks/REDACTED"
		u.RawPath = ""
	}
	if u.RawQuery != "" {
		if _, err := url.ParseQuery(u.RawQuery); err != nil {
			// Do not accidentally print a partially parsed credential.
			u.RawQuery = "REDACTED"
		} else {
			parts := strings.Split(u.RawQuery, "&")
			for i, part := range parts {
				key, value, _ := strings.Cut(part, "=")
				decodedKey, _ := url.QueryUnescape(key)
				decodedValue, _ := url.QueryUnescape(value)
				if !safeQueryValue(decodedKey, decodedValue) {
					parts[i] = key + "=REDACTED"
				}
			}
			u.RawQuery = strings.Join(parts, "&")
		}
	}
	return u.String()
}

func safeQueryValue(key, value string) bool {
	switch key {
	case "limit", "pageSize", "maxResults", "timeoutSeconds":
		if len(value) == 0 || len(value) > 10 {
			return false
		}
		for _, c := range value {
			if c < '0' || c > '9' {
				return false
			}
		}
		return true
	case "watch", "pretty", "allowWatchBookmarks":
		return value == "true" || value == "false"
	case "alt":
		return value == "json" || value == "media"
	default:
		return false
	}
}

// errorLabel deliberately excludes error prose, which may embed URLs, tokens or
// remote response text. The original error is still returned to the caller.
func errorLabel(err error) string {
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return "timeout"
	}
	return "transport_error"
}
