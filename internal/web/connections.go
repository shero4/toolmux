package web

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/shero4/toolmux/internal/store"
)

type connectionsPage struct {
	Connections         []store.Connection
	Query, Kind, Status string
	Total               int
	Pager               pager
}

func (p connectionsPage) Filtered() bool { return p.Query != "" || p.Kind != "" || p.Status != "" }

// connectionForm carries the new-connection form so validation errors can
// re-render it with the operator's input intact.
type connectionForm struct {
	ConnectorName, ConnectorSlug, Name, Slug string
	Kind, EndpointURL, HealthPath            string
	AuthMethod, AuthName                     string
	AuthorizationURL, TokenURL, ClientID     string
	Scopes, TokenAuthMethod                  string
	Error                                    string
}

type connectionPage struct {
	Connection    store.Connection
	Stdio         *store.MCPStdioSpec
	OAuth         *oauthSummary
	HasCredential bool
	Tools         []store.Tool
	Pager         pager
	Agents        []store.AgentAccess
	Checks        []store.CheckRecord
	Declarative   bool
}

// oauthSummary describes OAuth state without exposing any secret.
type oauthSummary struct {
	ClientID, AuthorizationURL, TokenURL string
	Registered, HasAccess, HasRefresh    bool
	ExpiresAt                            *time.Time
}

func (s *Server) connections(w http.ResponseWriter, r *http.Request) {
	const pageSize = 25
	all, err := s.store.ListConnections(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	page := connectionsPage{Query: strings.TrimSpace(r.URL.Query().Get("q")), Kind: r.URL.Query().Get("kind"), Status: r.URL.Query().Get("status"), Total: len(all)}
	items := all
	if page.Filtered() {
		items = nil
		for _, item := range all {
			matches := page.Query == "" || containsFold(item.Name, page.Query) || containsFold(item.ConnectorName, page.Query) || containsFold(item.Slug, page.Query) || containsFold(item.EndpointURL, page.Query)
			if matches && (page.Kind == "" || item.Kind == page.Kind) && (page.Status == "" || item.Status == page.Status) {
				items = append(items, item)
			}
		}
	}
	page.Pager = newPager(r, pageNumber(r), len(items), pageSize)
	page.Connections = pageSlice(items, page.Pager, pageSize)
	s.render(w, r, http.StatusOK, "connections", "connections", "Connections", page)
}

func (s *Server) newConnection(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "connection_new", "connections", "New connection", connectionForm{Kind: "mcp_http", AuthMethod: "none", TokenAuthMethod: "client_secret_basic"})
}

func (s *Server) createConnection(w http.ResponseWriter, r *http.Request) {
	reject := func(form connectionForm, message string) {
		form.Error = message
		s.render(w, r, http.StatusBadRequest, "connection_new", "connections", "New connection", form)
	}
	if err := r.ParseForm(); err != nil {
		reject(connectionForm{}, "The form could not be read.")
		return
	}
	value := func(name string) string { return strings.TrimSpace(r.FormValue(name)) }
	form := connectionForm{
		ConnectorName: value("connector_name"), ConnectorSlug: store.Slug(value("connector_slug")),
		Name: value("name"), Slug: store.Slug(value("connection_slug")),
		Kind: value("kind"), EndpointURL: value("endpoint_url"), HealthPath: value("health_path"),
		AuthMethod: value("auth_method"), AuthName: value("auth_name"),
		AuthorizationURL: value("authorization_url"), TokenURL: value("token_url"), ClientID: value("client_id"),
		Scopes: value("scopes"), TokenAuthMethod: value("token_auth_method"),
	}
	secret := value("secret")
	if form.ConnectorSlug == "" {
		form.ConnectorSlug = store.Slug(form.ConnectorName)
	}
	if form.Slug == "" {
		form.Slug = store.Slug(form.Name)
	}
	if form.TokenAuthMethod == "" {
		form.TokenAuthMethod = "client_secret_basic"
	}
	switch {
	case form.Kind != "mcp_http" && form.Kind != "http_api" && form.Kind != "command":
		reject(form, "Choose a connection type.")
	case form.ConnectorName == "" || form.ConnectorSlug == "" || form.Name == "" || form.Slug == "":
		reject(form, "Enter a connector name and a connection name.")
	case form.Kind != "command" && !validHTTPURL(form.EndpointURL):
		reject(form, "Enter a valid http or https endpoint.")
	case form.HealthPath != "" && !strings.HasPrefix(form.HealthPath, "/"):
		reject(form, "The health path must start with /.")
	case form.AuthMethod != "none" && form.AuthMethod != "bearer" && form.AuthMethod != "header" && form.AuthMethod != "oauth2":
		reject(form, "Choose an authorization method.")
	case (form.AuthMethod == "bearer" || form.AuthMethod == "header") && secret == "":
		reject(form, "Enter the credential for this connection.")
	case form.AuthMethod == "header" && form.AuthName == "":
		reject(form, "Enter the header name that carries the API key.")
	case form.AuthMethod == "oauth2" && (!validHTTPURL(form.AuthorizationURL) || !validHTTPURL(form.TokenURL) || form.ClientID == ""):
		reject(form, "OAuth needs an authorization URL, a token URL, and a client ID.")
	case form.AuthMethod == "oauth2" && form.TokenAuthMethod != "client_secret_basic" && form.TokenAuthMethod != "client_secret_post":
		reject(form, "Choose how the client secret is sent to the token endpoint.")
	default:
		var oauthConfig *store.OAuthConfig
		if form.AuthMethod == "oauth2" {
			oauthConfig = &store.OAuthConfig{AuthorizationURL: form.AuthorizationURL, TokenURL: form.TokenURL, ClientID: form.ClientID, Scopes: form.Scopes, TokenAuthMethod: form.TokenAuthMethod}
		}
		id, err := s.store.CreateConnection(r.Context(), form.Kind, form.ConnectorName, form.ConnectorSlug, form.EndpointURL, form.HealthPath, form.Name, form.Slug, form.AuthMethod, form.AuthName, secret, oauthConfig)
		if err != nil {
			s.log.Error("create connection", "error", err)
			reject(form, "The connection could not be created. Its slug may already be in use.")
			return
		}
		s.runCheck(r, id)
		message := "Connection created and checked."
		if form.AuthMethod == "oauth2" {
			message = "Connection created. Authorize it to obtain tokens."
		}
		s.redirect(w, r, "/connections/"+id, okFlash(message))
	}
}

func (s *Server) connection(w http.ResponseWriter, r *http.Request) {
	const pageSize = 25
	ctx := r.Context()
	connection, credential, err := s.store.GetConnection(ctx, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r, "Connection")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	page := connectionPage{Connection: connection, HasCredential: !credential.Empty(), Declarative: connection.Kind == "http_api" || connection.Kind == "command"}
	if connection.Kind == "mcp_stdio" {
		if spec, err := s.store.GetMCPStdioSpec(ctx, connection.ID); err == nil {
			page.Stdio = &spec
		}
	}
	if connection.AuthMethod == "oauth2" {
		summary := oauthSummary{HasAccess: credential.AccessToken != "", HasRefresh: credential.RefreshToken != "", ExpiresAt: credential.ExpiresAt}
		if config, err := s.store.GetOAuthConfig(ctx, connection.ID); err == nil {
			summary.ClientID, summary.AuthorizationURL, summary.TokenURL = config.ClientID, config.AuthorizationURL, config.TokenURL
			summary.Registered = config.RedirectURI == s.baseURL+"/oauth/callback"
		}
		page.OAuth = &summary
	}
	if page.Tools, page.Pager, err = s.searchToolsPage(r, store.ToolFilter{ConnectionID: connection.ID, Query: strings.TrimSpace(r.URL.Query().Get("q"))}, pageSize); err != nil {
		s.fail(w, err)
		return
	}
	if page.Agents, err = s.store.AgentsForConnection(ctx, connection.ID); err != nil {
		s.fail(w, err)
		return
	}
	if page.Checks, err = s.store.ListChecks(ctx, connection.ID, 10); err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, http.StatusOK, "connection", "connections", connection.Name, page)
}

func (s *Server) runCheck(r *http.Request, id string) {
	if err := s.checker.Check(r.Context(), id); err != nil {
		s.log.Error("check connection", "connection", id, "error", err)
	}
}

// checkFlash summarizes the connection's state after a check ran.
func (s *Server) checkFlash(r *http.Request, id string) flash {
	connection, err := s.store.GetConnectionSummary(r.Context(), id)
	if err != nil {
		return errorFlash("The check could not be completed.")
	}
	message := "Check finished: " + statusLabel(connection.Status) + "."
	if connection.LastError != "" {
		message += " " + connection.LastError
	}
	if connection.Status == "connected" {
		return okFlash(message)
	}
	return errorFlash(message)
}

func (s *Server) checkConnection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.GetConnectionSummary(r.Context(), id); err != nil {
		s.notFound(w, r, "Connection")
		return
	}
	_ = r.ParseForm()
	s.runCheck(r, id)
	s.redirect(w, r, returnTarget(r.FormValue("return_url"), "/connections/"+id), s.checkFlash(r, id))
}

func (s *Server) updateConnectionCredential(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	connection, _, err := s.store.GetConnection(r.Context(), id)
	if err != nil {
		s.notFound(w, r, "Connection")
		return
	}
	if connection.AuthMethod != "bearer" && connection.AuthMethod != "header" {
		s.redirect(w, r, "/connections/"+id, errorFlash("This connection does not use a replaceable credential."))
		return
	}
	if err := r.ParseForm(); err != nil || strings.TrimSpace(r.FormValue("secret")) == "" {
		s.redirect(w, r, "/connections/"+id, errorFlash("Enter the new credential."))
		return
	}
	if err := s.store.SaveCredential(r.Context(), id, store.Credential{BearerToken: strings.TrimSpace(r.FormValue("secret"))}); err != nil {
		s.log.Error("save credential", "error", err)
		s.redirect(w, r, "/connections/"+id, errorFlash("The credential could not be saved."))
		return
	}
	s.runCheck(r, id)
	s.redirect(w, r, "/connections/"+id, s.checkFlash(r, id))
}

func (s *Server) authorizeConnection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	target, err := s.oauth.Start(r.Context(), id)
	if err != nil {
		s.log.Error("start OAuth authorization", "connection", id, "error", err)
		s.redirect(w, r, "/connections/"+id, errorFlash("Authorization could not be started: "+err.Error()))
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Server) oauthCallback(w http.ResponseWriter, r *http.Request) {
	if providerError := r.URL.Query().Get("error"); providerError != "" {
		s.redirect(w, r, "/connections", errorFlash("The provider did not complete authorization: "+providerError))
		return
	}
	stateValue, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	if stateValue == "" || code == "" {
		s.redirect(w, r, "/connections", errorFlash("The OAuth callback was incomplete."))
		return
	}
	connectionID, err := s.oauth.Complete(r.Context(), stateValue, code)
	if err != nil {
		s.log.Error("complete OAuth authorization", "error", err)
		s.redirect(w, r, "/connections", errorFlash("The OAuth token exchange failed. Start the authorization again."))
		return
	}
	s.runCheck(r, connectionID)
	s.redirect(w, r, "/connections/"+connectionID, s.checkFlash(r, connectionID))
}

func (s *Server) enableConnection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.SetConnectionEnabled(r.Context(), id, true); err != nil {
		s.redirect(w, r, "/connections/"+id, errorFlash("The connection could not be enabled."))
		return
	}
	s.runCheck(r, id)
	s.redirect(w, r, "/connections/"+id, s.checkFlash(r, id))
}

func (s *Server) disableConnection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.SetConnectionEnabled(r.Context(), id, false); err != nil {
		s.redirect(w, r, "/connections/"+id, errorFlash("The connection could not be disabled."))
		return
	}
	s.redirect(w, r, "/connections/"+id, okFlash("Connection disabled. Agents can no longer list or call its tools; grants are kept."))
}

func (s *Server) deleteConnection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	connection, err := s.store.GetConnectionSummary(r.Context(), id)
	if err != nil {
		s.notFound(w, r, "Connection")
		return
	}
	if err := s.store.DeleteConnection(r.Context(), id); err != nil {
		s.log.Error("delete connection", "error", err)
		s.redirect(w, r, "/connections/"+id, errorFlash("The connection could not be deleted."))
		return
	}
	s.redirect(w, r, "/connections", okFlash("Connection "+connection.Name+" deleted, along with its tools, grants, and credentials."))
}
