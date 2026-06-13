// MCP output redaction helpers.

package server

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

const redactedValue = "[redacted]"

var mcpURLUserinfoPattern = regexp.MustCompile(`(https?://)[^\s/@:]+:[^\s/@]+@`)

var mcpSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`ghp_[A-Za-z0-9_]+`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]+`),
	regexp.MustCompile(`glpat-[A-Za-z0-9_\-]+`),
	regexp.MustCompile(`sk-[A-Za-z0-9_\-]+`),
	regexp.MustCompile(`sk-or-[A-Za-z0-9_\-]+`),
	regexp.MustCompile(`AIza[A-Za-z0-9_\-]+`),
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9_\-]+`),
	regexp.MustCompile(`(?i)(Authorization:\s*Bearer\s+)[A-Za-z0-9._\-]+`),
	regexp.MustCompile(`(?i)([A-Z0-9_]*(?:TOKEN|SECRET|PASSWORD|API_KEY)\s*=\s*)[^\s]+`),
}

func redactString(s string) string {
	for _, pattern := range mcpSecretPatterns {
		s = pattern.ReplaceAllStringFunc(s, func(match string) string {
			if strings.Contains(match, "=") {
				prefix, _, _ := strings.Cut(match, "=")
				return prefix + "=" + redactedValue
			}
			if strings.Contains(strings.ToLower(match), "authorization:") {
				return "Authorization: Bearer " + redactedValue
			}
			return redactedValue
		})
	}
	return redactURLUserinfo(s)
}

func redactURLUserinfo(s string) string {
	fields := strings.Fields(s)
	if len(fields) != 1 {
		return mcpURLUserinfoPattern.ReplaceAllString(s, "${1}"+redactedValue+"@")
	}
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return mcpURLUserinfoPattern.ReplaceAllString(s, "${1}"+redactedValue+"@")
	}
	u.User = url.User(redactedValue)
	return u.String()
}

func redactAny(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, value := range t {
			if isSecretKey(k) {
				out[k] = redactedValue
				continue
			}
			out[k] = redactAny(value)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, value := range t {
			out[i] = redactAny(value)
		}
		return out
	case string:
		return redactString(t)
	default:
		return v
	}
}

func redactForJSON(v any) any {
	data, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return v
	}
	return redactAny(decoded)
}

func isSecretKey(key string) bool {
	key = strings.ToLower(key)
	for _, marker := range []string{"token", "secret", "password", "authorization", "api_key", "apikey", "access_token", "refresh_token"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}
