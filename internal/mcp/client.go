package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/shero4/toolmux/internal/store"
)

var ErrUnauthorized = errors.New("upstream rejected the credential")

const maxMessageSize = 32 << 20

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

type listToolsResult struct {
	Tools []wireTool `json:"tools"`
}

type wireTool struct {
	Name         string          `json:"name"`
	Title        string          `json:"title,omitempty"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Annotations  json.RawMessage `json:"annotations,omitempty"`
}

func NewClient() *Client {
	return &Client{http: &http.Client{Timeout: 15 * time.Second}}
}

func (c *Client) Discover(ctx context.Context, connection store.Connection, credential store.Credential) ([]store.Tool, error) {
	session, err := c.connect(ctx, connection, credential)
	if err != nil {
		return nil, err
	}
	defer session.close(ctx)
	var result listToolsResult
	if err := session.call(ctx, "tools/list", map[string]any{}, &result); err != nil {
		return nil, err
	}
	tools := make([]store.Tool, 0, len(result.Tools))
	for _, tool := range result.Tools {
		if len(tool.InputSchema) == 0 {
			tool.InputSchema = json.RawMessage(`{"type":"object"}`)
		}
		tools = append(tools, store.Tool{UpstreamName: tool.Name, Title: tool.Title, Description: tool.Description, InputSchema: tool.InputSchema, OutputSchema: tool.OutputSchema, Annotations: tool.Annotations})
	}
	return tools, nil
}

func (c *Client) Call(ctx context.Context, connection store.Connection, credential store.Credential, tool string, arguments json.RawMessage) (json.RawMessage, error) {
	session, err := c.connect(ctx, connection, credential)
	if err != nil {
		return nil, err
	}
	defer session.close(ctx)
	var result json.RawMessage
	params := map[string]any{"name": tool, "arguments": json.RawMessage(arguments)}
	if err := session.call(ctx, "tools/call", params, &result); err != nil {
		return nil, err
	}
	return result, nil
}

type session struct {
	client     *Client
	connection store.Connection
	credential store.Credential
	sessionID  string
}

func (c *Client) connect(ctx context.Context, connection store.Connection, credential store.Credential) (*session, error) {
	s := &session{client: c, connection: connection, credential: credential}
	params := map[string]any{
		"protocolVersion": "2025-11-25",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "toolmux", "version": "0.1.0"},
	}
	var initialized struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := s.call(ctx, "initialize", params, &initialized); err != nil {
		return nil, err
	}
	if initialized.ProtocolVersion == "" {
		return nil, errors.New("upstream returned no protocol version")
	}
	if err := s.notify(ctx, "notifications/initialized", map[string]any{}); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *session) call(ctx context.Context, method string, params any, result any) error {
	id := s.client.ids.Add(1)
	response, err := s.send(ctx, rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}, true)
	if err != nil {
		return err
	}
	if response.Error != nil {
		return fmt.Errorf("upstream %s: %s (%d)", method, response.Error.Message, response.Error.Code)
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
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
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
	return decodeResponse(resp)
}

func decodeResponse(resp *http.Response) (rpcResponse, error) {
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/event-stream") {
		scanner := bufio.NewScanner(io.LimitReader(resp.Body, maxMessageSize))
		scanner.Buffer(make([]byte, 64<<10), maxMessageSize)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data:") {
				var response rpcResponse
				if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &response); err == nil {
					return response, nil
				}
			}
		}
		if err := scanner.Err(); err != nil {
			return rpcResponse{}, err
		}
		return rpcResponse{}, errors.New("upstream event stream contained no response")
	}
	var response rpcResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxMessageSize)).Decode(&response); err != nil {
		return rpcResponse{}, fmt.Errorf("decode upstream response: %w", err)
	}
	return response, nil
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
	resp, err := s.client.http.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

func (s *session) applyCredential(req *http.Request) {
	token := s.credential.Bearer()
	if token == "" {
		return
	}
	if s.connection.AuthMethod == "header" {
		req.Header.Set(s.connection.AuthName, token)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
}
