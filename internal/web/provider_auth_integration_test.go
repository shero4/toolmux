package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shero4/toolmux/internal/gateway"
	"github.com/shero4/toolmux/internal/store"
)

func TestProviderAuthPersistenceAndRedaction(t *testing.T) {
	db, _, _ := featureStore(t)
	ctx := context.Background()
	if err := db.CreateAdmin(ctx, "owner", "integration-password"); err != nil {
		t.Fatal(err)
	}
	owner, _ := db.AuthenticateUser(ctx, "owner", "integration-password")
	session, _ := db.NewAdminSession(ctx, owner.ID)
	p := store.ModelProvider{Name: "Native provider", Slug: "native", Adapter: "anthropic", BaseURL: "https://example.test", Enabled: true, TimeoutSeconds: 30, Options: store.ProviderOptions{AuthType: "header", AuthHeader: "X-Relay-Key"}}
	id, err := db.SaveModelProviderConfig(ctx, p, "private-api-key", map[string]string{"X-Project": "private-project-header"}, false, []string{"model"})
	if err != nil {
		t.Fatal(err)
	}
	p, err = db.GetModelProvider(ctx, id)
	if err != nil || p.Options.AuthHeader != "X-Relay-Key" || p.Headers != nil {
		t.Fatal("public configuration or headers", err)
	}
	resolved, _, key, err := db.ResolveModel(ctx, "native/model")
	if err != nil || key != "private-api-key" || resolved.Headers["X-Project"] != "private-project-header" {
		t.Fatal("credential persistence", err)
	}
	if _, err = db.SaveModelProviderConfig(ctx, p, "", nil, false, nil); err != nil {
		t.Fatal(err)
	}
	resolved, _, key, err = db.ResolveModel(ctx, "native/model")
	if err != nil || key != "private-api-key" || len(resolved.Headers) != 1 {
		t.Fatal("blank edit lost credentials")
	}
	ui, err := New(db, nil, nil, nil, nil, "http://toolmux.test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "http://toolmux.test/providers/"+id, nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	w := httptest.NewRecorder()
	ui.Handler(http.NotFoundHandler(), http.NotFoundHandler(), gateway.New(db, nil)).ServeHTTP(w, r)
	if w.Code != 200 || strings.Contains(w.Body.String(), "private-api-key") || strings.Contains(w.Body.String(), "private-project-header") || !strings.Contains(w.Body.String(), "X-Relay-Key") {
		t.Fatal("provider page leaked secret or lost settings", w.Code)
	}
	if _, err = db.SaveModelProviderConfig(ctx, p, "", map[string]string{}, false, nil); err != nil {
		t.Fatal(err)
	}
	resolved, _, key, _ = db.ResolveModel(ctx, "native/model")
	if key != "private-api-key" || len(resolved.Headers) != 0 {
		t.Fatal("clear headers affected key")
	}
	if _, err = db.SaveModelProviderConfig(ctx, p, "", nil, true, nil); err != nil {
		t.Fatal(err)
	}
	_, _, key, _ = db.ResolveModel(ctx, "native/model")
	if key != "" {
		t.Fatal("clear credentials failed")
	}
}
