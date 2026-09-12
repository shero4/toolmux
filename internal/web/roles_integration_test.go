package web

import (
	"context"
	"crypto/sha256"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/shero4/toolmux/internal/migrate"
	"github.com/shero4/toolmux/internal/store"
)

func TestBrowserRoles(t *testing.T) {
	db, _, _ := featureStore(t)
	ctx := context.Background()
	if err := db.CreateAdmin(ctx, "owner", "initial-owner-password"); err != nil {
		t.Fatal(err)
	}
	owner, err := db.AuthenticateUser(ctx, "owner", "initial-owner-password")
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"operator", "viewer"} {
		if err = db.CreateUser(ctx, owner.ID, role, "initial-user-password", role); err != nil {
			t.Fatal(err)
		}
	}
	ui, err := New(db, nil, nil, nil, nil, "http://toolmux.test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	handler := ui.Handler(http.NotFoundHandler(), http.NotFoundHandler())
	cookies := map[string]*http.Cookie{}
	people := map[string]store.User{"admin": owner}
	for _, role := range []string{"operator", "viewer"} {
		u, err := db.AuthenticateUser(ctx, role, "initial-user-password")
		if err != nil {
			t.Fatal(err)
		}
		people[role] = u
	}
	for role, u := range people {
		token, err := db.NewAdminSession(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		cookies[role] = &http.Cookie{Name: sessionCookie, Value: token}
	}
	do := func(role, method, path string, form url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://toolmux.test"+path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "http://toolmux.test")
		r.AddCookie(cookies[role])
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, role := range []string{"admin", "operator", "viewer"} {
		for _, path := range []string{"/", "/agents", "/connections", "/tools", "/providers", "/activity", "/settings"} {
			w := do(role, "GET", path, nil)
			if w.Code != 200 {
				t.Fatalf("%s %s=%d %s", role, path, w.Code, w.Body.String())
			}
			if role == "viewer" && strings.Contains(w.Body.String(), `class="btn btn-primary" href="/providers/new"`) {
				t.Fatal("viewer sees add provider")
			}
		}
		expected := 403
		if role == "admin" {
			expected = 200
		}
		if w := do(role, "GET", "/users", nil); w.Code != expected {
			t.Fatalf("%s users=%d", role, w.Code)
		}
		for _, path := range []string{"/agents/new", "/connections/new", "/providers/new"} {
			expected = 200
			if role == "viewer" {
				expected = 403
			}
			if w := do(role, "GET", path, nil); w.Code != expected {
				t.Fatalf("%s %s=%d", role, path, w.Code)
			}
		}
	}
	for _, path := range []string{"/agents", "/connections", "/tools", "/providers", "/agents/a/tokens", "/agents/a/grants", "/connections/a/check", "/providers/a/discover", "/users", "/users/any/access"} {
		if w := do("viewer", "POST", path, nil); w.Code != 403 {
			t.Fatalf("viewer mutation %s=%d", path, w.Code)
		}
	}
	if w := do("viewer", "GET", "/oauth/callback", nil); w.Code != 303 {
		t.Fatalf("state-gated OAuth callback unavailable: %d", w.Code)
	}
	if w := do("operator", "POST", "/users", url.Values{"username": {"escalated"}, "password": {"initial-user-password"}, "confirm": {"initial-user-password"}, "role": {"admin"}}); w.Code != 403 {
		t.Fatal("operator created admin")
	}
	if err := db.CreateUser(ctx, people["operator"].ID, "escalated", "initial-user-password", "admin"); err != store.ErrUserAccess {
		t.Fatal("store allowed operator escalation", err)
	}
	if w := do("operator", "POST", "/agents", url.Values{"name": {"Operator agent"}, "slug": {"operator-agent"}}); w.Code != 303 {
		t.Fatal("operator cannot manage agents", w.Code, w.Body.String())
	}
	if w := do("admin", "POST", "/users", url.Values{"username": {"new-user"}, "password": {"initial-user-password"}, "confirm": {"initial-user-password"}, "role": {"viewer"}}); w.Code != 303 {
		t.Fatal("admin cannot add user", w.Code, w.Body.String())
	}
	if err = db.SetUserAccess(ctx, owner.ID, owner.ID, "viewer", true); err != store.ErrLastAdmin {
		t.Fatal("last administrator demoted", err)
	}
	if err = db.SetUserAccess(ctx, owner.ID, owner.ID, "admin", false); err != store.ErrLastAdmin {
		t.Fatal("last administrator disabled", err)
	}
	if w := do("viewer", "POST", "/settings/password", url.Values{"current": {"initial-user-password"}, "password": {"new-viewer-password"}, "confirm": {"new-viewer-password"}}); w.Code != 303 {
		t.Fatal("viewer cannot change own password", w.Code)
	}
	if _, err = db.SessionUser(ctx, cookies["viewer"].Value); err == nil {
		t.Fatal("password change kept viewer session")
	}
	if _, err = db.SessionUser(ctx, cookies["admin"].Value); err != nil {
		t.Fatal("viewer password revoked admin session")
	}
	if err = db.SetUserAccess(ctx, owner.ID, people["operator"].ID, "viewer", true); err != nil {
		t.Fatal(err)
	}
	if w := do("operator", "POST", "/agents", nil); w.Code != 303 {
		t.Fatal("demoted user's old session still active", w.Code)
	}
	if err = db.SetUserAccess(ctx, owner.ID, people["viewer"].ID, "viewer", false); err != nil {
		t.Fatal(err)
	}
	if _, err = db.AuthenticateUser(ctx, "viewer", "new-viewer-password"); err == nil {
		t.Fatal("disabled user logged in")
	}
}

func TestLastAdministratorConcurrentChanges(t *testing.T) {
	db, _, _ := featureStore(t)
	ctx := context.Background()
	if err := db.CreateAdmin(ctx, "first", "initial-admin-password"); err != nil {
		t.Fatal(err)
	}
	first, _ := db.AuthenticateUser(ctx, "first", "initial-admin-password")
	if err := db.CreateUser(ctx, first.ID, "second", "initial-admin-password", "admin"); err != nil {
		t.Fatal(err)
	}
	second, _ := db.AuthenticateUser(ctx, "second", "initial-admin-password")
	var wg sync.WaitGroup
	outcomes := make(chan error, 2)
	for _, u := range []store.User{first, second} {
		wg.Add(1)
		go func(u store.User) { defer wg.Done(); outcomes <- db.SetUserAccess(ctx, u.ID, u.ID, "viewer", true) }(u)
	}
	wg.Wait()
	close(outcomes)
	successes := 0
	for err := range outcomes {
		if err == nil {
			successes++
		}
	}
	users, err := db.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	admins := 0
	for _, u := range users {
		if u.IsAdmin() {
			admins++
		}
	}
	if successes != 1 || admins != 1 {
		t.Fatalf("successes=%d admins=%d", successes, admins)
	}
}

func TestExistingAdministratorMigration(t *testing.T) {
	db, dsn, _ := featureStore(t, 8)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	hash, _ := store.PasswordHash("existing-admin-password")
	if _, err = conn.Exec(ctx, `INSERT INTO admin_account(username,password_hash) VALUES('existing',$1)`, hash); err != nil {
		t.Fatal(err)
	}
	tokenHash := sha256.Sum256([]byte("existing-session"))
	if _, err = conn.Exec(ctx, `INSERT INTO admin_sessions(token_hash,expires_at) VALUES($1,now()+interval '1 hour')`, tokenHash[:]); err != nil {
		t.Fatal(err)
	}
	all, _ := migrate.All()
	set := map[int]string{}
	for _, m := range all {
		set[m.Version] = m.SQL
	}
	if err = db.Migrate(ctx, set); err != nil {
		t.Fatal(err)
	}
	u, err := db.AuthenticateUser(ctx, "existing", "existing-admin-password")
	if err != nil || !u.IsAdmin() {
		t.Fatal("account migration", err, u)
	}
	session, err := db.SessionUser(ctx, "existing-session")
	if err != nil || session.ID != u.ID {
		t.Fatal("session migration", err)
	}
}
