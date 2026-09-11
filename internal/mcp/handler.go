package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/shero4/toolmux/internal/store"
)

type Handler struct {
	store       *store.Store
	caller      ToolCaller
	credentials CredentialResolver
	log         *slog.Logger
}

type ToolCaller interface {
	Call(context.Context, store.Tool, store.Connection, store.Credential, json.RawMessage) (json.RawMessage, error)
}

type CredentialResolver interface {
	Resolve(context.Context, store.Connection, store.Credential) (store.Credential, error)
}

func NewHandler(store *store.Store, caller ToolCaller, credentials CredentialResolver, log *slog.Logger) http.Handler {
	return &Handler{store: store, caller: caller, credentials: credentials, log: log}
}

type inboundRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}
type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" || token == r.Header.Get("Authorization") {
		h.unauthorized(w)
		return
	}
	agent, err := h.store.AuthenticateAgent(r.Context(), token)
	if err != nil {
		h.unauthorized(w)
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	var request inboundRequest
	if err := json.Unmarshal(data, &request); err != nil {
		h.writeError(w, nil, -32700, "parse error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("MCP-Protocol-Version", "2025-11-25")
	switch request.Method {
	case "initialize":
		h.writeResult(w, request.ID, map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{"listChanged": false}}, "serverInfo": map[string]string{"name": "toolmux", "version": "0.1.0"}})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "ping":
		h.writeResult(w, request.ID, map[string]any{})
	case "tools/list":
		h.listTools(w, r.Context(), request.ID, agent)
	case "tools/call":
		h.callTool(w, r.Context(), request.ID, request.Params, agent)
	default:
		h.writeError(w, request.ID, -32601, "method not found")
	}
}

func (h *Handler) listTools(w http.ResponseWriter, ctx context.Context, id json.RawMessage, agent store.Agent) {
	tools, err := h.store.ToolsForAgent(ctx, agent.ID)
	if err != nil {
		h.log.Error("list tools", "error", err)
		h.writeError(w, id, -32603, "tool catalog unavailable")
		return
	}
	items := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		item := map[string]any{"name": tool.ExposedName, "description": tool.Description, "inputSchema": tool.InputSchema}
		if tool.Title != "" {
			item["title"] = tool.Title
		}
		if string(tool.OutputSchema) != "null" {
			item["outputSchema"] = tool.OutputSchema
		}
		if string(tool.Annotations) != "null" {
			item["annotations"] = tool.Annotations
		}
		items = append(items, item)
	}
	h.writeResult(w, id, map[string]any{"tools": items})
}

func (h *Handler) callTool(w http.ResponseWriter, ctx context.Context, id, raw json.RawMessage, agent store.Agent) {
	start := time.Now()
	var params callParams
	if err := json.Unmarshal(raw, &params); err != nil || params.Name == "" {
		h.writeError(w, id, -32602, "invalid tool call")
		return
	}
	if len(params.Arguments) == 0 {
		params.Arguments = json.RawMessage(`{}`)
	}
	tool, connection, credential, err := h.store.ResolveGrantedTool(ctx, agent.ID, params.Name)
	if errors.Is(err, store.ErrNotFound) {
		_ = h.store.RecordAudit(ctx, agent.ID, "", "", "tools/call", "denied", "tool not granted", time.Since(start))
		h.writeError(w, id, -32601, "tool not found")
		return
	}
	if err != nil {
		h.log.Error("authorize tool", "error", err)
		_ = h.store.RecordAudit(ctx, agent.ID, "", "", "tools/call", "error", "authorization store unavailable", time.Since(start))
		h.writeError(w, id, -32603, "authorization unavailable")
		return
	}
	credential, err = h.credentials.Resolve(ctx, connection, credential)
	if err != nil {
		_ = h.store.RecordAudit(ctx, agent.ID, connection.ID, tool.ID, "tools/call", "error", err.Error(), time.Since(start))
		h.writeError(w, id, -32603, "connection authorization unavailable")
		return
	}
	result, err := h.caller.Call(ctx, tool, connection, credential, params.Arguments)
	if err != nil {
		h.log.Error("call upstream", "tool", tool.ExposedName, "error", err)
		_ = h.store.RecordAudit(ctx, agent.ID, connection.ID, tool.ID, "tools/call", "error", err.Error(), time.Since(start))
		h.writeError(w, id, -32603, "upstream tool failed")
		return
	}
	_ = h.store.RecordAudit(ctx, agent.ID, connection.ID, tool.ID, "tools/call", "allowed", "", time.Since(start))
	var decoded any
	if err := json.Unmarshal(result, &decoded); err != nil {
		h.writeError(w, id, -32603, "invalid upstream result")
		return
	}
	h.writeResult(w, id, decoded)
}

func (h *Handler) unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="toolmux"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}
func (h *Handler) writeResult(w http.ResponseWriter, id json.RawMessage, result any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rawID(id), "result": result})
}
func (h *Handler) writeError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rawID(id), "error": map[string]any{"code": code, "message": message}})
}
func rawID(id json.RawMessage) any {
	if len(id) == 0 {
		return nil
	}
	var value any
	if json.Unmarshal(id, &value) != nil {
		return nil
	}
	return value
}
