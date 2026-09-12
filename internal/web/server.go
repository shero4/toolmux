// Package web serves the administration interface and mounts the agent and
// control endpoints. Pages are server-rendered Go templates with one CSS file
// and a small script for progressive enhancement.
package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"

	"github.com/shero4/toolmux/internal/checker"
	"github.com/shero4/toolmux/internal/discovery"
	"github.com/shero4/toolmux/internal/gateway"
	"github.com/shero4/toolmux/internal/importer"
	"github.com/shero4/toolmux/internal/oauth"
	"github.com/shero4/toolmux/internal/operations"
	"github.com/shero4/toolmux/internal/store"
)

//go:embed templates static
var assets embed.FS

type Server struct {
	store        *store.Store
	checker      *checker.Checker
	oauth        *oauth.Manager
	discovery    *discovery.Scanner
	importer     *importer.Manager
	baseURL      string
	log          *slog.Logger
	pages        map[string]*template.Template
	flashes      *flashStore
	assetVersion string
	modelClient  *http.Client
	operations   *operations.Monitor
	rotationMu   sync.Mutex
}

// view is the data every page receives. Page-specific data lives in Data.
type view struct {
	User               store.User
	CanManage, IsAdmin bool
	Nav, Title         string
	BaseURL            string
	Endpoint           string
	ReturnURL          string // current page without the flash id, for return_url fields
	AssetVersion       string
	Flash              *flash
	Data               any
}

func New(store *store.Store, check *checker.Checker, oauth *oauth.Manager, scanner *discovery.Scanner, hermes *importer.Manager, baseURL string, log *slog.Logger) (*Server, error) {
	s := &Server{store: store, checker: check, oauth: oauth, discovery: scanner, importer: hermes, baseURL: strings.TrimRight(baseURL, "/"), log: log, pages: make(map[string]*template.Template), flashes: newFlashStore()}
	version, err := assetVersion()
	if err != nil {
		return nil, err
	}
	s.assetVersion = version
	s.operations = operations.New(store, check)
	s.modelClient = gateway.Client()
	base, err := template.New("").Funcs(functions()).ParseFS(assets, "templates/layout.html", "templates/partials.html")
	if err != nil {
		return nil, err
	}
	pageFiles, err := fs.Glob(assets, "templates/pages/*.html")
	if err != nil {
		return nil, err
	}
	for _, file := range pageFiles {
		page, err := template.Must(base.Clone()).ParseFS(assets, file)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", file, err)
		}
		s.pages[strings.TrimSuffix(path.Base(file), ".html")] = page
	}
	return s, nil
}

// assetVersion fingerprints the static assets so browsers can cache them
// indefinitely and still pick up new builds.
func assetVersion() (string, error) {
	digest := sha256.New()
	for _, name := range []string{"static/app.css", "static/app.js"} {
		data, err := assets.ReadFile(name)
		if err != nil {
			return "", err
		}
		digest.Write(data)
	}
	return hex.EncodeToString(digest.Sum(nil))[:12], nil
}

func (s *Server) Handler(mcpHandler, adminHandler http.Handler, inference ...http.Handler) http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheForever(http.FileServer(http.FS(static)))))
	mux.Handle("/mcp", mcpHandler)
	mux.Handle("/admin/mcp", adminHandler)
	if len(inference) > 0 {
		mux.Handle("/v1/", inference[0])
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("GET /{$}", s.overview)
	mux.HandleFunc("GET /setup", s.authForm)
	mux.HandleFunc("POST /setup", s.submitAuth)
	mux.HandleFunc("GET /login", s.authForm)
	mux.HandleFunc("POST /login", s.submitAuth)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /settings", s.settings)
	mux.HandleFunc("POST /settings/password", s.changePassword)
	mux.HandleFunc("POST /settings/monitoring", s.saveMonitoring)
	mux.HandleFunc("POST /settings/webhook/test", s.testWebhook)
	mux.HandleFunc("GET /operations", s.operationLogs)
	mux.HandleFunc("POST /operations/check", s.checkNow)
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		s.render(w, r, 200, "docs", "docs", "Documentation", nil)
	})
	mux.HandleFunc("GET /auth/codex", s.codexPage)
	mux.HandleFunc("GET /auth/codex/status", s.codexStatus)
	mux.HandleFunc("POST /auth/codex", s.codexLogin)
	mux.HandleFunc("GET /agents/{id}/tokens/{tokenID}/rotate", s.rotatePreview)
	mux.HandleFunc("POST /agents/{id}/tokens/{tokenID}/rotate", s.rotateToken)
	mux.HandleFunc("GET /users", s.users)
	mux.HandleFunc("POST /users", s.createUser)
	mux.HandleFunc("POST /users/{id}/access", s.setUserAccess)
	mux.HandleFunc("GET /providers", s.providers)
	mux.HandleFunc("GET /providers/guide", func(w http.ResponseWriter, r *http.Request) {
		s.render(w, r, 200, "provider-guide", "providers", "Provider setup", nil)
	})
	mux.HandleFunc("GET /providers/new", s.provider)
	mux.HandleFunc("POST /providers", s.saveProvider)
	mux.HandleFunc("GET /providers/{id}", s.provider)
	mux.HandleFunc("POST /providers/{id}", s.saveProvider)
	mux.HandleFunc("POST /providers/{id}/discover", s.discoverModels)

	mux.HandleFunc("GET /agents", s.agents)
	mux.HandleFunc("GET /agents/new", s.newAgent)
	mux.HandleFunc("POST /agents", s.createAgent)
	mux.HandleFunc("GET /agents/discover", s.discover)
	mux.HandleFunc("POST /agents/import", s.importCandidate)
	mux.HandleFunc("POST /agents/import/hermes", s.importHermes)
	mux.HandleFunc("GET /agents/{id}", s.agent)
	mux.HandleFunc("POST /agents/{id}/tokens", s.issueAgentToken)
	mux.HandleFunc("POST /agents/{id}/tokens/{tokenID}/revoke", s.revokeAgentToken)
	mux.HandleFunc("POST /agents/{id}/grants", s.saveGrants)
	mux.HandleFunc("POST /agents/{id}/connections/{connectionID}", s.setAgentConnection)
	mux.HandleFunc("POST /agents/{id}/disable", s.disableAgent)
	mux.HandleFunc("POST /agents/{id}/enable", s.enableAgent)
	mux.HandleFunc("POST /agents/{id}/delete", s.deleteAgent)

	mux.HandleFunc("GET /connections", s.connections)
	mux.HandleFunc("GET /connections/new", s.newConnection)
	mux.HandleFunc("POST /connections", s.createConnection)
	mux.HandleFunc("GET /connections/{id}", s.connection)
	mux.HandleFunc("POST /connections/{id}/check", s.checkConnection)
	mux.HandleFunc("POST /connections/{id}/credential", s.updateConnectionCredential)
	mux.HandleFunc("POST /connections/{id}/authorize", s.authorizeConnection)
	mux.HandleFunc("POST /connections/{id}/enable", s.enableConnection)
	mux.HandleFunc("POST /connections/{id}/disable", s.disableConnection)
	mux.HandleFunc("POST /connections/{id}/delete", s.deleteConnection)
	mux.HandleFunc("GET /oauth/callback", s.oauthCallback)

	mux.HandleFunc("GET /tools", s.tools)
	mux.HandleFunc("GET /tools/new", s.newTool)
	mux.HandleFunc("POST /tools", s.createTool)
	mux.HandleFunc("GET /tools/{id}", s.tool)
	mux.HandleFunc("POST /tools/{id}/delete", s.deleteTool)

	mux.HandleFunc("GET /activity", s.activity)

	return securityHeaders(sameOrigin(s.authenticated(mux)))
}

// render executes a page into a buffer first so a template failure produces a
// clean error instead of a half-written response.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page, nav, title string, data any) {
	tmpl, ok := s.pages[page]
	if !ok {
		s.fail(w, fmt.Errorf("unknown page %q", page))
		return
	}
	v := view{Nav: nav, Title: title, BaseURL: s.baseURL, Endpoint: s.baseURL + "/mcp", ReturnURL: currentPage(r), AssetVersion: s.assetVersion, Flash: s.flashes.take(r.URL.Query().Get("f")), Data: data}
	v.User = currentUser(r)
	v.CanManage = v.User.CanManage()
	v.IsAdmin = v.User.IsAdmin()
	if !v.CanManage && v.Flash != nil {
		v.Flash.Token = ""
		v.Flash.Setup = nil
	}
	var buffer bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buffer, "layout", v); err != nil {
		s.fail(w, fmt.Errorf("render %s: %w", page, err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buffer.WriteTo(w)
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request, what string) {
	s.render(w, r, http.StatusNotFound, "error", "", "Not found", errorPage{Title: what + " not found", Message: "It may have been deleted. Use the navigation to find what you need."})
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.log.Error("request failed", "error", err)
	http.Error(w, "Toolmux is temporarily unavailable.", http.StatusInternalServerError)
}

type errorPage struct {
	Title, Message string
}

// redirect stores a one-time message and sends the browser to target.
func (s *Server) redirect(w http.ResponseWriter, r *http.Request, target string, fl flash) {
	id := s.flashes.put(fl)
	separator := "?"
	if strings.Contains(target, "?") {
		separator = "&"
	}
	http.Redirect(w, r, target+separator+"f="+url.QueryEscape(id), http.StatusSeeOther)
}

// currentPage is the request path and query without the one-time flash id.
func currentPage(r *http.Request) string {
	query := r.URL.Query()
	query.Del("f")
	if encoded := query.Encode(); encoded != "" {
		return r.URL.Path + "?" + encoded
	}
	return r.URL.Path
}

// returnTarget accepts a same-site path supplied by a form so an action can
// send the browser back where it started.
func returnTarget(value, fallback string) string {
	if strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "//") {
		return value
	}
	return fallback
}

// sameOrigin rejects browser-originated POSTs from other sites. Requests
// without browser headers (agents, scripts) pass through; the MCP endpoints
// authenticate with bearer tokens instead.
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path != "/mcp" && r.URL.Path != "/admin/mcp" {
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				http.Error(w, "invalid origin", http.StatusForbidden)
				return
			}
			source := r.Header.Get("Origin")
			if source == "" {
				source = r.Header.Get("Referer")
			}
			if source != "" && !sameHost(source, r.Host) {
				http.Error(w, "invalid origin", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func sameHost(rawURL, host string) bool {
	parsed, err := url.Parse(rawURL)
	return err == nil && strings.EqualFold(parsed.Host, host)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		next.ServeHTTP(w, r)
	})
}

func cacheForever(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		next.ServeHTTP(w, r)
	})
}
