package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/shero4/toolmux/internal/gateway"
	"github.com/shero4/toolmux/internal/migrate"
	"github.com/shero4/toolmux/internal/secretbox"
	"github.com/shero4/toolmux/internal/store"
)

func featureStore(t *testing.T, through ...int) (*store.Store, string, *secretbox.Box) {
	t.Helper()
	raw := os.Getenv("TOOLMUX_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("set TOOLMUX_TEST_DATABASE_URL to an isolated database ending in _test")
	}
	parsed, err := url.Parse(raw)
	if err != nil || !strings.HasSuffix(parsed.Path, "_test") {
		t.Fatal("refusing a non-test database")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	random := make([]byte, 8)
	_, _ = rand.Read(random)
	schema := "toolmux_test_" + hex.EncodeToString(random)
	if _, err = conn.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		_ = conn.Close(ctx)
	})
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	box, _ := secretbox.New(make([]byte, 32))
	db, err := store.Open(ctx, parsed.String(), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	migrations, err := migrate.All()
	if err != nil {
		t.Fatal(err)
	}
	set := map[int]string{}
	for _, m := range migrations {
		if len(through) > 0 && m.Version > through[0] {
			continue
		}
		set[m.Version] = m.SQL
	}
	if err = db.Migrate(ctx, set); err != nil {
		t.Fatal(err)
	}
	if err = db.Migrate(ctx, set); err != nil {
		t.Fatal("idempotent migration", err)
	}
	return db, parsed.String(), box
}

func TestAuthenticationProvidersAndActivity(t *testing.T) {
	db, dsn, box := featureStore(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	makeHandler := func(db *store.Store) http.Handler {
		ui, err := New(db, nil, nil, nil, nil, "http://toolmux.test", log)
		if err != nil {
			t.Fatal(err)
		}
		return ui.Handler(http.NotFoundHandler(), http.NotFoundHandler(), gateway.New(db, log))
	}
	handler := makeHandler(db)
	do := func(method, path string, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://toolmux.test"+path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "http://toolmux.test")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := do("GET", "/settings", nil, nil); w.Code != 303 || w.Header().Get("Location") != "/setup" {
		t.Fatal("first-run gate", w.Code)
	}
	w := do("POST", "/setup", url.Values{"username": {"owner"}, "password": {"correct-test-password"}, "confirm": {"correct-test-password"}}, nil)
	if w.Code != 303 || len(w.Result().Cookies()) != 1 {
		t.Fatalf("setup %d %s", w.Code, w.Body.String())
	}
	cookie := w.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge != 43200 {
		t.Fatal("session cookie attributes")
	}
	if err := db.CreateAdmin(ctx, "intruder", "another-test-password"); err == nil {
		t.Fatal("second setup accepted")
	}
	// A fresh connection and server retain both the administrator and session.
	reopened, err := store.Open(ctx, dsn, box)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	handler = makeHandler(reopened)
	if err = reopened.VerifyAdmin(ctx, "owner", "correct-test-password"); err != nil {
		t.Fatal("password did not persist", err)
	}
	for _, path := range []string{"/", "/agents", "/connections", "/tools", "/providers", "/providers/new", "/settings", "/activity"} {
		if w := do("GET", path, nil, cookie); w.Code != 200 {
			t.Fatalf("page %s: %d %s", path, w.Code, w.Body.String())
		}
		if w := do("GET", path, nil, nil); w.Code != 303 {
			t.Fatalf("unprotected page %s", path)
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-provider-key" {
			t.Error("provider key not forwarded")
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = io.WriteString(w, `{"data":[{"id":"reasoning-model"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":9}}`)
	}))
	defer upstream.Close()
	form := url.Values{"name": {"Test provider"}, "slug": {"test-provider"}, "adapter": {"openai"}, "base_url": {upstream.URL + "/v1"}, "api_key": {"private-provider-key"}, "timeout": {"30"}, "enabled": {"on"}, "models": {"first-model"}}
	w = do("POST", "/providers", form, cookie)
	if w.Code != 303 {
		t.Fatalf("create provider %d %s", w.Code, w.Body.String())
	}
	providers, err := db.ListModelProviders(ctx)
	if err != nil || len(providers) != 1 {
		t.Fatal("provider not saved", err)
	}
	p := providers[0]
	if w = do("POST", "/providers/"+p.ID+"/discover", nil, cookie); w.Code != 303 {
		t.Fatal("discovery", w.Code)
	}
	if w = do("GET", "/providers/"+p.ID, nil, cookie); w.Code != 200 || strings.Contains(w.Body.String(), "private-provider-key") || !strings.Contains(w.Body.String(), "test-provider/reasoning-model") {
		t.Fatal("provider view", w.Code, w.Body.String())
	}
	agent, token, err := db.CreateAgent(ctx, "Test agent", "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	infer := func(path, body string) *httptest.ResponseRecorder {
		method := "POST"
		if path == "/v1/models" {
			method = "GET"
		}
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w = infer("/v1/models", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "test-provider/reasoning-model") {
		t.Fatal("agent model list", w.Code)
	}
	if w = infer("/v1/chat/completions", `{"model":"test-provider/reasoning-model","messages":[]}`); w.Code != 200 {
		t.Fatal("inference", w.Code, w.Body.String())
	}
	if w = do("GET", "/activity?q=test-provider", nil, cookie); w.Code != 200 || !strings.Contains(w.Body.String(), "reasoning-model") || !strings.Contains(w.Body.String(), "11") {
		t.Fatal("model activity", w.Code, w.Body.String())
	}
	p.Enabled = false
	if _, err = db.SaveModelProvider(ctx, p, "", nil); err != nil {
		t.Fatal(err)
	}
	if key, err := db.ModelProviderKey(ctx, p.ID); err != nil || key != "private-provider-key" {
		t.Fatal("empty edit changed key")
	}
	if w = infer("/v1/chat/completions", `{"model":"test-provider/reasoning-model"}`); w.Code != 404 {
		t.Fatal("paused provider accepted")
	}
	if err = db.DisableAgent(ctx, agent.ID); err != nil {
		t.Fatal(err)
	}
	if w = infer("/v1/models", ""); w.Code != 401 {
		t.Fatal("disabled agent accepted")
	}
	r := httptest.NewRequest("POST", "http://toolmux.test/providers", strings.NewReader(form.Encode()))
	r.Header.Set("Origin", "https://other.test")
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross-origin form accepted")
	}
	if w = do("POST", "/login", url.Values{"username": {"owner"}, "password": {"wrong-password"}}, nil); w.Code != 401 {
		t.Fatal("wrong password accepted")
	}
	w = do("POST", "/login", url.Values{"username": {"owner"}, "password": {"correct-test-password"}}, nil)
	if w.Code != 303 || len(w.Result().Cookies()) != 1 {
		t.Fatal("login failed", w.Code)
	}
	second := w.Result().Cookies()[0]
	if w = do("POST", "/logout", nil, second); w.Code != 303 {
		t.Fatal("logout failed")
	}
	if w = do("GET", "/settings", nil, second); w.Code != 303 {
		t.Fatal("logged-out session accepted")
	}
	w = do("POST", "/settings/password", url.Values{"current": {"correct-test-password"}, "password": {"replacement-test-password"}, "confirm": {"replacement-test-password"}}, cookie)
	if w.Code != 303 {
		t.Fatal("password change", w.Code)
	}
	if w = do("GET", "/settings", nil, cookie); w.Code != 303 {
		t.Fatal("old session survived password change")
	}
	if err = db.VerifyAdmin(ctx, "owner", "replacement-test-password"); err != nil {
		t.Fatal("new password failed")
	}
	for i := 0; i < 11; i++ {
		allowed, err := db.AllowLogin(ctx, "test-address")
		if err != nil || allowed != (i < 10) {
			t.Fatal(fmt.Sprint("login limit", i, err))
		}
	}
}
