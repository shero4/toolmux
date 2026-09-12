package web

import (
	"context"
	"encoding/json"
	"github.com/shero4/toolmux/internal/gateway"
	"github.com/shero4/toolmux/internal/localconfig"
	"github.com/shero4/toolmux/internal/store"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotationAndMonitoring(t *testing.T) {
	db, _, _ := featureStore(t)
	ctx := context.Background()
	must := func(e error) {
		t.Helper()
		if e != nil {
			t.Fatal(e)
		}
	}
	must(db.CreateAdmin(ctx, "owner", "integration-password"))
	owner, e := db.AuthenticateUser(ctx, "owner", "integration-password")
	must(e)
	session, e := db.NewAdminSession(ctx, owner.ID)
	must(e)
	ui, e := New(db, nil, nil, nil, nil, "http://toolmux.test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	must(e)
	handler := ui.Handler(http.NotFoundHandler(), http.NotFoundHandler(), gateway.New(db, nil))
	request := func(method, path string, form url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://toolmux.test"+path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "http://toolmux.test")
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	config := filepath.Join(t.TempDir(), "config.yaml")
	agent, old, e := db.CreateDiscoveredAgent(ctx, "Local test", "local-test", "fixture", "hermes", "test", "This computer", config)
	must(e)
	original := "# keep comment\nmodel:\n  api_key: " + old + "\nmcp_servers:\n  toolmux:\n    headers:\n      Authorization: Bearer " + old + "\n"
	must(os.WriteFile(config, []byte(original), 0600))
	tokens, e := db.ListAgentTokens(ctx, agent.ID)
	must(e)
	oldID := tokens[0].ID
	path := "/agents/" + agent.ID + "/tokens/" + oldID + "/rotate"
	w := request("GET", path, nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), old) || !strings.Contains(w.Body.String(), config) {
		t.Fatal("preview missing or leaked token", w.Code)
	}
	refs, e := localconfig.Inspect(ctx, []string{config}, func(c context.Context, v string) (bool, error) { return db.MatchesAgentToken(c, agent.ID, oldID, v) })
	must(e)
	fingerprint := localconfig.Fingerprint(refs)
	must(os.WriteFile(config, []byte(original+"# editor change\n"), 0600))
	w = request("POST", path, url.Values{"update_local": {"on"}, "fingerprint": {fingerprint}})
	if w.Code != 303 {
		t.Fatal(w.Code)
	}
	active, e := db.AgentTokenActive(ctx, agent.ID, old)
	must(e)
	if !active {
		t.Fatal("stale preview revoked old token")
	}
	must(os.WriteFile(config, []byte(original), 0600))
	w = request("POST", path, url.Values{"update_local": {"on"}, "fingerprint": {fingerprint}})
	if w.Code != 303 {
		t.Fatal(w.Code)
	}
	active, e = db.AgentTokenActive(ctx, agent.ID, old)
	must(e)
	if active {
		t.Fatal("old token still active")
	}
	data, e := os.ReadFile(config)
	must(e)
	if strings.Contains(string(data), old) || !strings.HasPrefix(string(data), "# keep comment") {
		t.Fatal("configuration not updated safely")
	}
	tokens, e = db.ListAgentTokens(ctx, agent.ID)
	must(e)
	if len(tokens) != 1 {
		t.Fatal("wrong active token count")
	}
	refs, e = localconfig.Inspect(ctx, []string{config}, func(c context.Context, v string) (bool, error) {
		return db.MatchesAgentToken(c, agent.ID, tokens[0].ID, v)
	})
	must(e)
	if len(refs) != 1 || refs[0].Count != 2 {
		t.Fatal("model and tool references not both rotated")
	}
	must(db.SaveOperationSettings(ctx, store.OperationSettings{IntervalSeconds: 60, WebhookEnabled: true}, "https://hooks.example.test/private-secret", false))
	w = request("GET", "/settings", nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "private-secret") {
		t.Fatal("webhook secret exposed")
	}
	for i := 0; i < 3; i++ {
		must(db.Observe(ctx, "provider", "fixture", "Fixture provider", "reauthorization_required"))
	}
	d, e := db.NextDelivery(ctx)
	must(e)
	var payload map[string]string
	must(json.Unmarshal(d.Payload, &payload))
	if !strings.Contains(payload["text"], "reauthorization_required") {
		t.Fatal("wrong alert")
	}
	if _, e = db.NextDelivery(ctx); e == nil {
		t.Fatal("duplicate transition alert")
	}
	must(db.Observe(ctx, "provider", "fixture", "Fixture provider", "unreachable"))
	must(db.Observe(ctx, "provider", "fixture", "Fixture provider", "reauthorization_required"))
	if _, e = db.NextDelivery(ctx); e == nil {
		t.Fatal("network flapping duplicated auth alert")
	}
	must(db.FinishDelivery(ctx, d, true))
	must(db.Observe(ctx, "provider", "fixture", "Fixture provider", "connected"))
	_, e = db.NextDelivery(ctx)
	must(e)
	must(db.SaveOperationSettings(ctx, store.OperationSettings{IntervalSeconds: 120}, "", false))
	if _, e = db.NextDelivery(ctx); e == nil {
		t.Fatal("queued alerts survived settings change")
	}
	for _, page := range []string{"/docs", "/operations", "/auth/codex"} {
		w = request("GET", page, nil)
		if w.Code != 200 {
			t.Fatal(page, w.Code, w.Body.String())
		}
	}
	w = request("GET", "/settings/updates", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `href="/settings/updates" aria-current="page">Updates</a>`) || strings.Contains(w.Body.String(), `href="/settings" aria-current="page">Settings</a>`) {
		t.Fatal("update navigation does not identify the active subsection", w.Code)
	}
}
func TestOperationRoleBoundaries(t *testing.T) {
	for _, role := range []string{"viewer", "operator"} {
		u := store.User{Role: role, Enabled: true}
		for _, path := range []string{"/settings/monitoring", "/settings/webhook/test", "/auth/codex"} {
			if permitted(u, httptest.NewRequest("POST", path, nil)) {
				t.Fatal(role, "allowed", path)
			}
		}
	}
	if permitted(store.User{Role: "viewer", Enabled: true}, httptest.NewRequest("GET", "/agents/id/tokens/id/rotate", nil)) {
		t.Fatal("viewer allowed rotation preview")
	}
}
