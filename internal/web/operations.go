package web

import (
	"context"
	"encoding/json"
	"github.com/shero4/toolmux/internal/localconfig"
	"github.com/shero4/toolmux/internal/operations"
	"github.com/shero4/toolmux/internal/store"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type operationsPage struct {
	Settings store.OperationSettings
	Events   []store.OperationEvent
	States   []store.OperationState
	Codex    operations.CodexState
}

func (s *Server) RunOperations(ctx context.Context) { s.operations.Run(ctx) }
func (s *Server) operationLogs(w http.ResponseWriter, r *http.Request) {
	events, err := s.store.OperationEvents(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	states, err := s.store.OperationStates(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, 200, "operations", "operations", "Operational logs", operationsPage{Events: events, States: states})
}
func (s *Server) saveMonitoring(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if r.ParseForm() != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	interval, _ := strconv.Atoi(r.FormValue("interval"))
	endpoint := strings.TrimSpace(r.FormValue("webhook_url"))
	if interval < 60 || interval > 3600 || (endpoint != "" && operations.ValidateWebhook(endpoint) != nil) {
		s.redirect(w, r, "/settings", errorFlash("Choose a check interval of 60–3600 seconds and a valid HTTPS webhook URL."))
		return
	}
	v := store.OperationSettings{IntervalSeconds: interval, WebhookEnabled: r.FormValue("webhook_enabled") == "on"}
	previous, err := s.store.OperationSettings(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	clear := r.FormValue("clear_webhook") == "on"
	if v.WebhookEnabled && (clear || (endpoint == "" && !previous.HasWebhook)) {
		s.redirect(w, r, "/settings", errorFlash("Enter a webhook URL before enabling alerts."))
		return
	}
	if err = s.store.SaveOperationSettings(r.Context(), v, endpoint, clear); err != nil {
		s.fail(w, err)
		return
	}
	_ = s.store.LogOperation(r.Context(), "settings", "", currentUser(r).Username, "updated", "Authorization monitoring settings updated.")
	s.redirect(w, r, "/settings", okFlash("Monitoring settings saved. Changes take effect within the next check interval."))
}
func (s *Server) testWebhook(w http.ResponseWriter, r *http.Request) {
	endpoint, err := s.store.WebhookURL(r.Context())
	if err != nil || endpoint == "" {
		s.redirect(w, r, "/settings", errorFlash("Save and enable a webhook first."))
		return
	}
	ok := operations.SendWebhook(r.Context(), endpoint, []byte(`{"text":"Toolmux: test authorization alert. No action is required."}`), "test-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	status, message := "delivered", "Test webhook delivered."
	if !ok {
		status = "failed"
		message = "Test webhook failed. Check the saved endpoint."
	}
	_ = s.store.LogOperation(r.Context(), "webhook", "", currentUser(r).Username, status, message)
	if ok {
		s.redirect(w, r, "/settings", okFlash(message))
	} else {
		s.redirect(w, r, "/settings", errorFlash(message))
	}
}
func (s *Server) checkNow(w http.ResponseWriter, r *http.Request) {
	go s.operations.Check(context.Background())
	s.redirect(w, r, "/operations", okFlash("Authorization checks queued. This page shows the latest completed results."))
}
func (s *Server) codexPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, 200, "codex", "providers", "Codex sign-in", s.operations.Codex.View())
}
func (s *Server) codexStatus(w http.ResponseWriter, r *http.Request) {
	v := s.operations.Codex.View()
	if !currentUser(r).IsAdmin() {
		v.Code = ""
		v.URL = ""
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func (s *Server) codexLogin(w http.ResponseWriter, r *http.Request) {
	err := s.operations.Codex.Start(func(status string) {
		_ = s.store.Observe(context.Background(), "subscription", "codex", "Local Codex bridge", status)
	})
	if err != nil {
		s.redirect(w, r, "/auth/codex", errorFlash("A sign-in is already in progress. Follow the current instructions."))
		return
	}
	_ = s.store.LogOperation(r.Context(), "subscription", "codex", currentUser(r).Username, "starting", "Local Codex sign-in started.")
	s.redirect(w, r, "/auth/codex", okFlash("Sign-in started. The instructions update automatically."))
}

type rotationPage struct {
	Agent                       store.Agent
	TokenID, Fingerprint, Error string
	References                  []localconfig.Reference
}

func (s *Server) rotationReferences(r *http.Request, a store.Agent) ([]localconfig.Reference, error) {
	return localconfig.Inspect(r.Context(), localconfig.Files(a), func(ctx context.Context, token string) (bool, error) {
		return s.store.MatchesAgentToken(ctx, a.ID, r.PathValue("tokenID"), token)
	})
}
func (s *Server) rotatePreview(w http.ResponseWriter, r *http.Request) {
	a, err := s.store.GetAgent(r.Context(), r.PathValue("id"))
	if err != nil {
		s.notFound(w, r, "Agent")
		return
	}
	refs, err := s.rotationReferences(r, a)
	page := rotationPage{Agent: a, TokenID: r.PathValue("tokenID"), References: refs, Fingerprint: localconfig.Fingerprint(refs)}
	if err != nil {
		page.Error = "The local configuration could not be safely inspected. Manual rotation is still available."
	}
	s.render(w, r, 200, "rotation", "agents", "Rotate agent token", page)
}
func (s *Server) rotateToken(w http.ResponseWriter, r *http.Request) {
	s.rotationMu.Lock()
	defer s.rotationMu.Unlock()
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if r.ParseForm() != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	a, err := s.store.GetAgent(r.Context(), r.PathValue("id"))
	if err != nil || a.Status != "active" {
		s.notFound(w, r, "Active agent")
		return
	}
	oldID := r.PathValue("tokenID")
	tokens, err := s.store.ListAgentTokens(r.Context(), a.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	label := ""
	found := false
	for _, t := range tokens {
		if t.ID == oldID {
			label = t.Label
			found = true
		}
	}
	if !found {
		s.redirect(w, r, "/agents/"+a.ID, errorFlash("That token is no longer active. Refresh and try again."))
		return
	}
	var refs []localconfig.Reference
	if r.FormValue("update_local") == "on" {
		refs, err = s.rotationReferences(r, a)
		if err != nil || len(refs) == 0 || localconfig.Fingerprint(refs) != r.FormValue("fingerprint") {
			s.redirect(w, r, "/agents/"+a.ID, errorFlash("The detected files changed. Review rotation again; no token was changed."))
			return
		}
	}
	token, err := s.store.IssueAgentToken(r.Context(), a.ID, label)
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(refs) > 0 {
		if err = localconfig.Apply(refs, token); err != nil {
			// Both tokens remain valid if a disk failure interrupts a multi-file update.
			// This keeps both updated and untouched clients usable for manual recovery.
			_ = s.store.LogOperation(r.Context(), "token", a.ID, a.Name, "incomplete", "Local rotation interrupted. Old and new tokens remain active; review configuration before revoking either.")
			s.redirect(w, r, "/agents/"+a.ID, s.tokenFlash(a, token, "Rotation incomplete: some files may have changed. Both tokens remain active. Review local configuration before revoking the old token."))
			return
		}
	}
	if err = s.store.RevokeAgentTokenByID(r.Context(), a.ID, oldID); err != nil {
		s.redirect(w, r, "/agents/"+a.ID, s.tokenFlash(a, token, "New token issued, but the old token could not be revoked. Review the token list."))
		return
	}
	message := "Token rotated. Update remote clients with the new token."
	if len(refs) > 0 {
		message = "Token rotated and matching local tool/model references updated. Restart running clients to load the new token."
	}
	_ = s.store.LogOperation(r.Context(), "token", a.ID, a.Name, "rotated", message)
	s.redirect(w, r, "/agents/"+a.ID, s.tokenFlash(a, token, message))
}
