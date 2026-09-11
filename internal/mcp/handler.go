package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/shero4/toolmux/internal/store"
)

const (
	modernProtocol = "2026-07-28"
	legacyProtocol = "2025-11-25"
	toolPageSize   = 200
)

var legacyProtocols = map[string]bool{"2024-11-05": true, "2025-03-26": true, "2025-06-18": true, legacyProtocol: true}

type Handler struct {
	store       *store.Store
	caller      ToolCaller
	credentials CredentialResolver
	log         *slog.Logger
}

type ToolCaller interface {
	Call(context.Context, store.Tool, store.Connection, store.Credential, store.ToolCall) (json.RawMessage, error)
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
	Name           string                     `json:"name"`
	Arguments      json.RawMessage            `json:"arguments"`
	InputResponses map[string]json.RawMessage `json:"inputResponses,omitempty"`
	RequestState   json.RawMessage            `json:"requestState,omitempty"`
}

type listParams struct {
	Cursor string `json:"cursor,omitempty"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !validOrigin(r) {
		http.Error(w, "invalid origin", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodDelete {
		if r.Header.Get("MCP-Protocol-Version") == modernProtocol {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
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
	if contentType := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(contentType), "application/json") {
		h.writeHTTPError(w, http.StatusUnsupportedMediaType, nil, -32600, "Content-Type must be application/json")
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		h.writeHTTPError(w, http.StatusBadRequest, nil, -32600, "invalid request")
		return
	}
	var request inboundRequest
	if err := json.Unmarshal(data, &request); err != nil {
		h.writeHTTPError(w, http.StatusBadRequest, nil, -32700, "parse error")
		return
	}
	if request.JSONRPC != "2.0" || request.Method == "" {
		h.writeHTTPError(w, http.StatusBadRequest, request.ID, -32600, "invalid request")
		return
	}
	modern, protocol, err := validateProtocolRequest(r, request)
	if err != nil {
		h.writeHTTPError(w, http.StatusBadRequest, request.ID, -32600, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("MCP-Protocol-Version", protocol)
	switch request.Method {
	case "server/discover":
		h.writeResult(w, request.ID, h.decorate(map[string]any{
			"supportedVersions": []string{modernProtocol},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"instructions":      "Toolmux exposes only the tools granted to this agent token. Call tools/list to discover them.",
		}, true))
	case "initialize":
		if modern {
			h.writeHTTPError(w, http.StatusBadRequest, request.ID, -32600, "initialize is not used by protocol 2026-07-28")
			return
		}
		negotiated := negotiateLegacy(request.Params)
		w.Header().Set("MCP-Protocol-Version", negotiated)
		h.writeResult(w, request.ID, map[string]any{"protocolVersion": negotiated, "capabilities": map[string]any{"tools": map[string]any{"listChanged": false}}, "serverInfo": serverInfo(), "instructions": "Toolmux exposes only the tools granted to this agent token."})
	case "notifications/initialized", "notifications/cancelled":
		w.WriteHeader(http.StatusAccepted)
	case "ping":
		h.writeResult(w, request.ID, h.decorate(map[string]any{}, modern))
	case "tools/list":
		h.listTools(w, r.Context(), request.ID, request.Params, agent, modern)
	case "tools/call":
		h.callTool(w, r.Context(), request.ID, request.Params, r.Header, agent, modern)
	default:
		if len(request.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if modern {
			h.writeHTTPError(w, http.StatusNotFound, request.ID, -32601, "method not found")
			return
		}
		h.writeError(w, request.ID, -32601, "method not found")
	}
}

func validOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Host != "" && strings.EqualFold(parsed.Host, r.Host)
}

func validateProtocolRequest(r *http.Request, request inboundRequest) (bool, string, error) {
	headerVersion := r.Header.Get("MCP-Protocol-Version")
	bodyVersion := requestProtocolVersion(request.Params)
	if bodyVersion == modernProtocol && headerVersion != modernProtocol {
		return false, modernProtocol, errors.New("MCP-Protocol-Version header does not match request metadata")
	}
	if headerVersion == modernProtocol {
		if r.Header.Get("Mcp-Method") != request.Method {
			return false, modernProtocol, errors.New("Mcp-Method header does not match request")
		}
		var params map[string]json.RawMessage
		if len(request.Params) == 0 || json.Unmarshal(request.Params, &params) != nil {
			return false, modernProtocol, errors.New("modern MCP requests require object params")
		}
		var meta map[string]json.RawMessage
		if json.Unmarshal(params["_meta"], &meta) != nil {
			return false, modernProtocol, errors.New("modern MCP requests require params._meta")
		}
		var bodyVersion string
		if json.Unmarshal(meta["io.modelcontextprotocol/protocolVersion"], &bodyVersion) != nil || bodyVersion != modernProtocol {
			return false, modernProtocol, errors.New("protocol version metadata does not match header")
		}
		if _, ok := meta["io.modelcontextprotocol/clientCapabilities"]; !ok {
			return false, modernProtocol, errors.New("modern MCP requests require client capabilities metadata")
		}
		if request.Method == "tools/call" {
			var call callParams
			if json.Unmarshal(request.Params, &call) != nil || call.Name == "" || r.Header.Get("Mcp-Name") != call.Name {
				return false, modernProtocol, errors.New("Mcp-Name header does not match tool name")
			}
		}
		return true, modernProtocol, nil
	}
	if headerVersion != "" && !legacyProtocols[headerVersion] {
		return false, headerVersion, errors.New("unsupported MCP protocol version")
	}
	if headerVersion == "" {
		headerVersion = "2025-03-26"
	}
	return false, headerVersion, nil
}

func requestProtocolVersion(raw json.RawMessage) string {
	var params map[string]json.RawMessage
	if json.Unmarshal(raw, &params) != nil {
		return ""
	}
	var meta map[string]json.RawMessage
	if json.Unmarshal(params["_meta"], &meta) != nil {
		return ""
	}
	var version string
	_ = json.Unmarshal(meta["io.modelcontextprotocol/protocolVersion"], &version)
	return version
}

func negotiateLegacy(raw json.RawMessage) string {
	var params struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(raw, &params) == nil && legacyProtocols[params.ProtocolVersion] {
		return params.ProtocolVersion
	}
	return legacyProtocol
}

func (h *Handler) listTools(w http.ResponseWriter, ctx context.Context, id, raw json.RawMessage, agent store.Agent, modern bool) {
	tools, err := h.store.ToolsForAgent(ctx, agent.ID)
	if err != nil {
		h.log.Error("list tools", "error", err)
		h.writeError(w, id, -32603, "tool catalog unavailable")
		return
	}
	var params listParams
	if len(raw) > 0 && json.Unmarshal(raw, &params) != nil {
		h.writeError(w, id, -32602, "invalid pagination cursor")
		return
	}
	offset, err := decodeCursor(params.Cursor)
	if err != nil || offset > len(tools) {
		h.writeError(w, id, -32602, "invalid pagination cursor")
		return
	}
	end := offset + toolPageSize
	if end > len(tools) {
		end = len(tools)
	}
	items := make([]map[string]any, 0, end-offset)
	for _, tool := range tools[offset:end] {
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
		if string(tool.Icons) != "null" {
			item["icons"] = tool.Icons
		}
		items = append(items, item)
	}
	result := map[string]any{"tools": items}
	if end < len(tools) {
		result["nextCursor"] = encodeCursor(end)
	}
	if modern {
		result["ttlMs"], result["cacheScope"] = 0, "private"
	}
	h.writeResult(w, id, h.decorate(result, modern))
}

func (h *Handler) callTool(w http.ResponseWriter, ctx context.Context, id, raw json.RawMessage, headers http.Header, agent store.Agent, modern bool) {
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
	if modern {
		for name, value := range parameterHeaders(tool.InputSchema, params.Arguments) {
			if headers.Get("Mcp-Param-"+name) != value {
				h.writeHTTPError(w, http.StatusBadRequest, id, -32020, "Mcp-Param header does not match tool arguments")
				return
			}
		}
	}
	credential, err = h.credentials.Resolve(ctx, connection, credential)
	if err != nil {
		_ = h.store.RecordAudit(ctx, agent.ID, connection.ID, tool.ID, "tools/call", "error", err.Error(), time.Since(start))
		h.writeError(w, id, -32603, "connection authorization unavailable")
		return
	}
	result, err := h.caller.Call(ctx, tool, connection, credential, store.ToolCall{Arguments: params.Arguments, InputResponses: params.InputResponses, RequestState: params.RequestState})
	if err != nil {
		h.log.Error("call upstream", "tool", tool.ExposedName, "error", err)
		_ = h.store.RecordAudit(ctx, agent.ID, connection.ID, tool.ID, "tools/call", "error", err.Error(), time.Since(start))
		h.writeError(w, id, -32603, "upstream tool failed")
		return
	}
	_ = h.store.RecordAudit(ctx, agent.ID, connection.ID, tool.ID, "tools/call", "allowed", "", time.Since(start))
	var decoded map[string]any
	if err := json.Unmarshal(result, &decoded); err != nil {
		h.writeError(w, id, -32603, "invalid upstream result")
		return
	}
	h.writeResult(w, id, h.decorate(decoded, modern))
}

func (h *Handler) decorate(result map[string]any, modern bool) map[string]any {
	if !modern {
		return result
	}
	if _, ok := result["resultType"]; !ok {
		result["resultType"] = "complete"
	}
	meta, _ := result["_meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta["io.modelcontextprotocol/serverInfo"] = serverInfo()
	result["_meta"] = meta
	return result
}

func serverInfo() map[string]string {
	return map[string]string{"name": "toolmux", "title": "Toolmux", "version": "0.2.0"}
}
func encodeCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}
func decodeCursor(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, err
	}
	offset, err := strconv.Atoi(string(decoded))
	if err != nil || offset < 0 {
		return 0, errors.New("invalid cursor")
	}
	return offset, nil
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
func (h *Handler) writeHTTPError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	h.writeError(w, id, code, message)
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
