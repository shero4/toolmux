package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/shero4/toolmux/internal/discovery"
	"github.com/shero4/toolmux/internal/importer"
	"github.com/shero4/toolmux/internal/store"
)

type agentsPage struct {
	Agents []store.Agent
	Query  string
	Total  int
	Pager  pager
}

type agentForm struct {
	Name, Slug, Error string
}

type agentPage struct {
	Agent       store.Agent
	Tokens      []store.AgentToken
	Connections []store.Connection
	Tools       []store.Tool
	Filter      toolFilter
	Pager       pager
}

// toolFilter holds the catalog filters a page received.
type toolFilter struct {
	Query, Kind, ConnectionID, Granted string
}

func (f toolFilter) Active() bool {
	return f.Query != "" || f.Kind != "" || f.ConnectionID != "" || f.Granted != ""
}

type discoverPage struct {
	Hermes   []hermesRow
	Skipped  []importer.Skipped
	GWS      []importer.GWSConnection
	OpenClaw []candidateRow
	Found    int
}

type hermesRow struct {
	Profile importer.Profile
	Agent   *store.Agent
}

type candidateRow struct {
	Candidate discovery.Candidate
	Agent     *store.Agent
}

type setupGuide struct {
	Title, Destination, Config, Note string
}

func readToolFilter(r *http.Request) toolFilter {
	return toolFilter{Query: strings.TrimSpace(r.URL.Query().Get("q")), Kind: r.URL.Query().Get("kind"), ConnectionID: r.URL.Query().Get("connection"), Granted: r.URL.Query().Get("granted")}
}

// searchToolsPage runs a paginated catalog search, clamping the page to the
// available range.
func (s *Server) searchToolsPage(r *http.Request, filter store.ToolFilter, pageSize int) ([]store.Tool, pager, error) {
	requested := pageNumber(r)
	filter.Limit = pageSize
	filter.Offset = (requested - 1) * pageSize
	tools, total, err := s.store.SearchTools(r.Context(), filter)
	if err != nil {
		return nil, pager{}, err
	}
	pagination := newPager(r, requested, total, pageSize)
	if pagination.Page != requested {
		filter.Offset = (pagination.Page - 1) * pageSize
		if tools, _, err = s.store.SearchTools(r.Context(), filter); err != nil {
			return nil, pager{}, err
		}
	}
	return tools, pagination, nil
}

func (s *Server) agents(w http.ResponseWriter, r *http.Request) {
	const pageSize = 25
	all, err := s.store.ListAgents(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	items := all
	if query != "" {
		items = nil
		for _, agent := range all {
			if containsFold(agent.Name, query) || containsFold(agent.Slug, query) || containsFold(agent.Runtime, query) || containsFold(agent.Profile, query) || containsFold(agent.Environment, query) {
				items = append(items, agent)
			}
		}
	}
	pagination := newPager(r, pageNumber(r), len(items), pageSize)
	s.render(w, r, http.StatusOK, "agents", "agents", "Agents", agentsPage{Agents: pageSlice(items, pagination, pageSize), Query: query, Total: len(all), Pager: pagination})
}

func (s *Server) newAgent(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "agent_new", "agents", "New agent", agentForm{})
}

func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.render(w, r, http.StatusBadRequest, "agent_new", "agents", "New agent", agentForm{Error: "The form could not be read."})
		return
	}
	form := agentForm{Name: strings.TrimSpace(r.FormValue("name")), Slug: store.Slug(r.FormValue("slug"))}
	if form.Slug == "" {
		form.Slug = store.Slug(form.Name)
	}
	if form.Name == "" || form.Slug == "" {
		form.Error = "Enter a name for the agent."
		s.render(w, r, http.StatusBadRequest, "agent_new", "agents", "New agent", form)
		return
	}
	agent, token, err := s.store.CreateAgent(r.Context(), form.Name, form.Slug)
	if err != nil {
		s.log.Error("create agent", "error", err)
		form.Error = "The agent could not be created. Its slug may already be in use."
		s.render(w, r, http.StatusBadRequest, "agent_new", "agents", "New agent", form)
		return
	}
	s.redirect(w, r, "/agents/"+agent.ID, s.tokenFlash(agent, token, "Agent created."))
}

// tokenFlash builds the one-time reveal shown after a token is issued.
func (s *Server) tokenFlash(agent store.Agent, token, message string) flash {
	guide := s.setupGuide(agent, token)
	return flash{Kind: "ok", Message: message + " Copy the token now; it will not be shown again.", Token: token, Setup: &guide}
}

func (s *Server) setupGuide(agent store.Agent, token string) setupGuide {
	endpoint := s.baseURL + "/mcp"
	switch agent.Runtime {
	case "hermes":
		config := "mcp_servers:\n  toolmux:\n    url: \"" + endpoint + "\"\n    headers:\n      Authorization: \"Bearer " + token + "\"\n    enabled: true\n    supports_parallel_tool_calls: true"
		return setupGuide{Title: "Hermes configuration", Destination: agent.ConfigPath, Config: config, Note: "Merge this server into mcp_servers, then restart that Hermes profile."}
	case "openclaw":
		serverName := "toolmux-" + agent.Slug
		payload, _ := json.Marshal(map[string]any{"url": endpoint, "transport": "streamable-http", "headers": map[string]string{"Authorization": "Bearer " + token}})
		command := "openclaw mcp set " + serverName + " '" + string(payload) + "'"
		return setupGuide{Title: "OpenClaw configuration", Destination: agent.ConfigPath, Config: command, Note: "Run this in the detected environment, then verify with: openclaw mcp probe " + serverName}
	default:
		return setupGuide{Title: "MCP client configuration", Config: "URL: " + endpoint + "\nAuthorization: Bearer " + token, Note: "Use the Streamable HTTP transport. The client discovers granted tools through tools/list."}
	}
}

func (s *Server) discover(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	candidates := s.discovery.Scan(ctx)
	inventory := s.importer.Inventory(ctx, candidates)
	agents, err := s.store.ListAgents(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	bySource := make(map[string]store.Agent, len(agents))
	for _, agent := range agents {
		if agent.SourceKey != "" {
			bySource[agent.SourceKey] = agent
		}
	}
	page := discoverPage{Skipped: inventory.Skipped, GWS: inventory.GWS, Found: len(candidates)}
	for _, profile := range inventory.Profiles {
		row := hermesRow{Profile: profile}
		if agent, ok := bySource[profile.Candidate.ID]; ok {
			row.Agent = &agent
		}
		page.Hermes = append(page.Hermes, row)
	}
	for _, candidate := range candidates {
		if candidate.Runtime == "hermes" {
			continue
		}
		row := candidateRow{Candidate: candidate}
		if agent, ok := bySource[candidate.ID]; ok {
			row.Agent = &agent
		}
		page.OpenClaw = append(page.OpenClaw, row)
	}
	s.render(w, r, http.StatusOK, "discover", "agents", "Discover agents", page)
}

func (s *Server) importCandidate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirect(w, r, "/agents/discover", errorFlash("The form could not be read."))
		return
	}
	candidate, ok := s.discovery.Find(r.Context(), r.FormValue("candidate_id"))
	if !ok {
		s.redirect(w, r, "/agents/discover", errorFlash("That installation is no longer available. Scan again."))
		return
	}
	if candidate.Runtime == "hermes" {
		inventory := s.importer.Inventory(r.Context(), []discovery.Candidate{candidate})
		summary, err := s.importer.ImportProfile(r.Context(), inventory, candidate.ID)
		if err != nil {
			s.log.Error("import Hermes profile", "profile", candidate.Name, "error", err)
			s.redirect(w, r, "/agents/discover", errorFlash("The Hermes profile could not be imported: "+err.Error()))
			return
		}
		s.checker.CheckMany(summary.ConnectionIDs)
		target := "/agents"
		if len(summary.AgentIDs) == 1 {
			target = "/agents/" + summary.AgentIDs[0]
		}
		s.redirect(w, r, target, importFlash(summary))
		return
	}
	agent, token, err := s.store.CreateDiscoveredAgent(r.Context(), candidate.Name, store.Slug(candidate.Identity()), candidate.ID, candidate.Runtime, candidate.Profile, candidate.Environment, candidate.ConfigPath)
	if err != nil {
		s.log.Error("import discovered agent", "error", err)
		s.redirect(w, r, "/agents/discover", errorFlash("The agent could not be added. It may already exist."))
		return
	}
	s.redirect(w, r, "/agents/"+agent.ID, s.tokenFlash(agent, token, "Agent added."))
}

func (s *Server) importHermes(w http.ResponseWriter, r *http.Request) {
	inventory := s.importer.Scan(r.Context())
	if len(inventory.Profiles) == 0 {
		s.redirect(w, r, "/agents/discover", errorFlash("No importable Hermes profiles were found."))
		return
	}
	summary, err := s.importer.ImportAll(r.Context(), inventory)
	if err != nil {
		s.log.Error("import Hermes configuration", "error", err)
		s.redirect(w, r, "/agents/discover", errorFlash("The Hermes import stopped before it completed: "+err.Error()))
		return
	}
	s.checker.CheckMany(summary.ConnectionIDs)
	s.redirect(w, r, "/agents", importFlash(summary))
}

func importFlash(summary importer.Summary) flash {
	message := fmt.Sprintf("Imported %d new %s. %d %s created, %d updated.", summary.AgentsCreated, plural(summary.AgentsCreated, "agent", "agents"), summary.ConnectionsCreated, plural(summary.ConnectionsCreated, "connection", "connections"), summary.ConnectionsUpdated)
	if summary.ConfigsUpdated > 0 {
		message += fmt.Sprintf(" %d Hermes %s now %s at Toolmux.", summary.ConfigsUpdated, plural(summary.ConfigsUpdated, "profile", "profiles"), plural(summary.ConfigsUpdated, "points", "point"))
	}
	if len(summary.ConnectionIDs) > 0 {
		message += " Connection checks are running in the background; refresh in a moment."
	}
	fl := flash{Kind: "ok", Message: message, Details: summary.Warnings}
	if len(summary.Warnings) > 0 {
		fl.Kind = "info"
	}
	return fl
}

func plural(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}

func (s *Server) agent(w http.ResponseWriter, r *http.Request) {
	const pageSize = 50
	ctx := r.Context()
	agent, err := s.store.GetAgent(ctx, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r, "Agent")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	connections, err := s.store.ConnectionsForAgent(ctx, agent.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	tokens, err := s.store.ListAgentTokens(ctx, agent.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	filter := readToolFilter(r)
	tools, pagination, err := s.searchToolsPage(r, store.ToolFilter{Query: filter.Query, Kind: filter.Kind, ConnectionID: filter.ConnectionID, AgentID: agent.ID, Granted: filter.Granted}, pageSize)
	if err != nil {
		s.fail(w, err)
		return
	}
	page := agentPage{Agent: agent, Tokens: tokens, Connections: connections, Tools: tools, Filter: filter, Pager: pagination}
	s.render(w, r, http.StatusOK, "agent", "agents", agent.Name, page)
}

func (s *Server) issueAgentToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	agent, err := s.store.GetAgent(r.Context(), id)
	if err != nil {
		s.notFound(w, r, "Agent")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.redirect(w, r, "/agents/"+id, errorFlash("The form could not be read."))
		return
	}
	label := strings.TrimSpace(r.FormValue("label"))
	if label == "" {
		label = "default"
	}
	if len(label) > 80 {
		s.redirect(w, r, "/agents/"+id, errorFlash("Token labels are limited to 80 characters."))
		return
	}
	if agent.Status != "active" {
		s.redirect(w, r, "/agents/"+id, errorFlash("Enable the agent before issuing a token."))
		return
	}
	token, err := s.store.IssueAgentToken(r.Context(), id, label)
	if err != nil {
		s.log.Error("issue token", "error", err)
		s.redirect(w, r, "/agents/"+id, errorFlash("The token could not be issued."))
		return
	}
	_ = s.store.LogOperation(r.Context(), "token", id, agent.Name, "issued", "Agent token issued.")
	s.redirect(w, r, "/agents/"+id, s.tokenFlash(agent, token, "Token issued."))
}

func (s *Server) revokeAgentToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.RevokeAgentTokenByID(r.Context(), id, r.PathValue("tokenID")); err != nil {
		s.redirect(w, r, "/agents/"+id, errorFlash("The token could not be revoked."))
		return
	}
	_ = s.store.LogOperation(r.Context(), "token", id, "Agent token", "revoked", "Agent token revoked.")
	s.redirect(w, r, "/agents/"+id, okFlash("Token revoked."))
}

// saveGrants updates the grants for the tools listed in the form. The page
// script posts single rows with X-Toolmux-Async; a plain form submit posts
// every visible row and redirects back.
func (s *Server) saveGrants(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	async := r.Header.Get("X-Toolmux-Async") == "true"
	var formErr error
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		formErr = r.ParseMultipartForm(2 << 20)
		if r.MultipartForm != nil {
			defer r.MultipartForm.RemoveAll()
		}
	} else {
		formErr = r.ParseForm()
	}
	if formErr != nil || len(r.Form["visible_tool_id"]) == 0 {
		if async {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		s.redirect(w, r, "/agents/"+id, errorFlash("The form could not be read."))
		return
	}
	if err := s.store.SetVisibleGrants(r.Context(), id, r.Form["visible_tool_id"], r.Form["tool_id"]); err != nil {
		s.log.Error("save grants", "agent", id, "error", err)
		if async {
			http.Error(w, "could not save access", http.StatusInternalServerError)
			return
		}
		s.redirect(w, r, "/agents/"+id, errorFlash("Access could not be saved."))
		return
	}
	if async {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	target := returnTarget(r.FormValue("return_url"), "/agents/"+id)
	if !strings.HasPrefix(target, "/agents/"+id) {
		target = "/agents/" + id
	}
	s.redirect(w, r, target, okFlash("Access updated for the listed tools."))
}

func (s *Server) setAgentConnection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		s.redirect(w, r, "/agents/"+id, errorFlash("The form could not be read."))
		return
	}
	enabled := r.FormValue("enabled") == "true"
	if err := s.store.SetAgentConnection(r.Context(), id, r.PathValue("connectionID"), enabled); err != nil {
		s.log.Error("set agent connection", "error", err)
		s.redirect(w, r, "/agents/"+id, errorFlash("Connection access could not be updated."))
		return
	}
	message := "Connection removed. Its tools are no longer granted to this agent."
	if enabled {
		message = "Connection assigned. Its current and future tools are granted to this agent."
	}
	s.redirect(w, r, "/agents/"+id, okFlash(message))
}

func (s *Server) disableAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DisableAgent(r.Context(), id); err != nil {
		s.redirect(w, r, "/agents/"+id, errorFlash("The agent could not be disabled."))
		return
	}
	s.redirect(w, r, "/agents/"+id, okFlash("Agent disabled. All of its tokens were revoked."))
}

func (s *Server) enableAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.EnableAgent(r.Context(), id); err != nil {
		s.redirect(w, r, "/agents/"+id, errorFlash("The agent could not be enabled."))
		return
	}
	s.redirect(w, r, "/agents/"+id, okFlash("Agent enabled. Issue a new token to reconnect it."))
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	agent, err := s.store.GetAgent(r.Context(), id)
	if err != nil {
		s.notFound(w, r, "Agent")
		return
	}
	if agent.Runtime == "hermes" && agent.ConfigPath != "" {
		if err := s.importer.DisconnectProfile(agent.ConfigPath); err != nil {
			s.log.Error("disconnect Hermes profile", "error", err)
			s.redirect(w, r, "/agents/"+id, errorFlash("The Hermes configuration could not be updated, so the agent was kept: "+err.Error()))
			return
		}
	}
	if err := s.store.DeleteAgent(r.Context(), id); err != nil {
		s.redirect(w, r, "/agents/"+id, errorFlash("The agent could not be deleted."))
		return
	}
	s.redirect(w, r, "/agents", okFlash("Agent "+agent.Name+" deleted."))
}
