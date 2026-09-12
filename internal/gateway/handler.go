package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/shero4/toolmux/internal/store"
)

type Repository interface {
	AuthenticateAgent(context.Context, string) (store.Agent, error)
	AvailableModels(context.Context) ([]string, error)
	ResolveModel(context.Context, string) (store.ModelProvider, string, string, error)
	RecordModelCall(context.Context, string, string, string, string, string, string, time.Duration, *int64, *int64) error
}

type Handler struct {
	Tokens Tokens
	Store  Repository
	Client *http.Client
	Log    *slog.Logger
}

func New(repository Repository, log *slog.Logger) *Handler {
	return &Handler{Store: repository, Client: Client(), Log: log}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	authorization := r.Header.Get("Authorization")
	token := strings.TrimPrefix(authorization, "Bearer ")
	if authorization == "" {
		token = r.Header.Get("X-Api-Key")
	}
	if token == "" || token == authorization {
		apiError(w, 401, "authentication_error", "An agent bearer token is required.")
		return
	}
	agent, err := h.Store.AuthenticateAgent(r.Context(), token)
	if err != nil {
		apiError(w, 401, "authentication_error", "Invalid or revoked agent token.")
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		models, err := h.Store.AvailableModels(r.Context())
		if err != nil {
			apiError(w, 503, "server_error", "Model catalog unavailable.")
			return
		}
		items := make([]map[string]any, 0, len(models))
		for _, model := range models {
			items = append(items, map[string]any{"id": model, "object": "model", "created": 0, "owned_by": "toolmux"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": items})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/")
	if r.Method != http.MethodPost || (path != "chat/completions" && path != "responses" && path != "embeddings" && path != "completions" && path != "messages" && path != "messages/count_tokens") {
		apiError(w, 404, "invalid_request_error", "Endpoint not supported.")
		return
	}
	h.infer(w, r, agent, path)
}

func (h *Handler) infer(w http.ResponseWriter, r *http.Request, agent store.Agent, path string) {
	start := time.Now()
	decision, reason := "error", "invalid request"
	providerName, selected, model := "", "", ""
	meter := usage{}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.Store.RecordModelCall(ctx, agent.ID, providerName, selected, model, decision, reason, time.Since(start), meter.Input, meter.Output); err != nil && h.Log != nil {
			h.Log.Error("record model call", "error", err)
		}
	}()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	if err != nil {
		apiError(w, 413, "invalid_request_error", "Request exceeds 16 MiB.")
		return
	}
	var input map[string]json.RawMessage
	if json.Unmarshal(body, &input) != nil || input == nil {
		apiError(w, 400, "invalid_request_error", "Expected a JSON object.")
		return
	}
	if json.Unmarshal(input["model"], &selected) != nil || selected == "" || len(selected) > 337 {
		apiError(w, 400, "invalid_request_error", "Choose a provider/model from /v1/models.")
		return
	}
	p, upstream, key, err := h.Store.ResolveModel(r.Context(), selected)
	if errors.Is(err, store.ErrNotFound) {
		decision, reason = "denied", "model not available"
		apiError(w, 404, "model_not_found", "Model is not configured or its provider is paused.")
		return
	}
	if err != nil {
		reason = "configuration unavailable"
		apiError(w, 503, "server_error", "Provider configuration unavailable.")
		return
	}
	providerName, model = p.Name, upstream
	if !Supports(p.Adapter, path) {
		reason = "protocol mismatch"
		apiError(w, 400, "invalid_request_error", "This provider does not support this endpoint. Use its native protocol or configure a LiteLLM gateway for translation.")
		return
	}
	if err = ValidateProvider(p, key, p.Headers); err != nil {
		reason = "invalid provider configuration"
		apiError(w, 503, "server_error", "Invalid provider configuration.")
		return
	}
	key, err = h.Tokens.Resolve(r.Context(), h.Client, p, key)
	if err != nil {
		reason = "provider authentication failed"
		apiError(w, 502, "authentication_error", "Provider authentication failed. Check its credentials and token endpoint.")
		return
	}
	adapter, err := adapterFor(p.Adapter)
	if err != nil {
		reason = "unsupported adapter"
		apiError(w, 503, "server_error", "Provider adapter unavailable.")
		return
	}
	input["model"], _ = json.Marshal(upstream)
	// All other parameters, tool definitions, and provider extensions are preserved.
	body, _ = json.Marshal(input)
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(p.TimeoutSeconds)*time.Second)
	defer cancel()
	req, err := adapter.Request(ctx, p, key, path, bytes.NewReader(body))
	if err != nil {
		reason = "invalid provider configuration"
		apiError(w, 503, "server_error", "Invalid provider configuration.")
		return
	}
	// These are protocol features, not credentials. Never forward incoming
	// Authorization, X-Api-Key, cookies, or arbitrary caller headers.
	if path == "messages" || path == "messages/count_tokens" {
		if p.Options.APIVersion == "" && r.Header.Get("Anthropic-Version") != "" && len(r.Header.Get("Anthropic-Version")) <= 80 {
			customVersion := false
			for name := range p.Headers {
				if strings.EqualFold(name, "Anthropic-Version") {
					customVersion = true
				}
			}
			if !customVersion {
				req.Header.Set("Anthropic-Version", r.Header.Get("Anthropic-Version"))
			}
		}
		for _, name := range []string{"Anthropic-Version", "Anthropic-Beta"} {
			if value := r.Header.Get(name); value != "" && len(value) <= 4096 && req.Header.Get(name) == "" {
				req.Header.Set(name, value)
			}
		}
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		reason = "provider unavailable or request timed out"
		apiError(w, 502, "upstream_error", "Provider unavailable or request timed out.")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		reason = fmt.Sprintf("provider HTTP %d", resp.StatusCode)
		status := resp.StatusCode
		if status < 400 || status > 599 {
			status = 502
		}
		apiError(w, status, "upstream_error", reason+". Check provider configuration and request compatibility.")
		return
	}
	contentType := resp.Header.Get("Content-Type")
	if isStream(contentType) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(resp.StatusCode)
		reader := io.TeeReader(resp.Body, &meter)
		buffer := make([]byte, 32<<10)
		for {
			n, readErr := reader.Read(buffer)
			if n > 0 {
				if _, err = w.Write(buffer[:n]); err != nil {
					reason = "client disconnected"
					return
				}
				if err = http.NewResponseController(w).Flush(); err != nil {
					reason = "client disconnected"
					return
				}
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					reason = "stream interrupted"
					return
				}
				break
			}
		}
		if meter.Failed || !meter.Complete {
			reason = "provider stream failed or ended early"
			return
		}
	} else {
		data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20+1))
		if err != nil || len(data) > 32<<20 {
			reason = "response interrupted or too large"
			apiError(w, 502, "upstream_error", "Provider response unavailable or too large.")
			return
		}
		if !json.Valid(data) {
			reason = "invalid provider response"
			apiError(w, 502, "upstream_error", "Provider returned invalid JSON.")
			return
		}
		meter.json(data)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		if _, err = w.Write(data); err != nil {
			reason = "client disconnected"
			return
		}
		if meter.Failed {
			reason = "provider returned an error"
			return
		}
	}
	decision, reason = "allowed", ""
}

func apiError(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]any{"message": message, "type": kind, "code": kind}})
}
