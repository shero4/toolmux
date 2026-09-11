package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shero4/toolmux/internal/checker"
	"github.com/shero4/toolmux/internal/discovery"
	"github.com/shero4/toolmux/internal/importer"
	"github.com/shero4/toolmux/internal/oauth"
	"github.com/shero4/toolmux/internal/store"
)

//go:embed templates/*.html static/app.js static/bundle.css static/favicon.svg
var assets embed.FS

type Server struct {
	store     *store.Store
	checker   *checker.Checker
	oauth     *oauth.Manager
	discovery *discovery.Scanner
	importer  *importer.Manager
	baseURL   string
	log       *slog.Logger
	templates *template.Template
}

type pageData struct {
	Page, Title, Notice, Error, Token      string
	Query, KindFilter, StatusFilter        string
	DecisionFilter, ConnectionFilter       string
	AgentFilter, ReturnURL                 string
	Agents                                 []store.Agent
	AgentOptions                           []store.Agent
	Discovered                             []discovery.Candidate
	Scanned                                bool
	Setup                                  setupGuide
	Agent                                  store.Agent
	Connections                            []store.Connection
	ConnectionOptions                      []store.Connection
	Tools                                  []store.Tool
	Events                                 []store.AuditEvent
	Hermes                                 importer.Inventory
	AgentCount, ConnectionCount, ToolCount int
	Pager                                  pager
}

type pager struct {
	Page, Pages, Total, From, To int
	PreviousURL, NextURL         string
	HasPrevious, HasNext         bool
}

type setupGuide struct {
	Title, Destination, Config, Note string
}

var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func New(store *store.Store, check *checker.Checker, oauth *oauth.Manager, discoveryScanner *discovery.Scanner, hermesImporter *importer.Manager, baseURL string, log *slog.Logger) (*Server, error) {
	functions := template.FuncMap{"date": func(value *time.Time) string {
		if value == nil {
			return "Never"
		}
		return value.Local().Format("Jan 2, 15:04")
	}, "time": func(value time.Time) string { return value.Local().Format("Jan 2, 15:04:05") }, "status": statusLabel, "kind": kindLabel, "runtime": runtimeLabel, "short": func(value string) string {
		const limit = 220
		value = strings.TrimSpace(value)
		if len(value) <= limit {
			return value
		}
		return strings.TrimSpace(value[:limit]) + "…"
	}}
	templates, err := template.New("pages.html").Funcs(functions).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{store: store, checker: check, oauth: oauth, discovery: discoveryScanner, importer: hermesImporter, baseURL: strings.TrimRight(baseURL, "/"), log: log, templates: templates}, nil
}

func statusLabel(value string) string {
	switch value {
	case "reauthorization_required":
		return "Needs authorization"
	case "active":
		return "Active"
	case "connected":
		return "Connected"
	case "checking":
		return "Checking"
	case "degraded":
		return "Degraded"
	case "unreachable":
		return "Unreachable"
	case "disabled":
		return "Disabled"
	default:
		return strings.ReplaceAll(value, "_", " ")
	}
}

func kindLabel(value string) string {
	switch value {
	case "mcp_http":
		return "Remote MCP"
	case "mcp_stdio":
		return "Local MCP"
	case "http_api":
		return "HTTP API"
	case "mcp":
		return "MCP"
	case "http":
		return "HTTP"
	case "command":
		return "Command"
	default:
		return value
	}
}

func runtimeLabel(value string) string {
	switch strings.ToLower(value) {
	case "hermes":
		return "Hermes"
	case "openclaw":
		return "OpenClaw"
	default:
		return value
	}
}

func (s *Server) Handler(mcpHandler http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServer(http.FS(assets)))
	mux.Handle("/mcp", mcpHandler)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /{$}", s.dashboard)
	mux.HandleFunc("GET /connections", s.connections)
	mux.HandleFunc("POST /connections", s.createConnection)
	mux.HandleFunc("POST /connections/{id}/check", s.checkConnection)
	mux.HandleFunc("POST /connections/{id}/credential", s.updateConnectionCredential)
	mux.HandleFunc("GET /connections/{id}/authorize", s.authorizeConnection)
	mux.HandleFunc("GET /oauth/callback", s.oauthCallback)
	mux.HandleFunc("GET /agents", s.agents)
	mux.HandleFunc("POST /agents/import", s.importAgent)
	mux.HandleFunc("GET /imports/hermes", s.hermesImport)
	mux.HandleFunc("POST /imports/hermes", s.importHermes)
	mux.HandleFunc("GET /tools", s.tools)
	mux.HandleFunc("POST /tools", s.createTool)
	mux.HandleFunc("POST /agents", s.createAgent)
	mux.HandleFunc("GET /agents/{id}", s.agentAccess)
	mux.HandleFunc("POST /agents/{id}/grants", s.saveGrants)
	mux.HandleFunc("POST /agents/{id}/disable", s.disableAgent)
	mux.HandleFunc("POST /agents/{id}/delete", s.deleteAgent)
	mux.HandleFunc("GET /activity", s.activity)
	return s.securityHeaders(s.sameOrigin(mux))
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	agents, connections, tools, err := s.store.Summary(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	items, _ := s.store.ListConnections(r.Context())
	if len(items) > 6 {
		items = items[:6]
	}
	events, _ := s.store.ListAudit(r.Context(), 8)
	s.render(w, pageData{Page: "dashboard", Title: "Overview", AgentCount: agents, ConnectionCount: connections, ToolCount: tools, Connections: items, Events: events})
}
func (s *Server) connections(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListConnections(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	kind, status := r.URL.Query().Get("kind"), r.URL.Query().Get("status")
	filtered := items[:0]
	for _, item := range items {
		matchesQuery := query == "" || containsFold(item.Name, query) || containsFold(item.ConnectorName, query) || containsFold(item.Slug, query)
		if matchesQuery && (kind == "" || item.Kind == kind) && (status == "" || item.Status == status) {
			filtered = append(filtered, item)
		}
	}
	page := pageNumber(r)
	pagination := newPager(r, page, len(filtered), 15)
	filtered = pageSlice(filtered, pagination, 15)
	s.render(w, pageData{Page: "connections", Title: "Connections", Connections: filtered, Query: query, KindFilter: kind, StatusFilter: status, Pager: pagination, Notice: r.URL.Query().Get("notice"), Error: r.URL.Query().Get("error")})
}
func (s *Server) agents(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListAgents(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	allItems := items
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query != "" {
		items = nil
		for _, item := range allItems {
			if containsFold(item.Name, query) || containsFold(item.Slug, query) || containsFold(item.Runtime, query) || containsFold(item.Profile, query) {
				items = append(items, item)
			}
		}
	}
	page := pageNumber(r)
	const pageSize = 20
	pagination := newPager(r, page, len(items), pageSize)
	items = pageSlice(items, pagination, pageSize)
	data := pageData{Page: "agents", Title: "Agents", Agents: items, Query: query, Pager: pagination, Token: r.URL.Query().Get("token"), Notice: r.URL.Query().Get("notice"), Error: r.URL.Query().Get("error")}
	if r.URL.Query().Get("discover") == "1" {
		data.Scanned = true
		imported := make(map[string]bool)
		for _, agent := range allItems {
			imported[agent.SourceKey] = true
		}
		for _, candidate := range s.discovery.Scan(r.Context()) {
			if !imported[candidate.ID] {
				data.Discovered = append(data.Discovered, candidate)
			}
		}
	}
	if data.Token != "" {
		if agent, err := s.store.GetAgent(r.Context(), r.URL.Query().Get("agent")); err == nil {
			data.Setup = s.setupGuide(agent, data.Token)
		}
	}
	s.render(w, data)
}

func (s *Server) hermesImport(w http.ResponseWriter, r *http.Request) {
	inventory, err := s.importer.Scan(r.Context())
	if err != nil {
		s.log.Error("scan Hermes configuration", "error", err)
		s.render(w, pageData{Page: "hermes-import", Title: "Import Hermes", Error: "Hermes configurations could not be read."})
		return
	}
	s.render(w, pageData{Page: "hermes-import", Title: "Import Hermes", Hermes: inventory, Notice: r.URL.Query().Get("notice"), Error: r.URL.Query().Get("error")})
}

func (s *Server) importHermes(w http.ResponseWriter, r *http.Request) {
	inventory, err := s.importer.Scan(r.Context())
	if err != nil {
		s.redirectError(w, r, "/imports/hermes", "Hermes configurations could not be read")
		return
	}
	if len(inventory.Profiles) == 0 {
		s.redirectError(w, r, "/imports/hermes", "No Hermes profiles were found")
		return
	}
	summary, err := s.importer.ImportAll(r.Context(), inventory)
	if err != nil {
		s.log.Error("import Hermes configuration", "error", err)
		s.redirectError(w, r, "/imports/hermes", "The Hermes import stopped before it completed")
		return
	}
	connectionIDs := append([]string(nil), summary.ConnectionIDs...)
	go func() {
		seen := make(map[string]bool)
		sem := make(chan struct{}, 4)
		var checks sync.WaitGroup
		for _, id := range connectionIDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			checks.Add(1)
			go func(connectionID string) {
				defer checks.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				defer cancel()
				s.runCheck(ctx, connectionID)
			}(id)
		}
		checks.Wait()
	}()
	message := fmt.Sprintf("Imported %d agents and %d connections. Connection checks are running.", summary.AgentsCreated, summary.ConnectionsCreated)
	if summary.ConfigsUpdated > 0 {
		message += fmt.Sprintf(" Connected %d Hermes profiles to Toolmux.", summary.ConfigsUpdated)
	}
	if len(summary.Warnings) > 0 {
		message += fmt.Sprintf(" %d profile configurations need attention.", len(summary.Warnings))
		for _, warning := range summary.Warnings {
			s.log.Warn("Hermes configuration needs attention", "detail", warning)
		}
	}
	http.Redirect(w, r, "/connections?notice="+url.QueryEscape(message), http.StatusSeeOther)
}

func (s *Server) importAgent(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectError(w, r, "/agents?discover=1", "Invalid form")
		return
	}
	candidate, ok := s.discovery.Find(r.Context(), r.FormValue("candidate_id"))
	if !ok {
		s.redirectError(w, r, "/agents?discover=1", "That agent configuration is no longer available")
		return
	}
	slug := store.Slug(candidate.Runtime + "-" + candidate.Profile)
	agent, token, err := s.store.CreateDiscoveredAgent(r.Context(), candidate.Name, slug, candidate.ID, candidate.Runtime, candidate.Profile, candidate.Environment, candidate.ConfigPath)
	if err != nil {
		s.log.Error("import discovered agent", "error", err)
		s.redirectError(w, r, "/agents?discover=1", "Could not import this agent; it may already exist")
		return
	}
	target := "/agents?notice=" + url.QueryEscape("Agent imported. Connect it with the configuration below; the token will not be shown again.") + "&token=" + url.QueryEscape(token) + "&agent=" + url.QueryEscape(agent.ID)
	http.Redirect(w, r, target, http.StatusSeeOther)
}
func (s *Server) tools(w http.ResponseWriter, r *http.Request) {
	connections, err := s.store.ListConnections(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	kind, connectionID := r.URL.Query().Get("kind"), r.URL.Query().Get("connection")
	page := pageNumber(r)
	const pageSize = 25
	items, total, err := s.store.SearchTools(r.Context(), query, kind, connectionID, pageSize, (page-1)*pageSize)
	if err != nil {
		s.fail(w, err)
		return
	}
	pagination := newPager(r, page, total, pageSize)
	if pagination.Page != page {
		items, _, err = s.store.SearchTools(r.Context(), query, kind, connectionID, pageSize, (pagination.Page-1)*pageSize)
		if err != nil {
			s.fail(w, err)
			return
		}
	}
	s.render(w, pageData{Page: "tools", Title: "Tools", Tools: items, ConnectionOptions: connections, Query: query, KindFilter: kind, ConnectionFilter: connectionID, Pager: pagination, Notice: r.URL.Query().Get("notice"), Error: r.URL.Query().Get("error")})
}
func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	agents, err := s.store.ListAgents(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	decision, agentID := r.URL.Query().Get("decision"), r.URL.Query().Get("agent")
	page := pageNumber(r)
	const pageSize = 30
	events, total, err := s.store.SearchAudit(r.Context(), query, decision, agentID, pageSize, (page-1)*pageSize)
	if err != nil {
		s.fail(w, err)
		return
	}
	pagination := newPager(r, page, total, pageSize)
	if pagination.Page != page {
		events, _, err = s.store.SearchAudit(r.Context(), query, decision, agentID, pageSize, (pagination.Page-1)*pageSize)
		if err != nil {
			s.fail(w, err)
			return
		}
	}
	s.render(w, pageData{Page: "activity", Title: "Activity", Events: events, AgentOptions: agents, Query: query, DecisionFilter: decision, AgentFilter: agentID, Pager: pagination})
}

func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectError(w, r, "/agents", "Invalid form")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	slug := store.Slug(r.FormValue("slug"))
	if slug == "" {
		slug = store.Slug(name)
	}
	if name == "" || slug == "" {
		s.redirectError(w, r, "/agents", "Name and slug are required")
		return
	}
	agent, token, err := s.store.CreateAgent(r.Context(), name, slug)
	if err != nil {
		s.redirectError(w, r, "/agents", "Could not create agent")
		return
	}
	http.Redirect(w, r, "/agents?notice="+url.QueryEscape("Agent created. Connect it with the configuration below; the token will not be shown again.")+"&token="+url.QueryEscape(token)+"&agent="+url.QueryEscape(agent.ID), http.StatusSeeOther)
}

func (s *Server) setupGuide(agent store.Agent, token string) setupGuide {
	endpoint := s.baseURL + "/mcp"
	switch agent.Runtime {
	case "hermes":
		config := "mcp_servers:\n  toolmux:\n    url: \"" + endpoint + "\"\n    headers:\n      Authorization: \"Bearer " + token + "\"\n    enabled: true\n    supports_parallel_tool_calls: true"
		return setupGuide{Title: "Connect Hermes", Destination: agent.ConfigPath, Config: config, Note: "Merge this server into mcp_servers, then restart that Hermes profile."}
	case "openclaw":
		serverName := "toolmux-" + agent.Slug
		payload, _ := json.Marshal(map[string]any{"url": endpoint, "transport": "streamable-http", "headers": map[string]string{"Authorization": "Bearer " + token}})
		command := "openclaw mcp set " + serverName + " '" + string(payload) + "'"
		return setupGuide{Title: "Connect OpenClaw", Destination: agent.ConfigPath, Config: command, Note: "Run this in the detected environment, then run openclaw mcp probe " + serverName + "."}
	default:
		return setupGuide{Title: "Connect this agent", Destination: "MCP client settings", Config: "URL: " + endpoint + "\nAuthorization: Bearer " + token, Note: "Use Streamable HTTP transport."}
	}
}

func (s *Server) createConnection(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectError(w, r, "/connections", "Invalid form")
		return
	}
	connector := strings.TrimSpace(r.FormValue("connector_name"))
	connectorSlug := store.Slug(r.FormValue("connector_slug"))
	if connectorSlug == "" {
		connectorSlug = store.Slug(connector)
	}
	name := strings.TrimSpace(r.FormValue("name"))
	connectionSlug := store.Slug(r.FormValue("connection_slug"))
	if connectionSlug == "" {
		connectionSlug = store.Slug(name)
	}
	endpoint := strings.TrimSpace(r.FormValue("endpoint_url"))
	kind := r.FormValue("kind")
	healthPath := strings.TrimSpace(r.FormValue("health_path"))
	auth := r.FormValue("auth_method")
	authName := strings.TrimSpace(r.FormValue("auth_name"))
	secret := strings.TrimSpace(r.FormValue("secret"))
	if kind != "mcp_http" && kind != "http_api" && kind != "command" {
		s.redirectError(w, r, "/connections", "Choose a supported connection type")
		return
	}
	if connector == "" || connectorSlug == "" || name == "" || connectionSlug == "" || (kind != "command" && !validHTTPURL(endpoint)) {
		s.redirectError(w, r, "/connections", "Enter names and a valid HTTP endpoint")
		return
	}
	if healthPath != "" && !strings.HasPrefix(healthPath, "/") {
		s.redirectError(w, r, "/connections", "Health path must start with /")
		return
	}
	if auth != "none" && auth != "bearer" && auth != "header" && auth != "oauth2" {
		s.redirectError(w, r, "/connections", "Unsupported authorization method")
		return
	}
	if (auth == "bearer" || auth == "header") && secret == "" {
		s.redirectError(w, r, "/connections", "A credential is required")
		return
	}
	if auth == "header" && authName == "" {
		s.redirectError(w, r, "/connections", "Enter the API credential header name")
		return
	}
	var oauthConfig *store.OAuthConfig
	if auth == "oauth2" {
		authorizationURL := strings.TrimSpace(r.FormValue("authorization_url"))
		tokenURL := strings.TrimSpace(r.FormValue("token_url"))
		clientID := strings.TrimSpace(r.FormValue("client_id"))
		tokenAuthMethod := r.FormValue("token_auth_method")
		if tokenAuthMethod == "" {
			tokenAuthMethod = "client_secret_basic"
		}
		if !validHTTPURL(authorizationURL) || !validHTTPURL(tokenURL) || clientID == "" || (tokenAuthMethod != "client_secret_basic" && tokenAuthMethod != "client_secret_post") {
			s.redirectError(w, r, "/connections", "OAuth authorization URL, token URL, and client ID are required")
			return
		}
		oauthConfig = &store.OAuthConfig{AuthorizationURL: authorizationURL, TokenURL: tokenURL, ClientID: clientID, Scopes: strings.TrimSpace(r.FormValue("scopes")), TokenAuthMethod: tokenAuthMethod}
	}
	id, err := s.store.CreateConnection(r.Context(), kind, connector, connectorSlug, endpoint, healthPath, name, connectionSlug, auth, authName, secret, oauthConfig)
	if err != nil {
		s.log.Error("create connection", "error", err)
		s.redirectError(w, r, "/connections", "Could not create connection")
		return
	}
	s.runCheck(r.Context(), id)
	http.Redirect(w, r, "/connections?notice="+url.QueryEscape("Connection created and checked."), http.StatusSeeOther)
}

func (s *Server) createTool(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectError(w, r, "/tools", "Invalid form")
		return
	}
	kind := r.FormValue("kind")
	connectionID := r.FormValue("connection_id")
	name := strings.TrimSpace(r.FormValue("name"))
	title := strings.TrimSpace(r.FormValue("title"))
	description := strings.TrimSpace(r.FormValue("description"))
	inputSchema := json.RawMessage(strings.TrimSpace(r.FormValue("input_schema")))
	if connectionID == "" || !toolNamePattern.MatchString(name) || !validJSONObject(inputSchema) {
		s.redirectError(w, r, "/tools", "Choose a connection, use a simple tool name, and provide a JSON object schema")
		return
	}
	var err error
	switch kind {
	case "http":
		method := strings.ToUpper(r.FormValue("method"))
		location := strings.TrimSpace(r.FormValue("url_template"))
		query := jsonOrDefault(r.FormValue("query_template"), `{}`)
		headers := jsonOrDefault(r.FormValue("headers_template"), `{}`)
		body := jsonOrDefault(r.FormValue("body_template"), `null`)
		if !validURLTemplate(location) || !validStringMap(query) || !validStringMap(headers) || !json.Valid(body) {
			s.redirectError(w, r, "/tools", "HTTP URL and JSON templates are invalid")
			return
		}
		err = s.store.CreateHTTPTool(r.Context(), connectionID, name, title, description, inputSchema, store.HTTPToolSpec{Method: method, URLTemplate: location, QueryTemplate: query, HeadersTemplate: headers, BodyTemplate: body, TimeoutMS: durationMS(r.FormValue("http_timeout_seconds"), 60), MaxResponseBytes: 4 << 20})
	case "command":
		executable := strings.TrimSpace(r.FormValue("executable"))
		workingDirectory := strings.TrimSpace(r.FormValue("working_directory"))
		args := jsonOrDefault(r.FormValue("args_template"), `[]`)
		stdinMode := r.FormValue("stdin_mode")
		credentialEnv := strings.TrimSpace(r.FormValue("credential_env"))
		var values []string
		if executable == "" || json.Unmarshal(args, &values) != nil || (stdinMode != "none" && stdinMode != "json") {
			s.redirectError(w, r, "/tools", "Command executable or arguments are invalid")
			return
		}
		err = s.store.CreateCommandTool(r.Context(), connectionID, name, title, description, inputSchema, store.CommandToolSpec{Executable: executable, WorkingDirectory: workingDirectory, ArgsTemplate: args, StdinMode: stdinMode, CredentialEnv: credentialEnv, TimeoutMS: durationMS(r.FormValue("command_timeout_seconds"), 60), MaxOutputBytes: 4 << 20})
	default:
		s.redirectError(w, r, "/tools", "Choose HTTP API or command")
		return
	}
	if err != nil {
		s.log.Error("create tool", "error", err)
		s.redirectError(w, r, "/tools", "Could not create tool; check that its type matches the connection")
		return
	}
	_ = s.checker.Check(r.Context(), connectionID)
	http.Redirect(w, r, "/tools?notice="+url.QueryEscape("Tool created."), http.StatusSeeOther)
}

func (s *Server) authorizeConnection(w http.ResponseWriter, r *http.Request) {
	target, err := s.oauth.Start(r.Context(), r.PathValue("id"))
	if err != nil {
		s.redirectError(w, r, "/connections", "Could not begin OAuth authorization")
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Server) oauthCallback(w http.ResponseWriter, r *http.Request) {
	if providerError := r.URL.Query().Get("error"); providerError != "" {
		s.redirectError(w, r, "/connections", "OAuth authorization was not completed: "+providerError)
		return
	}
	stateValue, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	if stateValue == "" || code == "" {
		s.redirectError(w, r, "/connections", "OAuth callback was incomplete")
		return
	}
	connectionID, err := s.oauth.Complete(r.Context(), stateValue, code)
	if err != nil {
		s.log.Error("complete OAuth authorization", "error", err)
		s.redirectError(w, r, "/connections", "OAuth token exchange failed")
		return
	}
	if err := s.checker.Check(r.Context(), connectionID); err != nil {
		s.log.Error("check OAuth connection", "error", err)
	}
	http.Redirect(w, r, "/connections?notice="+url.QueryEscape("OAuth connection authorized."), http.StatusSeeOther)
}

func (s *Server) checkConnection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.runCheck(r.Context(), id)
	http.Redirect(w, r, "/connections?notice="+url.QueryEscape("Connection check completed."), http.StatusSeeOther)
}

func (s *Server) updateConnectionCredential(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	connection, _, err := s.store.GetConnection(r.Context(), id)
	if err != nil {
		s.redirectError(w, r, "/connections", "Connection not found")
		return
	}
	if connection.AuthMethod != "bearer" && connection.AuthMethod != "header" {
		s.redirectError(w, r, "/connections", "This connection does not use a replaceable key")
		return
	}
	if err := r.ParseForm(); err != nil || strings.TrimSpace(r.FormValue("secret")) == "" {
		s.redirectError(w, r, "/connections", "Enter a credential")
		return
	}
	if err := s.store.SaveCredential(r.Context(), id, store.Credential{BearerToken: strings.TrimSpace(r.FormValue("secret"))}); err != nil {
		s.redirectError(w, r, "/connections", "The credential could not be saved")
		return
	}
	s.runCheck(r.Context(), id)
	http.Redirect(w, r, "/connections?notice="+url.QueryEscape("Credential updated and checked."), http.StatusSeeOther)
}

func (s *Server) runCheck(ctx context.Context, id string) {
	if err := s.checker.Check(ctx, id); err != nil {
		s.log.Error("check connection", "error", err)
	}
}

func (s *Server) agentAccess(w http.ResponseWriter, r *http.Request) {
	agent, err := s.store.GetAgent(r.Context(), r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	connections, err := s.store.ListConnections(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	kind, connectionID := r.URL.Query().Get("kind"), r.URL.Query().Get("connection")
	page := pageNumber(r)
	const pageSize = 40
	tools, total, err := s.store.SearchToolsForAgentGrant(r.Context(), agent.ID, query, kind, connectionID, pageSize, (page-1)*pageSize)
	if err != nil {
		s.fail(w, err)
		return
	}
	pagination := newPager(r, page, total, pageSize)
	if pagination.Page != page {
		tools, _, err = s.store.SearchToolsForAgentGrant(r.Context(), agent.ID, query, kind, connectionID, pageSize, (pagination.Page-1)*pageSize)
		if err != nil {
			s.fail(w, err)
			return
		}
	}
	returnURL := r.URL.RequestURI()
	s.render(w, pageData{Page: "agent-access", Title: "Agent access", Agent: agent, Tools: tools, ConnectionOptions: connections, Query: query, KindFilter: kind, ConnectionFilter: connectionID, Pager: pagination, ReturnURL: returnURL, Notice: r.URL.Query().Get("notice")})
}
func (s *Server) saveGrants(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		s.redirectError(w, r, "/agents/"+id, "Invalid form")
		return
	}
	if err := s.store.SetVisibleGrants(r.Context(), id, r.Form["visible_tool_id"], r.Form["tool_id"]); err != nil {
		s.redirectError(w, r, "/agents/"+id, "Could not save access")
		return
	}
	target := r.FormValue("return_url")
	if !strings.HasPrefix(target, "/agents/"+id) {
		target = "/agents/" + id
	}
	target += map[bool]string{true: "&", false: "?"}[strings.Contains(target, "?")] + "notice=" + url.QueryEscape("Access updated for this page.")
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Server) disableAgent(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DisableAgent(r.Context(), r.PathValue("id")); err != nil {
		s.redirectError(w, r, "/agents", "Could not disable agent")
		return
	}
	http.Redirect(w, r, "/agents?notice="+url.QueryEscape("Agent disabled and all of its tokens revoked."), http.StatusSeeOther)
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	agent, err := s.store.GetAgent(r.Context(), id)
	if err != nil {
		s.redirectError(w, r, "/agents", "Agent not found")
		return
	}
	if agent.Runtime == "hermes" && agent.ConfigPath != "" {
		if err := s.importer.DisconnectProfile(agent.ConfigPath); err != nil {
			s.log.Error("disconnect Hermes profile", "error", err)
			s.redirectError(w, r, "/agents", "The Hermes configuration could not be updated")
			return
		}
	}
	if err := s.store.DeleteAgent(r.Context(), id); err != nil {
		s.redirectError(w, r, "/agents", "Could not delete agent")
		return
	}
	http.Redirect(w, r, "/agents?notice="+url.QueryEscape("Agent deleted and its Toolmux access removed."), http.StatusSeeOther)
}

func (s *Server) render(w http.ResponseWriter, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "pages.html", data); err != nil {
		s.log.Error("render page", "error", err)
	}
}
func (s *Server) fail(w http.ResponseWriter, err error) {
	s.log.Error("request failed", "error", err)
	http.Error(w, "Toolmux is temporarily unavailable.", http.StatusInternalServerError)
}
func (s *Server) redirectError(w http.ResponseWriter, r *http.Request, path, message string) {
	http.Redirect(w, r, path+map[bool]string{true: "&", false: "?"}[strings.Contains(path, "?")]+"error="+url.QueryEscape(message), http.StatusSeeOther)
}

func validHTTPURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != ""
}

func jsonOrDefault(value, fallback string) json.RawMessage {
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	return json.RawMessage(value)
}

func validJSONObject(raw json.RawMessage) bool {
	var value map[string]any
	return json.Unmarshal(raw, &value) == nil
}

func validStringMap(raw json.RawMessage) bool {
	var value map[string]string
	return json.Unmarshal(raw, &value) == nil
}

func validURLTemplate(value string) bool {
	return strings.HasPrefix(value, "/") || strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://")
}

func durationMS(value string, fallbackSeconds int) int {
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 1 || seconds > 3600 {
		seconds = fallbackSeconds
	}
	return seconds * 1000
}

func pageNumber(r *http.Request) int {
	page, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || page < 1 {
		return 1
	}
	return page
}

func newPager(r *http.Request, requested, total, pageSize int) pager {
	pages := (total + pageSize - 1) / pageSize
	if pages < 1 {
		pages = 1
	}
	page := requested
	if page > pages {
		page = pages
	}
	from := 0
	to := 0
	if total > 0 {
		from = (page-1)*pageSize + 1
		to = min(page*pageSize, total)
	}
	result := pager{Page: page, Pages: pages, Total: total, From: from, To: to, HasPrevious: page > 1, HasNext: page < pages}
	pageURL := func(value int) string {
		query := r.URL.Query()
		query.Set("page", strconv.Itoa(value))
		return r.URL.Path + "?" + query.Encode()
	}
	if result.HasPrevious {
		result.PreviousURL = pageURL(page - 1)
	}
	if result.HasNext {
		result.NextURL = pageURL(page + 1)
	}
	return result
}

func pageSlice[T any](items []T, pagination pager, pageSize int) []T {
	if len(items) == 0 {
		return items
	}
	start := (pagination.Page - 1) * pageSize
	return items[start:min(start+pageSize, len(items))]
}

func containsFold(value, query string) bool {
	return strings.Contains(strings.ToLower(value), strings.ToLower(query))
}

func (s *Server) sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path != "/mcp" {
			origin := strings.TrimRight(r.Header.Get("Origin"), "/")
			referer := r.Header.Get("Referer")
			if origin != "" && origin != s.baseURL {
				http.Error(w, "invalid origin", http.StatusForbidden)
				return
			}
			if origin == "" && referer != "" && !strings.HasPrefix(referer, s.baseURL+"/") {
				http.Error(w, "invalid origin", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; form-action 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}
