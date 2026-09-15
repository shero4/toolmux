package web

import (
	"context"
	"html"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/shero4/toolmux/internal/oauth"
	"github.com/shero4/toolmux/internal/store"
)

type authorizationRepository struct {
	*store.Store
	state, verifier string
}

func (repo *authorizationRepository) GetOAuthConfig(context.Context, string) (store.OAuthConfig, error) {
	return store.OAuthConfig{AuthorizationURL: "https://identity.example/authorize", TokenURL: "https://identity.example/token", ClientID: "toolmux"}, nil
}
func (repo *authorizationRepository) CreateOAuthState(_ context.Context, _ string, state, verifier string, _ time.Time) error {
	repo.state, repo.verifier = state, verifier
	return nil
}

func TestAuthorizeConnectionCompletesPostBeforeProviderNavigation(t *testing.T) {
	repo := &authorizationRepository{}
	server, err := New(nil, nil, oauth.New(repo, "https://toolmux.example"), nil, nil, "https://toolmux.example", slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/connections/connection/authorize", nil)
	request.SetPathValue("id", "connection")
	response := httptest.NewRecorder()
	securityHeaders(http.HandlerFunc(server.authorizeConnection)).ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Location") != "" {
		t.Fatalf("form response must finish without a redirect: %d %v", response.Code, response.Header())
	}
	if !strings.Contains(response.Header().Get("Content-Security-Policy"), "form-action 'self'") {
		t.Fatal("form policy changed")
	}
	body := response.Body.String()
	start := strings.Index(body, `href="https://identity.example/authorize?`)
	if start < 0 {
		t.Fatalf("missing provider continuation link: %s", body)
	}
	link := strings.SplitN(body[start+6:], `"`, 2)[0]
	target, err := url.Parse(html.UnescapeString(link))
	if err != nil {
		t.Fatal(err)
	}
	query := target.Query()
	if query.Get("state") != repo.state || repo.state == "" || repo.verifier == "" || query.Get("code_challenge") == "" || query.Get("code_challenge_method") != "S256" || query.Get("redirect_uri") != "https://toolmux.example/oauth/callback" {
		t.Fatalf("OAuth parameters lost: %v", query)
	}
	if !strings.Contains(body, "data-oauth-continue") || !strings.Contains(body, "Continue to sign in") {
		t.Fatal("missing automatic navigation hook or no-JavaScript fallback")
	}
}
