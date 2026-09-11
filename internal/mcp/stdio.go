package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/shero4/toolmux/internal/store"
)

func (c *Client) DiscoverStdio(ctx context.Context, spec store.MCPStdioSpec, credential store.Credential) ([]store.Tool, error) {
	session, err := c.connectStdio(ctx, spec, credential)
	if err != nil {
		return nil, err
	}
	defer session.close()
	var result listToolsResult
	if err := session.call("tools/list", map[string]any{}, &result); err != nil {
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

func (c *Client) CallStdio(ctx context.Context, spec store.MCPStdioSpec, credential store.Credential, tool string, arguments json.RawMessage) (json.RawMessage, error) {
	session, err := c.connectStdio(ctx, spec, credential)
	if err != nil {
		return nil, err
	}
	defer session.close()
	var result json.RawMessage
	if err := session.call("tools/call", map[string]any{"name": tool, "arguments": json.RawMessage(arguments)}, &result); err != nil {
		return nil, err
	}
	return result, nil
}

type stdioSession struct {
	client *Client
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	decode *json.Decoder
	encode *json.Encoder
	stderr bytes.Buffer
}

func (c *Client) connectStdio(ctx context.Context, spec store.MCPStdioSpec, credential store.Credential) (*stdioSession, error) {
	var args []string
	if err := json.Unmarshal(spec.Args, &args); err != nil {
		return nil, fmt.Errorf("decode MCP command arguments: %w", err)
	}
	cmd := exec.CommandContext(ctx, spec.Executable, args...)
	cmd.Dir = spec.WorkingDirectory
	cmd.Env = mergeEnvironment(os.Environ(), credential.Environment)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	session := &stdioSession{client: c, cmd: cmd, stdin: stdin, decode: json.NewDecoder(stdout), encode: json.NewEncoder(stdin)}
	cmd.Stderr = &session.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start MCP command: %w", err)
	}
	var initialized struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := session.call("initialize", map[string]any{
		"protocolVersion": "2025-11-25",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "toolmux", "version": "0.1.0"},
	}, &initialized); err != nil {
		session.close()
		return nil, err
	}
	if initialized.ProtocolVersion == "" {
		session.close()
		return nil, errors.New("upstream returned no protocol version")
	}
	if err := session.encode.Encode(rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized", Params: map[string]any{}}); err != nil {
		session.close()
		return nil, err
	}
	return session, nil
}

func (s *stdioSession) call(method string, params, result any) error {
	id := s.client.ids.Add(1)
	if err := s.encode.Encode(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		return fmt.Errorf("write upstream %s: %w", method, err)
	}
	for {
		var response rpcResponse
		if err := s.decode.Decode(&response); err != nil {
			detail := strings.TrimSpace(s.stderr.String())
			if detail != "" {
				return fmt.Errorf("read upstream %s: %w: %s", method, err, truncateStdio(detail, 512))
			}
			return fmt.Errorf("read upstream %s: %w", method, err)
		}
		var responseID int64
		if len(response.ID) == 0 || json.Unmarshal(response.ID, &responseID) != nil || responseID != id {
			continue
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
}

func (s *stdioSession) close() {
	_ = s.stdin.Close()
	done := make(chan struct{})
	go func() {
		_ = s.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		_ = s.cmd.Process.Kill()
		<-done
	}
}

func mergeEnvironment(base []string, values map[string]string) []string {
	result := append([]string(nil), base...)
	for name, value := range values {
		prefix := strings.ToUpper(name) + "="
		filtered := result[:0]
		for _, item := range result {
			if !strings.HasPrefix(strings.ToUpper(item), prefix) {
				filtered = append(filtered, item)
			}
		}
		result = append(filtered, name+"="+value)
	}
	return result
}

func truncateStdio(value string, limit int) string {
	if len(value) > limit {
		return value[:limit]
	}
	return value
}
