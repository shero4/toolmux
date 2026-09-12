package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/shero4/toolmux/internal/gateway"
	"github.com/shero4/toolmux/internal/store"
)

type providersPage struct{ Providers []store.ModelProvider }
type providerPage struct {
	Provider store.ModelProvider
	Models   []string
	Error    string
	New      bool
}

func (s *Server) providers(w http.ResponseWriter, r *http.Request) {
	providers, err := s.store.ListModelProviders(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, 200, "providers", "providers", "Model providers", providersPage{providers})
}

func (s *Server) provider(w http.ResponseWriter, r *http.Request) {
	page := providerPage{New: r.PathValue("id") == "", Provider: store.ModelProvider{Adapter: "openai", Enabled: true, TimeoutSeconds: 300}}
	if !page.New {
		var err error
		page.Provider, err = s.store.GetModelProvider(r.Context(), r.PathValue("id"))
		if errors.Is(err, store.ErrNotFound) {
			s.notFound(w, r, "Provider")
			return
		}
		if err != nil {
			s.fail(w, err)
			return
		}
		page.Models, err = s.store.ProviderModels(r.Context(), page.Provider.ID)
		if err != nil {
			s.fail(w, err)
			return
		}
	}
	title := page.Provider.Name
	if page.New {
		title = "New model provider"
	}
	s.render(w, r, 200, "provider", "providers", title, page)
}

var providerSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,79}$`)

func (s *Server) saveProvider(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if r.ParseForm() != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	timeout, _ := strconv.Atoi(r.FormValue("timeout"))
	p := store.ModelProvider{ID: r.PathValue("id"), Name: strings.TrimSpace(r.FormValue("name")), Slug: strings.TrimSpace(r.FormValue("slug")), BaseURL: strings.TrimRight(strings.TrimSpace(r.FormValue("base_url")), "/"), Adapter: r.FormValue("adapter"), Enabled: r.FormValue("enabled") == "on", TimeoutSeconds: timeout}
	p.Options = store.ProviderOptions{AuthType: r.FormValue("auth_type"), AuthHeader: strings.TrimSpace(r.FormValue("auth_header")), Username: strings.TrimSpace(r.FormValue("auth_username")), TokenURL: strings.TrimSpace(r.FormValue("token_url")), ClientID: strings.TrimSpace(r.FormValue("client_id")), Scope: strings.TrimSpace(r.FormValue("scope")), Audience: strings.TrimSpace(r.FormValue("audience")), TokenAuth: r.FormValue("token_auth"), APIVersion: strings.TrimSpace(r.FormValue("api_version"))}
	models := strings.Fields(r.FormValue("models"))
	reject := func(message string) {
		s.render(w, r, 400, "provider", "providers", "Model provider", providerPage{Provider: p, Error: message, New: p.ID == ""})
	}
	if p.Name == "" || len(p.Name) > 120 {
		reject("Enter a provider name of 1–120 characters.")
		return
	}
	if p.ID == "" && !providerSlug.MatchString(p.Slug) {
		reject("Use a stable provider prefix of up to 80 lowercase letters, numbers, and hyphens.")
		return
	}
	var headers map[string]string
	if raw := strings.TrimSpace(r.FormValue("headers")); raw != "" {
		if json.Unmarshal([]byte(raw), &headers) != nil || headers == nil {
			reject("Additional headers must be a JSON object of header names and string values.")
			return
		}
	}
	key := strings.TrimSpace(r.FormValue("api_key"))
	// Validate the effective configuration, including retained secret headers.
	checkKey, checkHeaders := key, headers
	if p.ID != "" && r.FormValue("clear_credentials") != "on" {
		previous, err := s.store.ModelProviderCredential(r.Context(), p.ID)
		if err != nil {
			reject("Could not load the saved credentials.")
			return
		}
		if key == "" {
			checkKey = previous.BearerToken
		}
		if headers == nil {
			checkHeaders = previous.Headers
		}
	}
	if err := gateway.ValidateProvider(p, checkKey, checkHeaders); err != nil {
		reject(err.Error())
		return
	}
	if timeout < 10 || timeout > 3600 {
		reject("Timeout must be between 10 and 3600 seconds.")
		return
	}
	for _, m := range models {
		if len(m) > 256 {
			reject("Model IDs must be at most 256 characters.")
			return
		}
	}
	id, err := s.store.SaveModelProviderConfig(r.Context(), p, key, headers, r.FormValue("clear_credentials") == "on", models)
	if err != nil {
		reject("Could not save provider. Check for a duplicate name or prefix.")
		return
	}
	s.redirect(w, r, "/providers/"+id, flash{Kind: "ok", Message: "Provider saved. Discover its models or add model IDs manually."})
}

func (s *Server) discoverModels(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.GetModelProvider(r.Context(), r.PathValue("id"))
	if err != nil {
		s.notFound(w, r, "Provider")
		return
	}
	credential, err := s.store.ModelProviderCredential(r.Context(), p.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	p.Headers = credential.Headers
	models, discoverErr := gateway.Discover(r.Context(), s.modelClient, p, credential.BearerToken)
	detail := ""
	if discoverErr != nil {
		detail = discoverErr.Error()
	}
	if err = s.store.SaveDiscoveredModels(r.Context(), p.ID, models, detail); err != nil {
		s.fail(w, err)
		return
	}
	fl := flash{Kind: "ok", Message: "Model catalog refreshed. Existing model IDs were preserved."}
	if discoverErr != nil {
		fl = flash{Kind: "error", Message: detail}
	}
	s.redirect(w, r, "/providers/"+p.ID, fl)
}
