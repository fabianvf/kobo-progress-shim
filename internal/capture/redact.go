package capture

import (
	"net/http"
	"regexp"
	"strings"
)

const redacted = "[redacted]"

// safeHeaders is an allowlist so that an unexpected auth header stays out of
// the capture by default. Other headers are logged by name only.
var safeHeaders = map[string]bool{
	"Accept":              true,
	"Accept-Encoding":     true,
	"Content-Encoding":    true,
	"Content-Length":      true,
	"Content-Type":        true,
	"Location":            true,
	"User-Agent":          true,
	"X-Forwarded-Host":    true,
	"X-Forwarded-Proto":   true,
	"X-Kobo-Recent-Reads": true,
	"X-Kobo-Sync":         true,
	"X-Kobo-Sync-Mode":    true,
}

var (
	// CWA's auth token is a path segment, and appears in the download and
	// cover URLs it returns.
	koboPathToken = regexp.MustCompile(`/kobo/[^/?#"\s]+`)
	queryValue    = regexp.MustCompile(`([?&][^=&#"\s]+=)[^&#"\s]*`)
	sensitiveKey  = regexp.MustCompile(`(?i)token|secret|password|passwd|auth|cookie|credential|signature|session|userkey|apikey|api_key|email|userid|user_id|deviceid|device_id|serial`)
)

func redactHeaders(header http.Header) map[string][]string {
	out := make(map[string][]string, len(header))
	for name, values := range header {
		if !safeHeaders[http.CanonicalHeaderKey(name)] {
			out[name] = []string{redacted}
			continue
		}
		clean := make([]string, len(values))
		for i, value := range values {
			clean[i] = redactString(value)
		}
		out[name] = clean
	}
	return out
}

func redactString(s string) string {
	s = koboPathToken.ReplaceAllString(s, "/kobo/"+redacted)
	if strings.Contains(s, "://") || strings.HasPrefix(s, "/") {
		s = queryValue.ReplaceAllString(s, "${1}"+redacted)
	}
	return s
}

func redactValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			if sensitiveKey.MatchString(key) {
				v[key] = redacted
			} else {
				v[key] = redactValue(item)
			}
		}
	case []any:
		for i, item := range v {
			v[i] = redactValue(item)
		}
	case string:
		return redactString(v)
	}
	return value
}
