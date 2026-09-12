package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shero4/toolmux/internal/store"
)

func TestDiscoverAndCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var request rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case "server/discover":
			if r.Header.Get("MCP-Protocol-Version") != modernProtocol || r.Header.Get("Mcp-Method") != "server/discover" {
				t.Errorf("missing modern protocol headers")
			}
			writeTestResult(t, w, request.ID, map[string]any{"resultType": "complete", "supportedVersions": []string{modernProtocol}, "capabilities": map[string]any{"tools": map[string]any{}}})
		case "tools/list":
			writeTestResult(t, w, request.ID, map[string]any{"tools": []map[string]any{{"name": "balance", "description": "Read balance", "inputSchema": map[string]any{"type": "object"}}}})
		case "tools/call":
			if r.Header.Get("Mcp-Name") != "balance" {
				t.Errorf("Mcp-Name = %q", r.Header.Get("Mcp-Name"))
			}
			writeTestResult(t, w, request.ID, map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}})
		default:
			t.Fatalf("unexpected method %q", request.Method)
		}
	}))
	defer server.Close()

	client := NewClient()
	connection := store.Connection{EndpointURL: server.URL}
	credential := store.Credential{BearerToken: "upstream-secret"}
	tools, err := client.Discover(context.Background(), connection, credential)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].UpstreamName != "balance" {
		t.Fatalf("unexpected tools: %#v", tools)
	}
	result, err := client.Call(context.Background(), connection, credential, "balance", store.ToolCall{Arguments: json.RawMessage(`{}`)}, json.RawMessage(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(result) {
		t.Fatalf("invalid result: %s", result)
	}
}

func TestFallsBackToLegacyHandshake(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case "server/discover":
			writeTestError(t, w, request.ID, -32601, "method not found")
		case "initialize":
			writeTestResult(t, w, request.ID, map[string]any{"protocolVersion": legacyProtocol, "capabilities": map[string]any{}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeTestResult(t, w, request.ID, map[string]any{"tools": []any{}})
		default:
			t.Fatalf("unexpected method %q", request.Method)
		}
	}))
	defer server.Close()
	if _, err := NewClient().Discover(context.Background(), store.Connection{EndpointURL: server.URL}, store.Credential{}); err != nil {
		t.Fatal(err)
	}
}

func TestUnauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()
	_, err := NewClient().Discover(context.Background(), store.Connection{EndpointURL: server.URL}, store.Credential{})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("got %v, want ErrUnauthorized", err)
	}
}

func TestDecodeLargeServerSentEvent(t *testing.T) {
	payload := `data: {"jsonrpc":"2.0","id":1,"result":{"value":"` + strings.Repeat("x", 128<<10) + `"}}` + "\n\n"
	resp := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   io.NopCloser(strings.NewReader(payload)),
	}
	decoded, err := decodeResponse(resp, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Result) < 128<<10 {
		t.Fatalf("result was truncated: %d bytes", len(decoded.Result))
	}
}

func TestDecodeServerSentEventSkipsNotifications(t *testing.T) {
	payload := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":7,\ndata: \"result\":{\"ok\":true}}\n\n"
	resp := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   io.NopCloser(strings.NewReader(payload)),
	}
	decoded, err := decodeResponse(resp, 7)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded.Result) != `{"ok":true}` {
		t.Fatalf("unexpected result: %s", decoded.Result)
	}
}

func writeTestResult(t *testing.T, w http.ResponseWriter, id int64, result any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}); err != nil {
		t.Fatal(err)
	}
}

func writeTestError(t *testing.T, w http.ResponseWriter, id int64, code int, message string) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}); err != nil {
		t.Fatal(err)
	}
}
