package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shero4/toolmux/internal/gateway"
	"github.com/shero4/toolmux/internal/store"
)

func TestBrowserGrantRemovalSurvivesCatalogRefresh(t *testing.T) {
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
	session, err := db.NewAdminSession(ctx, owner.ID)
	require(err)
	agent, _, err := db.CreateAgent(ctx, "Fixture", "fixture")
	require(err)
	id, err := db.CreateConnection(ctx, "mcp_http", "Fixture", "fixture", "http://fixture.test", "", "Fixture", "fixture", "none", "", "", nil)
	require(err)
	conn, err := db.GetConnectionSummary(ctx, id)
	require(err)
	catalog := []store.Tool{{UpstreamName: "first", InputSchema: json.RawMessage(`{"type":"object"}`)}, {UpstreamName: "second", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	require(db.ReconcileTools(ctx, conn, catalog))
	require(db.SetAgentConnection(ctx, agent.ID, id, true))
	tools, err := db.ToolsForAgent(ctx, agent.ID)
	require(err)
	if len(tools) != 2 {
		t.Fatal("fixture catalog")
	}
	ui, err := New(db, nil, nil, nil, nil, "http://toolmux.test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	require(err)
	handler := ui.Handler(http.NotFoundHandler(), http.NotFoundHandler(), gateway.New(db, nil))
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	require(form.WriteField("visible_tool_id", tools[0].ID))
	require(form.Close())
	r := httptest.NewRequest("POST", "http://toolmux.test/agents/"+agent.ID+"/grants", &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	r.Header.Set("Origin", "http://toolmux.test")
	r.Header.Set("X-Toolmux-Async", "true")
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	_, _, _, err = db.ResolveGrantedTool(ctx, agent.ID, tools[0].ExposedName)
	if err == nil {
		t.Fatal("browser grant removal did not revoke access")
	}
	require(db.ReconcileTools(ctx, conn, catalog))
	after, err := db.ToolsForAgent(ctx, agent.ID)
	require(err)
	if len(after) != 1 || after[0].ID != tools[1].ID {
		t.Fatal("refresh restored revoked access or removed unrelated access")
	}
	// An empty async request must not claim success without changing anything.
	r = httptest.NewRequest("POST", "http://toolmux.test/agents/"+agent.ID+"/grants", nil)
	r.Header.Set("Origin", "http://toolmux.test")
	r.Header.Set("X-Toolmux-Async", "true")
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("empty save: %d", w.Code)
	}
}
