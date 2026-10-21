package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/shero4/toolmux/internal/checker"
	"github.com/shero4/toolmux/internal/execute"
	"github.com/shero4/toolmux/internal/mcp"
	"github.com/shero4/toolmux/internal/oauth"
	"github.com/shero4/toolmux/internal/store"
)

func TestConnectionEditAndCreateGuards(t *testing.T) {
	db, _, _ := featureStore(t)
	ctx := context.Background()
	require := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	require(db.CreateAdmin(ctx, "owner", "integration-password"))
	owner, err := db.AuthenticateUser(ctx, "owner", "integration-password")
	require(err)
	require(db.CreateUser(ctx, owner.ID, "viewer", "viewer-password", "viewer"))
	viewer, err := db.AuthenticateUser(ctx, "viewer", "viewer-password")
	require(err)
	ownerSession, err := db.NewAdminSession(ctx, owner.ID)
	require(err)
	viewerSession, err := db.NewAdminSession(ctx, viewer.ID)
	require(err)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	health := checker.New(db, execute.New(db, mcp.NewClient()), oauth.New(db, "http://toolmux.test"), log)
	ui, err := New(db, health, nil, nil, nil, "http://toolmux.test", log)
	require(err)
	handler := ui.Handler(http.NotFoundHandler(), http.NotFoundHandler())
	do := func(session, method, path string, form url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://toolmux.test"+path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "http://toolmux.test")
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}

	agent, _, err := db.CreateAgent(ctx, "Fixture", "fixture-agent")
	require(err)
	id, err := db.CreateConnection(ctx, "mcp_http", "Fixture", "fixture", "http://fixture.test/mcp", "", "Fixture account", "fixture-account", "none", "", "", nil)
	require(err)
	conn, err := db.GetConnectionSummary(ctx, id)
	require(err)
	require(db.ReconcileTools(ctx, conn, []store.Tool{{UpstreamName: "read_file", InputSchema: json.RawMessage(`{"type":"object"}`)}}))
	require(db.SetAgentConnection(ctx, agent.ID, id, true))

	// An endpoint pasted into the name fields is rejected instead of becoming
	// the connection's label and tool prefix.
	created := url.Values{"kind": {"mcp_http"}, "auth_method": {"none"}, "endpoint_url": {"http://other.test/mcp"},
		"connector_name": {"http://other.test/mcp"}, "name": {"http://other.test/mcp"}}
	if w := do(ownerSession, "POST", "/connections", created); w.Code != 400 || !strings.Contains(w.Body.String(), "endpoint URL goes in its own field") {
		t.Fatalf("URL as name: %d", w.Code)
	}

	// Reusing a connector slug for a different endpoint no longer repoints the
	// connections that already share it.
	created = url.Values{"kind": {"mcp_http"}, "auth_method": {"none"}, "endpoint_url": {"http://other.test/mcp"},
		"connector_name": {"Other"}, "connector_slug": {"fixture"}, "name": {"Other account"}}
	if w := do(ownerSession, "POST", "/connections", created); w.Code != 400 || !strings.Contains(w.Body.String(), "already belongs to a different endpoint") {
		t.Fatalf("connector conflict: %d", w.Code)
	}
	if conn, err = db.GetConnectionSummary(ctx, id); err != nil || conn.EndpointURL != "http://fixture.test/mcp" {
		t.Fatalf("existing connector was rewritten: %q %v", conn.EndpointURL, err)
	}

	// Viewers can neither open nor submit the edit form.
	if w := do(viewerSession, "GET", "/connections/"+id+"/edit", nil); w.Code != 403 {
		t.Fatalf("viewer edit page: %d", w.Code)
	}
	if w := do(viewerSession, "POST", "/connections/"+id, url.Values{"connector_name": {"X"}, "name": {"X"}}); w.Code != 403 {
		t.Fatalf("viewer update: %d", w.Code)
	}
	if w := do(ownerSession, "GET", "/connections/"+id+"/edit", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `value="fixture-account"`) {
		t.Fatalf("edit page: %d", w.Code)
	}

	edit := url.Values{"connector_name": {"Fixture"}, "name": {"http://fixture.test/mcp"}, "connection_slug": {"fixture-account"}, "endpoint_url": {"http://fixture.test/mcp"}}
	if w := do(ownerSession, "POST", "/connections/"+id, edit); w.Code != 400 {
		t.Fatalf("edit URL as name: %d", w.Code)
	}

	// Renaming moves the tool prefix and keeps the agent's grant.
	edit = url.Values{"connector_name": {"Fixture Server"}, "name": {"Renamed account"}, "connection_slug": {"renamed"}, "endpoint_url": {"http://fixture.test/mcp"}}
	if w := do(ownerSession, "POST", "/connections/"+id, edit); w.Code != 303 {
		t.Fatalf("rename: %d %s", w.Code, w.Body.String())
	}
	conn, err = db.GetConnectionSummary(ctx, id)
	require(err)
	if conn.Name != "Renamed account" || conn.Slug != "renamed" || conn.ConnectorName != "Fixture Server" || conn.ConnectorSlug != "fixture" {
		t.Fatalf("rename not stored: %+v", conn)
	}
	if _, _, _, err = db.ResolveGrantedTool(ctx, agent.ID, "renamed__read_file"); err != nil {
		t.Fatal("grant did not follow the renamed tool:", err)
	}
	if _, _, _, err = db.ResolveGrantedTool(ctx, agent.ID, "fixture-account__read_file"); err == nil {
		t.Fatal("old tool name still resolves")
	}
	if conn.Status != "unchecked" {
		t.Fatalf("a rename alone should not run a check: %s", conn.Status)
	}

	// A slug taken by another connection is refused without partial changes.
	_, err = db.CreateConnection(ctx, "mcp_http", "Taken", "taken", "http://taken.test/mcp", "", "Taken", "taken", "none", "", "", nil)
	require(err)
	edit.Set("connection_slug", "taken")
	if w := do(ownerSession, "POST", "/connections/"+id, edit); w.Code != 400 {
		t.Fatalf("duplicate slug: %d", w.Code)
	}
	if conn, err = db.GetConnectionSummary(ctx, id); err != nil || conn.Slug != "renamed" {
		t.Fatalf("duplicate slug changed the connection: %q %v", conn.Slug, err)
	}

	// Changing the endpoint stores it and re-checks against the new address.
	edit.Set("connection_slug", "renamed")
	edit.Set("endpoint_url", "http://127.0.0.1:1/mcp")
	if w := do(ownerSession, "POST", "/connections/"+id, edit); w.Code != 303 {
		t.Fatalf("endpoint change: %d %s", w.Code, w.Body.String())
	}
	conn, err = db.GetConnectionSummary(ctx, id)
	require(err)
	if conn.EndpointURL != "http://127.0.0.1:1/mcp" || conn.Status != "unreachable" || !strings.Contains(conn.LastError, "127.0.0.1:1") {
		t.Fatalf("endpoint change not checked: %q %q %q", conn.EndpointURL, conn.Status, conn.LastError)
	}
}
