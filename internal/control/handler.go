package control

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/shero4/toolmux/internal/checker"
	"github.com/shero4/toolmux/internal/importer"
	"github.com/shero4/toolmux/internal/store"
)

const protocolVersion = "2026-07-28"

type Handler struct {
	store    *store.Store
	checker  *checker.Checker
	importer *importer.Manager
	token    string
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func Token(masterKey []byte) string {
	digest := hmac.New(sha256.New, masterKey)
	digest.Write([]byte("toolmux-admin-mcp-v1"))
	return "tmx_admin_" + base64.RawURLEncoding.EncodeToString(digest.Sum(nil))
}

func New(store *store.Store, checker *checker.Checker, importer *importer.Manager, token string) http.Handler {
	return &Handler{store: store, checker: checker, importer: importer, token: token}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if provided == r.Header.Get("Authorization") || subtle.ConstantTimeCompare([]byte(provided), []byte(h.token)) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="toolmux-admin"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var message request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&message); err != nil {
		h.writeError(w, nil, -32700, "parse error")
		return
	}
	if message.JSONRPC != "2.0" || message.Method == "" {
		h.writeError(w, message.ID, -32600, "invalid request")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch message.Method {
	case "server/discover":
		h.writeResult(w, message.ID, map[string]any{"resultType": "complete", "supportedVersions": []string{protocolVersion}, "capabilities": map[string]any{"tools": map[string]any{}}, "instructions": "Administrative Toolmux control plane. These tools can create identities, issue credentials, and change access."})
	case "initialize":
		h.writeResult(w, message.ID, map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{"listChanged": false}}, "serverInfo": serverInfo()})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "ping":
		h.writeResult(w, message.ID, map[string]any{})
	case "tools/list":
		h.writeResult(w, message.ID, map[string]any{"resultType": "complete", "tools": definitions(), "ttlMs": 0, "cacheScope": "private"})
	case "tools/call":
		h.call(w, r.Context(), message)
	default:
		h.writeError(w, message.ID, -32601, "method not found")
	}
}

func definitions() []map[string]any {
	return []map[string]any{
		tool("connections_list", "List connections", "List configured service accounts and their health.", `{"type":"object","additionalProperties":false}`),
		tool("connection_check", "Check connection", "Verify reachability, authorization, protocol, and catalog for one connection.", `{"type":"object","properties":{"connection_id":{"type":"string"}},"required":["connection_id"],"additionalProperties":false}`),
		tool("agents_list", "List agents", "List agent identities and granted tool counts.", `{"type":"object","additionalProperties":false}`),
		tool("agent_create", "Create agent", "Create an agent identity and return its first token once.", `{"type":"object","properties":{"name":{"type":"string"},"slug":{"type":"string"}},"required":["name"],"additionalProperties":false}`),
		tool("agent_issue_token", "Issue agent token", "Issue another runtime token for an existing agent.", `{"type":"object","properties":{"agent_id":{"type":"string"},"label":{"type":"string"}},"required":["agent_id"],"additionalProperties":false}`),
		tool("agent_assign_connection", "Assign connection", "Grant or remove every capability on a connection and keep future capabilities in sync.", `{"type":"object","properties":{"agent_id":{"type":"string"},"connection_id":{"type":"string"},"enabled":{"type":"boolean"}},"required":["agent_id","connection_id","enabled"],"additionalProperties":false}`),
		tool("hermes_import", "Import Hermes", "Import detected Hermes profiles, connections, credentials, and Toolmux runtime configuration.", `{"type":"object","additionalProperties":false}`),
	}
}

func tool(name, title, description, schema string) map[string]any {
	var input any
	_ = json.Unmarshal([]byte(schema), &input)
	return map[string]any{"name": name, "title": title, "description": description, "inputSchema": input}
}

func (h *Handler) call(w http.ResponseWriter, ctx context.Context, message request) {
	var params callParams
	if json.Unmarshal(message.Params, &params) != nil || params.Name == "" {
		h.writeError(w, message.ID, -32602, "invalid tool call")
		return
	}
	var args struct {
		Name         string `json:"name"`
		Slug         string `json:"slug"`
		AgentID      string `json:"agent_id"`
		ConnectionID string `json:"connection_id"`
		Label        string `json:"label"`
		Enabled      *bool  `json:"enabled"`
	}
	if len(params.Arguments) > 0 && json.Unmarshal(params.Arguments, &args) != nil {
		h.writeError(w, message.ID, -32602, "invalid arguments")
		return
	}
	var value any
	var err error
	switch params.Name {
	case "connections_list":
		value, err = h.store.ListConnections(ctx)
	case "connection_check":
		if args.ConnectionID == "" {
			err = errors.New("connection_id is required")
		} else {
			err = h.checker.Check(ctx, args.ConnectionID)
			value = map[string]any{"checked": err == nil}
		}
	case "agents_list":
		value, err = h.store.ListAgents(ctx)
	case "agent_create":
		if args.Name == "" {
			err = errors.New("name is required")
			break
		}
		if args.Slug == "" {
			args.Slug = store.Slug(args.Name)
		}
		var agent store.Agent
		var token string
		agent, token, err = h.store.CreateAgent(ctx, args.Name, args.Slug)
		value = map[string]any{"agent": agent, "token": token, "endpoint": "/mcp"}
	case "agent_issue_token":
		if args.AgentID == "" {
			err = errors.New("agent_id is required")
			break
		}
		var token string
		token, err = h.store.IssueAgentToken(ctx, args.AgentID, args.Label)
		value = map[string]any{"token": token, "endpoint": "/mcp"}
	case "agent_assign_connection":
		if args.AgentID == "" || args.ConnectionID == "" || args.Enabled == nil {
			err = errors.New("agent_id, connection_id, and enabled are required")
			break
		}
		err = h.store.SetAgentConnection(ctx, args.AgentID, args.ConnectionID, *args.Enabled)
		value = map[string]any{"updated": err == nil}
	case "hermes_import":
		var inventory importer.Inventory
		inventory, err = h.importer.Scan(ctx)
		if err == nil {
			value, err = h.importer.ImportAll(ctx, inventory)
		}
	default:
		h.writeError(w, message.ID, -32601, "tool not found")
		return
	}
	if err != nil {
		h.writeError(w, message.ID, -32603, err.Error())
		return
	}
	payload, _ := json.Marshal(value)
	h.writeResult(w, message.ID, map[string]any{"resultType": "complete", "content": []map[string]any{{"type": "text", "text": string(payload)}}, "structuredContent": value, "isError": false})
}

func serverInfo() map[string]string {
	return map[string]string{"name": "toolmux-admin", "title": "Toolmux Administration", "version": "0.2.0"}
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
	_ = json.Unmarshal(id, &value)
	return value
}
