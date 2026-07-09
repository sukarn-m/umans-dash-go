package proxy

import (
	"encoding/json"
	"strings"
)

// ─── Error Logging (§16) ────────────────────────────────────────────────────

// ─── Error Logging (§16) ────────────────────────────────────────────────────

func RedactHeaders(headers map[string][]string) map[string][]string {
	result := map[string][]string{}
	for k, v := range headers {
		lower := strings.ToLower(k)
		sensitive := lower == "authorization" || lower == "x-api-key" || lower == "cookie" || lower == "set-cookie" || lower == "api-key"
		if !sensitive {
			if strings.Contains(lower, "auth") || strings.Contains(lower, "token") || strings.Contains(lower, "key") || strings.Contains(lower, "password") || strings.Contains(lower, "secret") {
				sensitive = true
			}
		}
		if sensitive {
			result[k] = []string{"[REDACTED]"}
		} else {
			result[k] = v
		}
	}
	return result
}

func RedactBodyJson(body string) string {
	if body == "" {
		return ""
	}
	var parsed interface{}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return body
	}
	walked := redactWalk(parsed)
	b, err := json.MarshalIndent(walked, "", "  ")
	if err != nil {
		return "[unserializable]"
	}
	return string(b)
}

func redactWalk(o interface{}) interface{} {
	if m, ok := o.(map[string]interface{}); ok {
		result := map[string]interface{}{}
		for k, v := range m {
			lower := strings.ToLower(k)
			if lower == "api_key" || lower == "apikey" || strings.Contains(lower, "token") || strings.Contains(lower, "password") || strings.Contains(lower, "secret") || strings.Contains(lower, "authorization") {
				result[k] = "[REDACTED]"
			} else if k == "messages" {
				if arr, ok := v.([]interface{}); ok {
					result[k] = redactWalk(arr)
				} else {
					result[k] = redactWalk(v)
				}
			} else if k == "content" {
				if s, ok := v.(string); ok && len(s) > 2000 {
					result[k] = s[:2000] + "...[truncated]"
				} else {
					result[k] = redactWalk(v)
				}
			} else {
				result[k] = redactWalk(v)
			}
		}
		return result
	}
	if arr, ok := o.([]interface{}); ok {
		result := make([]interface{}, len(arr))
		for i, v := range arr {
			result[i] = redactWalk(v)
		}
		return result
	}
	return o
}
