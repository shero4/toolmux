package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/shero4/toolmux/internal/store"
)

var ErrUnauthorized = errors.New("upstream rejected the credential")

const (
	maxMessageSize  = 32 << 20
	maxToolPages    = 100
	upstreamTimeout = 5 * time.Minute
)

type Client struct {
	http *http.Client
	ids  atomic.Int64
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("%s (%d)", e.Message, e.Code) }

type listToolsResult struct {
	Tools      []wireTool `json:"tools"`
	NextCursor string     `json:"nextCursor"`
}

type wireTool struct {
	Name         string          `json:"name"`
	Title        string          `json:"title,omitempty"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Annotations  json.RawMessage `json:"annotations,omitempty"`
	Icons        json.RawMessage `json:"icons,omitempty"`
}

func NewClient() *Client {
	return &Client{http: &http.Client{Timeout: upstreamTimeout}}
}

func (c *Client) Discover(ctx context.Context, connection store.Connection, credential store.Credential) ([]store.Tool, error) {
	session, err := c.connect(ctx, connection, credential)
	if err != nil {
		return nil, err
	}
	defer session.close(ctx)
	var tools []store.Tool
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxToolPages {
			return nil, errors.New("upstream tool catalog did not end after 100 pages")
		}
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var result listToolsResult
		if err := session.call(ctx, "tools/list", params, &result); err != nil {
			return nil, err
		}
		for _, tool := range result.Tools {
			if len(tool.InputSchema) == 0 {
				tool.InputSchema = json.RawMessage(`{"type":"object"}`)
			}
			tools = append(tools, store.Tool{UpstreamName: tool.Name, Title: tool.Title, Description: tool.Description, InputSchema: tool.InputSchema, OutputSchema: tool.OutputSchema, Annotations: tool.Annotations, Icons: tool.Icons})
		}
		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}
	return tools, nil
}

func (c *Client) Call(ctx context.Context, connection store.Connection, credential store.Credential, tool string, call store.ToolCall, inputSchema json.RawMessage) (json.RawMessage, error) {
	session, err := c.connect(ctx, connection, credential)
	if err != nil {
		return nil, err
	}
	defer session.close(ctx)
	params := map[string]any{"name": tool, "arguments": json.RawMessage(call.Arguments)}
	if len(call.InputResponses) > 0 {
		params["inputResponses"] = call.InputResponses
	}
	if len(call.RequestState) > 0 {
		params["requestState"] = call.RequestState
	}
	session.parameterHeaders = parameterHeaders(inputSchema, call.Arguments)
	var result json.RawMessage
	if err := session.call(ctx, "tools/call", params, &result); err != nil {
		return nil, err
	}
	return result, nil
}

type session struct {
	client           *Client
	connection       store.Connection
	credential       store.Credential
	sessionID        string
	protocolVersion  string
	modern           bool
	parameterHeaders map[string]string
}

func (c *Client) connect(ctx context.Context, connection store.Connection, credential store.Credential) (*session, error) {
	s := &session{client: c, connection: connection, credential: credential, protocolVersion: modernProtocol, modern: true}
	var discovery json.RawMessage
	if err := s.call(ctx, "server/discover", map[string]any{}, &discovery); err == nil {
		return s, nil
	} else if errors.Is(err, ErrUnauthorized) {
		return nil, err
	}
	s = &session{client: c, connection: connection, credential: credential, protocolVersion: legacyProtocol}
	var initialized struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := s.call(ctx, "initialize", map[string]any{"protocolVersion": legacyProtocol, "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "toolmux", "version": "0.2.0"}}, &initialized); err != nil {
		return nil, err
	}
	if initialized.ProtocolVersion == "" {
		return nil, errors.New("upstream returned no protocol version")
	}
	s.protocolVersion = initialized.ProtocolVersion
	if err := s.notify(ctx, "notifications/initialized", map[string]any{}); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *session) call(ctx context.Context, method string, params any, result any) error {
	if s.modern {
		params = modernParams(params)
	}
	id := s.client.ids.Add(1)
	response, err := s.send(ctx, rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}, true)
	if err != nil {
		return err
	}
	if response.Error != nil {
		return fmt.Errorf("upstream %s: %w", method, response.Error)
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(response.Result, result); err != nil {
		return fmt.Errorf("decode upstream %s result: %w", method, err)
	}
	return nil
}

func (s *session) notify(ctx context.Context, method string, params any) error {
	_, err := s.send(ctx, rpcRequest{JSONRPC: "2.0", Method: method, Params: params}, false)
	return err
}

func modernParams(params any) map[string]any {
	result := map[string]any{}
	if params != nil {
		data, _ := json.Marshal(params)
		_ = json.Unmarshal(data, &result)
	}
	result["_meta"] = map[string]any{
		"io.modelcontextprotocol/protocolVersion":    modernProtocol,
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		"io.modelcontextprotocol/clientInfo":         map[string]string{"name": "toolmux", "version": "0.2.0"},
	}
	return result
}

func (s *session) send(ctx context.Context, payload rpcRequest, expectResponse bool) (rpcResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return rpcResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.connection.EndpointURL, bytes.NewReader(body))
	if err != nil {
		return rpcResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", s.protocolVersion)
	if s.modern {
		req.Header.Set("Mcp-Method", payload.Method)
		if name := requestName(payload.Params); name != "" {
			req.Header.Set("Mcp-Name", encodeHeaderValue(name))
		}
		for name, value := range s.parameterHeaders {
			req.Header.Set("Mcp-Param-"+name, value)
		}
	}
	if s.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", s.sessionID)
	}
	s.applyCredential(req)
	resp, err := s.client.http.Do(req)
	if err != nil {
		return rpcResponse{}, fmt.Errorf("reach upstream: %w", err)
	}
	defer resp.Body.Close()
	if value := resp.Header.Get("Mcp-Session-Id"); value != "" {
		s.sessionID = value
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return rpcResponse{}, ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return rpcResponse{}, fmt.Errorf("upstream returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if !expectResponse {
		return rpcResponse{}, nil
	}
	return decodeResponse(resp, payload.ID)
}

func requestName(params any) string {
	data, _ := json.Marshal(params)
	var value struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(data, &value)
	return value.Name
}

func parameterHeaders(schema, arguments json.RawMessage) map[string]string {
	var definition struct {
		Properties map[string]struct {
			Type   string `json:"type"`
			Header string `json:"x-mcp-header"`
		} `json:"properties"`
	}
	var values map[string]any
	if json.Unmarshal(schema, &definition) != nil || json.Unmarshal(arguments, &values) != nil {
		return nil
	}
	result := map[string]string{}
	for property, spec := range definition.Properties {
		if spec.Header == "" {
			continue
		}
		value, ok := values[property]
		if !ok {
			continue
		}
		var encoded string
		switch typed := value.(type) {
		case string:
			encoded = typed
		case bool:
			encoded = strconv.FormatBool(typed)
		case float64:
			if spec.Type != "integer" {
				continue
			}
			encoded = strconv.FormatInt(int64(typed), 10)
		default:
			continue
		}
		result[spec.Header] = encodeHeaderValue(encoded)
	}
	return result
}

func encodeHeaderValue(value string) string {
	if asciiHeaderValue(value) && strings.Trim(value, " \t") == value && !(strings.HasPrefix(value, "=?base64?") && strings.HasSuffix(value, "?=")) {
		return value
	}
	return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(value)) + "?="
}

func asciiHeaderValue(value string) bool {
	for _, character := range value {
		if character != '\t' && (character < 0x20 || character > 0x7e) {
			return false
		}
	}
	return true
}

// decodeResponse reads the JSON-RPC response with the given id. Event streams
// may carry notifications first; those are skipped.
func decodeResponse(resp *http.Response, id int64) (rpcResponse, error) {
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		var response rpcResponse
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxMessageSize)).Decode(&response); err != nil {
			return rpcResponse{}, fmt.Errorf("decode upstream response: %w", err)
		}
		return response, nil
	}
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, maxMessageSize))
	scanner.Buffer(make([]byte, 64<<10), maxMessageSize)
	var data []string
	flush := func() (rpcResponse, bool) {
		if len(data) == 0 {
			return rpcResponse{}, false
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		var response rpcResponse
		if json.Unmarshal([]byte(payload), &response) != nil {
			return rpcResponse{}, false
		}
		var responseID int64
		if len(response.ID) == 0 || json.Unmarshal(response.ID, &responseID) != nil || responseID != id {
			return rpcResponse{}, false
		}
		return response, true
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if response, ok := flush(); ok {
				return response, nil
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return rpcResponse{}, err
	}
	if response, ok := flush(); ok {
		return response, nil
	}
	return rpcResponse{}, errors.New("upstream event stream contained no response")
}

func (s *session) close(ctx context.Context) {
	if s.sessionID == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.connection.EndpointURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("Mcp-Session-Id", s.sessionID)
	s.applyCredential(req)
	if resp, err := s.client.http.Do(req); err == nil {
		resp.Body.Close()
	}
}

func (s *session) applyCredential(req *http.Request) {
	token := s.credential.Bearer()
	if token == "" || s.connection.AuthMethod == "none" {
		return
	}
	if s.connection.AuthMethod == "header" {
		req.Header.Set(s.connection.AuthName, token)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
}
