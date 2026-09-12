package execute

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/shero4/toolmux/internal/mcp"
	"github.com/shero4/toolmux/internal/store"
)

var placeholder = regexp.MustCompile(`\$\{([A-Za-z0-9_]+)\}`)

type Router struct {
	store *store.Store
	mcp   *mcp.Client
	http  *http.Client
}

func New(store *store.Store, mcpClient *mcp.Client) *Router {
	return &Router{store: store, mcp: mcpClient, http: &http.Client{CheckRedirect: sameHostRedirect}}
}

// sameHostRedirect follows redirects only within the original host so a
// connection credential is never replayed to a third party.
func sameHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if req.URL.Host != via[0].URL.Host {
		return errors.New("refusing to follow a redirect to a different host")
	}
	return nil
}

func (r *Router) Call(ctx context.Context, tool store.Tool, connection store.Connection, credential store.Credential, call store.ToolCall) (json.RawMessage, error) {
	switch tool.Kind {
	case "mcp":
		if connection.Kind == "mcp_stdio" {
			spec, err := r.store.GetMCPStdioSpec(ctx, connection.ID)
			if err != nil {
				return nil, err
			}
			return r.mcp.CallStdio(ctx, spec, credential, tool.UpstreamName, call)
		}
		return r.mcp.Call(ctx, connection, credential, tool.UpstreamName, call, tool.InputSchema)
	case "http":
		return r.callHTTP(ctx, tool, connection, credential, call.Arguments)
	case "command":
		return r.callCommand(ctx, tool, credential, call.Arguments)
	default:
		return nil, fmt.Errorf("unsupported tool kind %q", tool.Kind)
	}
}

func (r *Router) Check(ctx context.Context, connection store.Connection, credential store.Credential) (store.Check, error) {
	switch connection.Kind {
	case "mcp_http":
		tools, err := r.mcp.Discover(ctx, connection, credential)
		if err != nil {
			return store.Check{}, err
		}
		if err := r.store.ReconcileTools(ctx, connection, tools); err != nil {
			return store.Check{Status: "degraded", Reachable: true, ProtocolOK: true, Authorized: true, Detail: "Tool catalog could not be saved."}, nil
		}
		return store.Check{Status: "connected", Reachable: true, ProtocolOK: true, Authorized: true, CapabilityOK: true, ToolCount: len(tools)}, nil
	case "mcp_stdio":
		spec, err := r.store.GetMCPStdioSpec(ctx, connection.ID)
		if err != nil {
			return store.Check{}, err
		}
		tools, err := r.mcp.DiscoverStdio(ctx, spec, credential)
		if err != nil {
			return store.Check{}, err
		}
		if err := r.store.ReconcileTools(ctx, connection, tools); err != nil {
			return store.Check{Status: "degraded", Reachable: true, ProtocolOK: true, Authorized: true, Detail: "Tool catalog could not be saved."}, nil
		}
		return store.Check{Status: "connected", Reachable: true, ProtocolOK: true, Authorized: true, CapabilityOK: true, ToolCount: len(tools)}, nil
	case "http_api":
		return r.checkHTTP(ctx, connection, credential)
	case "command":
		return r.checkCommand(ctx, connection)
	default:
		return store.Check{}, fmt.Errorf("unsupported connection kind %q", connection.Kind)
	}
}

func (r *Router) callHTTP(ctx context.Context, tool store.Tool, connection store.Connection, credential store.Credential, raw json.RawMessage) (json.RawMessage, error) {
	spec, err := r.store.GetHTTPToolSpec(ctx, tool.ID)
	if err != nil {
		return nil, err
	}
	args, err := decodeArguments(raw)
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(connection.EndpointURL)
	if err != nil {
		return nil, err
	}
	location, err := renderString(spec.URLTemplate, args, true)
	if err != nil {
		return nil, err
	}
	target, err := base.Parse(location)
	if err != nil {
		return nil, err
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return nil, errors.New("HTTP tool URL must use http or https")
	}
	query, err := renderStringMap(spec.QueryTemplate, args, false)
	if err != nil {
		return nil, err
	}
	values := target.Query()
	for key, value := range query {
		values.Set(key, value)
	}
	target.RawQuery = values.Encode()
	var body io.Reader
	if string(spec.BodyTemplate) != "null" {
		var template any
		if err := json.Unmarshal(spec.BodyTemplate, &template); err != nil {
			return nil, err
		}
		rendered, err := renderJSON(template, args)
		if err != nil {
			return nil, err
		}
		data, err := json.Marshal(rendered)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(spec.TimeoutMS)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, spec.Method, target.String(), body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	headers, err := renderStringMap(spec.HeadersTemplate, args, false)
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	applyCredential(req, connection, credential)
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP tool request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(spec.MaxResponseBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > spec.MaxResponseBytes {
		return nil, errors.New("HTTP tool response exceeded its configured limit")
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, mcp.ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP tool returned %s: %s", resp.Status, truncate(string(data), 512))
	}
	return toolResult(data), nil
}

func (r *Router) callCommand(ctx context.Context, tool store.Tool, credential store.Credential, raw json.RawMessage) (json.RawMessage, error) {
	spec, err := r.store.GetCommandToolSpec(ctx, tool.ID)
	if err != nil {
		return nil, err
	}
	args, err := decodeArguments(raw)
	if err != nil {
		return nil, err
	}
	var templates []string
	if err := json.Unmarshal(spec.ArgsTemplate, &templates); err != nil {
		return nil, err
	}
	rendered := make([]string, len(templates))
	for i, value := range templates {
		rendered[i], err = renderString(value, args, false)
		if err != nil {
			return nil, err
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(spec.TimeoutMS)*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(callCtx, spec.Executable, rendered...)
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = spec.WorkingDirectory
	cmd.Env = os.Environ()
	if spec.CredentialEnv != "" && credential.Bearer() != "" {
		cmd.Env = setEnv(cmd.Env, spec.CredentialEnv, credential.Bearer())
	}
	if spec.StdinMode == "json" {
		cmd.Stdin = bytes.NewReader(raw)
	}
	stdout := &limitedBuffer{limit: spec.MaxOutputBytes}
	stderr := &limitedBuffer{limit: 64 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		if callCtx.Err() != nil {
			return nil, fmt.Errorf("command tool: %w", callCtx.Err())
		}
		return nil, fmt.Errorf("command tool failed: %w: %s", err, truncate(stderr.String(), 512))
	}
	if stdout.exceeded {
		return nil, errors.New("command output exceeded its configured limit")
	}
	return toolResult(stdout.Bytes()), nil
}

func (r *Router) checkHTTP(ctx context.Context, connection store.Connection, credential store.Credential) (store.Check, error) {
	base, err := url.Parse(connection.EndpointURL)
	if err != nil {
		return store.Check{}, err
	}
	path := connection.HealthPath
	if path == "" {
		path = "/"
	}
	target, err := base.Parse(path)
	if err != nil || target.Host != base.Host || target.Scheme != base.Scheme {
		return store.Check{}, errors.New("invalid health path")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, target.String(), nil)
	if err != nil {
		return store.Check{}, err
	}
	applyCredential(req, connection, credential)
	resp, err := r.http.Do(req)
	if err != nil {
		return store.Check{}, err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return store.Check{}, mcp.ErrUnauthorized
	}
	if resp.StatusCode >= 500 {
		return store.Check{}, fmt.Errorf("health endpoint returned %s", resp.Status)
	}
	count, err := r.store.CountToolsForConnection(ctx, connection.ID)
	if err != nil {
		return store.Check{}, err
	}
	return store.Check{Status: "connected", Reachable: true, ProtocolOK: true, Authorized: true, CapabilityOK: true, ToolCount: count}, nil
}

func (r *Router) checkCommand(ctx context.Context, connection store.Connection) (store.Check, error) {
	specs, err := r.store.CommandSpecsForConnection(ctx, connection.ID)
	if err != nil {
		return store.Check{}, err
	}
	for _, spec := range specs {
		if _, err := exec.LookPath(spec.Executable); err != nil {
			return store.Check{}, fmt.Errorf("executable %q is unavailable", spec.Executable)
		}
	}
	return store.Check{Status: "connected", Reachable: true, ProtocolOK: true, Authorized: true, CapabilityOK: true, ToolCount: len(specs)}, nil
}

func applyCredential(req *http.Request, connection store.Connection, credential store.Credential) {
	token := credential.Bearer()
	if token == "" || connection.AuthMethod == "none" {
		return
	}
	if connection.AuthMethod == "header" {
		req.Header.Set(connection.AuthName, token)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
}

func setEnv(environment []string, name, value string) []string {
	prefix := name + "="
	result := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if !strings.HasPrefix(item, prefix) {
			result = append(result, item)
		}
	}
	return append(result, prefix+value)
}

func decodeArguments(raw json.RawMessage) (map[string]any, error) {
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, errors.New("tool arguments must be a JSON object")
	}
	return args, nil
}

func renderString(value string, args map[string]any, escapePath bool) (string, error) {
	var renderErr error
	result := placeholder.ReplaceAllStringFunc(value, func(token string) string {
		name := placeholder.FindStringSubmatch(token)[1]
		value, ok := args[name]
		if !ok {
			renderErr = fmt.Errorf("missing input %q", name)
			return ""
		}
		text := fmt.Sprint(value)
		if escapePath {
			return url.PathEscape(text)
		}
		return text
	})
	return result, renderErr
}

func renderStringMap(raw json.RawMessage, args map[string]any, escapePath bool) (map[string]string, error) {
	values := map[string]string{}
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	for key, value := range values {
		rendered, err := renderString(value, args, escapePath)
		if err != nil {
			return nil, err
		}
		values[key] = rendered
	}
	return values, nil
}

func renderJSON(value any, args map[string]any) (any, error) {
	switch value := value.(type) {
	case string:
		matches := placeholder.FindStringSubmatch(value)
		if len(matches) == 2 && matches[0] == value {
			input, ok := args[matches[1]]
			if !ok {
				return nil, fmt.Errorf("missing input %q", matches[1])
			}
			return input, nil
		}
		return renderString(value, args, false)
	case []any:
		for i := range value {
			rendered, err := renderJSON(value[i], args)
			if err != nil {
				return nil, err
			}
			value[i] = rendered
		}
	case map[string]any:
		for key := range value {
			rendered, err := renderJSON(value[key], args)
			if err != nil {
				return nil, err
			}
			value[key] = rendered
		}
	}
	return value, nil
}

func toolResult(data []byte) json.RawMessage {
	result, _ := json.Marshal(map[string]any{"content": []map[string]any{{"type": "text", "text": string(data)}}})
	return result
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) > limit {
		return value[:limit]
	}
	return value
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		b.exceeded = true
		return len(data), nil
	}
	if len(data) > remaining {
		b.exceeded = true
		_, _ = b.Buffer.Write(data[:remaining])
		return len(data), nil
	}
	return b.Buffer.Write(data)
}
