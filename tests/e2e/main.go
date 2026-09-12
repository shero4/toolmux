// Local opt-in E2E setup and API checks. Run from the Toolmux checkout.
// This creates only the named test profile's connections and GLM test provider.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shero4/toolmux/internal/checker"
	"github.com/shero4/toolmux/internal/config"
	"github.com/shero4/toolmux/internal/discovery"
	"github.com/shero4/toolmux/internal/execute"
	"github.com/shero4/toolmux/internal/importer"
	"github.com/shero4/toolmux/internal/mcp"
	"github.com/shero4/toolmux/internal/oauth"
	"github.com/shero4/toolmux/internal/secretbox"
	"github.com/shero4/toolmux/internal/store"
	"gopkg.in/yaml.v3"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func main() {
	profile := flag.String("profile", "", "Path to the existing isolated toolmux-e2e Hermes profile")
	python := flag.String("python", "python", "Python executable used by local fixtures")
	fixture := flag.String("fixture", "tests/e2e/fixture.py", "Local fixture script")
	upstream := flag.String("upstream", "", "Previously configured GLM base URL")
	model := flag.String("model", "glm-5", "GLM model")
	codexEnabled := flag.Bool("enable-codex", false, "Enable the Codex bridge after interactive sign-in succeeds")
	flag.Parse()
	if filepath.Base(*profile) != "toolmux-e2e" || *upstream == "" || os.Getenv("TOOLMUX_E2E_GLM_KEY") == "" {
		panic("Explicit isolated profile, upstream and TOOLMUX_E2E_GLM_KEY required")
	}
	cfg, err := config.Load()
	must(err)
	ctx := context.Background()
	box, err := secretbox.New(cfg.MasterKey)
	must(err)
	db, err := store.Open(ctx, cfg.DatabaseURL, box)
	must(err)
	defer db.Close()
	fixturePath, err := filepath.Abs(*fixture)
	must(err)
	profilePath, err := filepath.Abs(*profile)
	must(err)
	path := filepath.Join(profilePath, "config.yaml")
	raw, err := os.ReadFile(path)
	must(err)
	var pc map[string]any
	must(yaml.Unmarshal(raw, &pc))
	if existing, ok := pc["mcp_servers"].(map[string]any); ok {
		for name := range existing {
			if name != "toolmux" && !strings.HasPrefix(name, "e2e_") {
				panic("Profile has unrelated MCP servers")
			}
		}
	}
	servers := map[string]any{"e2e_remote": map[string]any{"url": "http://127.0.0.1:8082/mcp"}, "e2e_stdio": map[string]any{"command": *python, "args": []string{fixturePath, "stdio"}}}
	// Keep the existing Toolmux token when rerunning the harness.
	if existing, ok := pc["mcp_servers"].(map[string]any); ok {
		if t, ok := existing["toolmux"]; ok {
			servers["toolmux"] = t
		}
	}
	pc["mcp_servers"] = servers
	writeYAML(path, pc)
	scanner := discovery.New(cfg.DiscoveryRoots)
	manager := importer.New(db, scanner, cfg.BaseURL)
	inventory := manager.Scan(ctx)
	selected := importer.Inventory{}
	for _, p := range inventory.Profiles {
		if strings.EqualFold(filepath.Clean(p.Candidate.ConfigPath), filepath.Clean(path)) {
			selected.Profiles = append(selected.Profiles, p)
		}
	}
	if len(selected.Profiles) != 1 {
		panic("Isolated profile not discovered")
	}
	summary, err := manager.ImportAll(ctx, selected)
	must(err)
	if len(summary.Warnings) > 0 {
		panic(strings.Join(summary.Warnings, "; "))
	}
	agentID := summary.AgentIDs[0]
	fmt.Println("PASS: Hermes discovery and isolated profile import")
	raw, err = os.ReadFile(path)
	must(err)
	must(yaml.Unmarshal(raw, &pc))
	servers = pc["mcp_servers"].(map[string]any)
	tmx := servers["toolmux"].(map[string]any)
	token := strings.TrimPrefix(tmx["headers"].(map[string]any)["Authorization"].(string), "Bearer ")
	// Every active MCP entry in the test agent now points to Toolmux.
	pc["mcp_servers"] = map[string]any{"toolmux": tmx}
	pc["model"] = map[string]any{"provider": "custom", "default": "e2e-glm/" + *model, "base_url": cfg.BaseURL + "/v1", "api_key": token, "context_length": 128000}
	pc["providers"] = map[string]any{
		"toolmux-glm":   map[string]any{"base_url": cfg.BaseURL + "/v1", "api_key": token, "transport": "chat_completions", "default_model": "e2e-glm/" + *model},
		"toolmux-codex": map[string]any{"base_url": cfg.BaseURL + "/v1", "api_key": token, "transport": "codex_responses", "default_model": "codex-bridge/codex"},
	}
	pc["toolsets"] = []string{"mcp-toolmux"}
	pc["platform_toolsets"] = map[string]any{"cli": []string{"mcp-toolmux"}}
	writeYAML(path, pc)
	provider := store.ModelProvider{Name: "E2E GLM", Slug: "e2e-glm", Adapter: "openai", BaseURL: *upstream, Enabled: true, TimeoutSeconds: 120}
	providers, err := db.ListModelProviders(ctx)
	must(err)
	for _, p := range providers {
		if p.Slug == provider.Slug {
			provider.ID = p.ID
		}
	}
	provider.ID, err = db.SaveModelProvider(ctx, provider, os.Getenv("TOOLMUX_E2E_GLM_KEY"), []string{*model})
	must(err)
	if key := os.Getenv("TOOLMUX_E2E_BRIDGE_KEY"); key != "" {
		bridge := store.ModelProvider{Name: "Codex bridge", Slug: "codex-bridge", Adapter: "litellm", BaseURL: "http://127.0.0.1:4000/v1", Enabled: *codexEnabled, TimeoutSeconds: 120}
		for _, p := range providers {
			if p.Slug == bridge.Slug {
				bridge.ID = p.ID
			}
		}
		_, err = db.SaveModelProvider(ctx, bridge, key, []string{"codex"})
		must(err)
		fmt.Println("Codex bridge configured; enabled:", *codexEnabled)
	}
	client := mcp.NewClient()
	executor := execute.New(db, client)
	health := checker.New(db, executor, oauth.New(db, cfg.BaseURL), slog.Default())
	ids := append([]string{}, summary.ConnectionIDs...)
	for _, kind := range []string{"http_api", "command"} {
		id, _, err := db.ImportConnection(ctx, store.ImportedConnection{SourceKey: "toolmux-e2e:" + kind, Kind: kind, ConnectorName: "E2E " + kind, ConnectorSlug: "e2e-" + store.Slug(kind), ConnectionName: "Local fixture", ConnectionSlug: "e2e-" + store.Slug(kind), EndpointURL: "http://127.0.0.1:8082", AuthMethod: "none", AgentID: agentID})
		must(err)
		count, err := db.CountToolsForConnection(ctx, id)
		must(err)
		if count == 0 {
			if kind == "http_api" {
				_, err = db.CreateHTTPTool(ctx, id, "status", "Fixture status", "Read the local HTTP fixture verification code", json.RawMessage(`{"type":"object","properties":{}}`), store.HTTPToolSpec{Method: "GET", URLTemplate: "/status", QueryTemplate: json.RawMessage(`{}`), HeadersTemplate: json.RawMessage(`{}`), BodyTemplate: json.RawMessage(`null`), TimeoutMS: 5000, MaxResponseBytes: 65536})
			} else {
				args, _ := json.Marshal([]string{fixturePath, "command"})
				_, err = db.CreateCommandTool(ctx, id, "add", "Fixture sum", "Add two integers in the local command fixture", json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"]}`), store.CommandToolSpec{Executable: *python, ArgsTemplate: args, StdinMode: "json", TimeoutMS: 5000, MaxOutputBytes: 65536})
			}
			must(err)
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		must(health.Check(ctx, id))
		connection, err := db.GetConnectionSummary(ctx, id)
		must(err)
		if connection.Status != "connected" {
			panic("Fixture health check failed: " + connection.Status + " " + connection.LastError)
		}
		must(db.SetAgentConnection(ctx, agentID, id, true))
	}
	fmt.Println("PASS: Remote MCP, stdio MCP, HTTP and command connection checks")
	call := func(tok, path string, value any) (int, map[string]any) {
		var body io.Reader
		if value != nil {
			b, _ := json.Marshal(value)
			body = bytes.NewReader(b)
		}
		method := "GET"
		if value != nil {
			method = "POST"
		}
		r, _ := http.NewRequest(method, cfg.BaseURL+path, body)
		r.Header.Set("Authorization", "Bearer "+tok)
		r.Header.Set("Content-Type", "application/json")
		res, e := (&http.Client{Timeout: 150 * time.Second}).Do(r)
		must(e)
		defer res.Body.Close()
		var v map[string]any
		_ = json.NewDecoder(res.Body).Decode(&v)
		return res.StatusCode, v
	}
	rpc := func(name string, args any) map[string]any {
		return map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}}
	}
	tools, err := db.ToolsForAgent(ctx, agentID)
	must(err)
	if len(tools) != 6 {
		panic(fmt.Sprintf("Expected six fixture tools, got %d", len(tools)))
	}
	for _, tool := range tools {
		status, v := call(token, "/mcp", rpc(tool.ExposedName, map[string]any{"a": 37, "b": 19}))
		if status != 200 || v["error"] != nil {
			panic("Tool call failed: " + tool.ExposedName)
		}
		result, ok := v["result"].(map[string]any)
		if !ok || result["isError"] == true {
			panic(fmt.Sprintf("Tool execution failed %s: %v", tool.ExposedName, v))
		}
		fmt.Println("PASS: proxied tool", tool.ExposedName)
	}
	// Remove one fixture connection and prove cached tool names cannot bypass access.
	first := tools[0]
	must(db.SetAgentConnection(ctx, agentID, first.ConnectionID, false))
	_, denied := call(token, "/mcp", rpc(first.ExposedName, map[string]any{"a": 1, "b": 2}))
	must(db.SetAgentConnection(ctx, agentID, first.ConnectionID, true))
	if denied["error"] == nil {
		panic("Revoked connection remained callable")
	}
	fmt.Println("PASS: revoked tool grant denied")
	disposable, err := db.IssueAgentToken(ctx, agentID, "e2e-revocation")
	must(err)
	status, _ := call(disposable, "/v1/models", nil)
	if status != 200 {
		panic("New token rejected")
	}
	must(db.RevokeAgentToken(ctx, disposable))
	status, _ = call(disposable, "/v1/models", nil)
	if status != 401 {
		panic("Revoked model token accepted")
	}
	status, _ = call(disposable, "/mcp", rpc(first.ExposedName, map[string]any{}))
	if status != 401 {
		panic("Revoked MCP token accepted")
	}
	fmt.Println("PASS: new token accepted; revoked token rejected by model and MCP endpoints")
	status, catalog := call(token, "/v1/models", nil)
	if status != 200 {
		panic("Catalog failed")
	}
	encoded, _ := json.Marshal(catalog)
	if !bytes.Contains(encoded, []byte("e2e-glm/"+*model)) {
		panic("Model missing from catalog")
	}
	fmt.Println("PASS: shared model catalog includes GLM test route")
	// Live inference is performed by Hermes, not duplicated here.
	metadata := map[string]any{"agent_id": agentID, "provider_id": provider.ID, "connection_ids": ids, "model": "e2e-glm/" + *model, "profile": profilePath}
	b, _ := json.MarshalIndent(metadata, "", "  ")
	must(os.MkdirAll("tmp/e2e", 0700))
	must(os.WriteFile("tmp/e2e/metadata.json", b, 0600))
	fmt.Println("Ready for Hermes live inference; no credentials written to test output.")
}
func writeYAML(path string, value any) {
	b, err := yaml.Marshal(value)
	must(err)
	must(os.WriteFile(path, b, 0600))
}
