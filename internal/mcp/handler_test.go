package mcp

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestValidateModernRequest(t *testing.T) {
	request := inboundRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"stripe.balance","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}`),
	}
	httpRequest := httptest.NewRequest("POST", "/mcp", nil)
	httpRequest.Header.Set("MCP-Protocol-Version", modernProtocol)
	httpRequest.Header.Set("Mcp-Method", "tools/call")
	httpRequest.Header.Set("Mcp-Name", "stripe.balance")
	modern, version, err := validateProtocolRequest(httpRequest, request)
	if err != nil || !modern || version != modernProtocol {
		t.Fatalf("modern=%v version=%q err=%v", modern, version, err)
	}
}

func TestValidateModernRequestRejectsHeaderMismatch(t *testing.T) {
	request := inboundRequest{
		JSONRPC: "2.0",
		Method:  "tools/list",
		Params:  json.RawMessage(`{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}`),
	}
	httpRequest := httptest.NewRequest("POST", "/mcp", nil)
	httpRequest.Header.Set("MCP-Protocol-Version", modernProtocol)
	httpRequest.Header.Set("Mcp-Method", "tools/call")
	if _, _, err := validateProtocolRequest(httpRequest, request); err == nil {
		t.Fatal("expected header mismatch")
	}
}

func TestValidateModernRequestRejectsMissingVersionHeader(t *testing.T) {
	request := inboundRequest{
		JSONRPC: "2.0",
		Method:  "tools/list",
		Params:  json.RawMessage(`{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}`),
	}
	httpRequest := httptest.NewRequest("POST", "/mcp", nil)
	httpRequest.Header.Set("Mcp-Method", "tools/list")
	if _, _, err := validateProtocolRequest(httpRequest, request); err == nil {
		t.Fatal("expected missing protocol version header to be rejected")
	}
}

func TestToolCursorRoundTrip(t *testing.T) {
	for _, offset := range []int{0, 1, toolPageSize, 1197} {
		decoded, err := decodeCursor(encodeCursor(offset))
		if err != nil || decoded != offset {
			t.Fatalf("offset %d decoded as %d: %v", offset, decoded, err)
		}
	}
}
