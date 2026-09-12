package web

import (
	"bytes"
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type pager struct {
	Page, Pages, Total, From, To int
	PreviousURL, NextURL         string
	HasPrevious, HasNext         bool
}

func pageNumber(r *http.Request) int {
	page, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || page < 1 {
		return 1
	}
	return page
}

// newPager clamps the requested page and builds previous/next links that keep
// the request's other query parameters.
func newPager(r *http.Request, requested, total, pageSize int) pager {
	pages := max((total+pageSize-1)/pageSize, 1)
	page := min(requested, pages)
	result := pager{Page: page, Pages: pages, Total: total, HasPrevious: page > 1, HasNext: page < pages}
	if total > 0 {
		result.From = (page-1)*pageSize + 1
		result.To = min(page*pageSize, total)
	}
	pageURL := func(value int) string {
		query := r.URL.Query()
		query.Set("page", strconv.Itoa(value))
		return r.URL.Path + "?" + query.Encode()
	}
	if result.HasPrevious {
		result.PreviousURL = pageURL(page - 1)
	}
	if result.HasNext {
		result.NextURL = pageURL(page + 1)
	}
	return result
}

func pageSlice[T any](items []T, pagination pager, pageSize int) []T {
	if len(items) == 0 {
		return items
	}
	start := (pagination.Page - 1) * pageSize
	return items[start:min(start+pageSize, len(items))]
}

func containsFold(value, query string) bool {
	return strings.Contains(strings.ToLower(value), strings.ToLower(query))
}

func validHTTPURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != ""
}

func jsonOrDefault(value, fallback string) json.RawMessage {
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	return json.RawMessage(value)
}

func validJSONObject(raw json.RawMessage) bool {
	var value map[string]any
	return json.Unmarshal(raw, &value) == nil && value != nil
}

func validStringMap(raw json.RawMessage) bool {
	var value map[string]string
	return json.Unmarshal(raw, &value) == nil
}

func validURLTemplate(value string) bool {
	return strings.HasPrefix(value, "/") || strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://")
}

// timeoutMS parses a seconds field bounded to one hour, in milliseconds.
func timeoutMS(value string, fallbackSeconds int) int {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds < 1 || seconds > 3600 {
		seconds = fallbackSeconds
	}
	return seconds * 1000
}

// shorten truncates on a rune boundary and appends an ellipsis.
func shorten(value string, limit int) string {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return strings.TrimSpace(string(runes[:limit])) + "…"
}

func prettyJSON(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return string(raw)
	}
	return out.String()
}

func functions() template.FuncMap {
	return template.FuncMap{
		"date": func(value *time.Time) string {
			if value == nil {
				return "—"
			}
			return value.Local().Format("2006-01-02 15:04")
		},
		"time":     func(value time.Time) string { return value.Local().Format("2006-01-02 15:04:05") },
		"status":   statusLabel,
		"badge":    statusClass,
		"decision": decisionClass,
		"kind":     kindLabel,
		"auth":     authLabel,
		"runtime":  runtimeLabel,
		"short":    func(value string) string { return shorten(value, 140) },
		"json":     prettyJSON,
		"args": func(raw json.RawMessage) string {
			var values []string
			if json.Unmarshal(raw, &values) != nil {
				return string(raw)
			}
			return strings.Join(values, " ")
		},
		"seconds": func(ms int) int { return ms / 1000 },
		"plural": func(count int, singular, plural string) string {
			if count == 1 {
				return singular
			}
			return plural
		},
		"query": template.URLQueryEscaper,
	}
}

func statusLabel(value string) string {
	switch value {
	case "reauthorization_required":
		return "Needs authorization"
	case "active":
		return "Active"
	case "connected":
		return "Connected"
	case "unchecked":
		return "Not checked"
	case "checking":
		return "Checking"
	case "degraded":
		return "Degraded"
	case "unreachable":
		return "Unreachable"
	case "disabled":
		return "Disabled"
	default:
		return strings.ReplaceAll(value, "_", " ")
	}
}

// statusClass maps a connection or agent status to a badge tone.
func statusClass(value string) string {
	switch value {
	case "connected", "active":
		return "ok"
	case "degraded", "checking", "unchecked":
		return "warn"
	case "unreachable", "reauthorization_required":
		return "bad"
	default:
		return "muted"
	}
}

func decisionClass(value string) string {
	switch value {
	case "allowed":
		return "ok"
	case "denied":
		return "bad"
	case "error":
		return "warn"
	default:
		return "muted"
	}
}

func kindLabel(value string) string {
	switch value {
	case "mcp_http":
		return "Remote MCP"
	case "mcp_stdio":
		return "Local MCP"
	case "http_api":
		return "HTTP API"
	case "mcp":
		return "MCP"
	case "http":
		return "HTTP"
	case "command":
		return "Command"
	default:
		return value
	}
}

func authLabel(value string) string {
	switch value {
	case "none":
		return "None"
	case "bearer":
		return "Bearer token"
	case "header":
		return "API key header"
	case "oauth2":
		return "OAuth 2.0"
	default:
		return value
	}
}

func runtimeLabel(value string) string {
	switch strings.ToLower(value) {
	case "hermes":
		return "Hermes"
	case "openclaw":
		return "OpenClaw"
	case "codex":
		return "Codex"
	case "claude":
		return "Claude Code"
	case "":
		return "Manual"
	default:
		return value
	}
}
