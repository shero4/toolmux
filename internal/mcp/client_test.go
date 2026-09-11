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
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var request rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "test-session")
			writeTestResult(t, w, request.ID, map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeTestResult(t, w, request.ID, map[string]any{"tools": []map[string]any{{"name": "balance", "description": "Read balance", "inputSchema": map[string]any{"type": "object"}}}})
		case "tools/call":
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
	result, err := client.Call(context.Background(), connection, credential, "balance", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(result) {
		t.Fatalf("invalid result: %s", result)
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
	decoded, err := decodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Result) < 128<<10 {
		t.Fatalf("result was truncated: %d bytes", len(decoded.Result))
	}
}

func writeTestResult(t *testing.T, w http.ResponseWriter, id int64, result any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}); err != nil {
		t.Fatal(err)
	}
}
