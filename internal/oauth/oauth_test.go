package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/shero4/toolmux/internal/store"
)

type fakeRepository struct {
	config          store.OAuthConfig
	connection      store.Connection
	credential      store.Credential
	state, verifier string
}

func (f *fakeRepository) GetOAuthConfig(context.Context, string) (store.OAuthConfig, error) {
	return f.config, nil
}
func (f *fakeRepository) CreateOAuthState(_ context.Context, _, state, verifier string, _ time.Time) error {
	f.state = state
	f.verifier = verifier
	return nil
}
func (f *fakeRepository) ConsumeOAuthState(_ context.Context, state string) (string, string, error) {
	if state != f.state {
		return "", "", store.ErrNotFound
	}
	return f.connection.ID, f.verifier, nil
}
func (f *fakeRepository) GetConnection(context.Context, string) (store.Connection, store.Credential, error) {
	return f.connection, f.credential, nil
}
func (f *fakeRepository) SaveCredential(_ context.Context, _ string, credential store.Credential) error {
	f.credential = credential
	return nil
}
func (f *fakeRepository) SaveOAuthClient(_ context.Context, _ string, clientID, clientSecret, tokenAuthMethod, redirectURI string) error {
	f.config.ClientID = clientID
	f.config.TokenAuthMethod = tokenAuthMethod
	f.config.RedirectURI = redirectURI
	f.credential.ClientSecret = clientSecret
	return nil
}

func TestAuthorizationAndRefresh(t *testing.T) {
	var grants []string
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, password, ok := r.BasicAuth(); !ok || user != "client" || password != "secret" {
			t.Error("missing client authentication")
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		grants = append(grants, r.Form.Get("grant_type"))
		w.Header().Set("Content-Type", "application/json")
		if r.Form.Get("grant_type") == "authorization_code" && r.Form.Get("code_verifier") == "" {
			t.Error("missing PKCE verifier")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-" + r.Form.Get("grant_type"), "refresh_token": "refresh-next", "token_type": "Bearer", "expires_in": 1})
	}))
	defer tokenServer.Close()
	repo := &fakeRepository{config: store.OAuthConfig{AuthorizationURL: "https://identity.example/authorize", TokenURL: tokenServer.URL, ClientID: "client", Scopes: "read", TokenAuthMethod: "client_secret_basic"}, connection: store.Connection{ID: "connection", AuthMethod: "oauth2"}, credential: store.Credential{ClientSecret: "secret"}}
	manager := New(repo, "http://localhost:8080")
	target, err := manager.Start(context.Background(), "connection")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(target)
	if parsed.Query().Get("code_challenge") == "" || parsed.Query().Get("state") != repo.state {
		t.Fatalf("invalid authorization URL: %s", target)
	}
	if _, err := manager.Complete(context.Background(), repo.state, "authorization-code"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(repo.credential.AccessToken, "access-authorization_code") {
		t.Fatalf("unexpected access token %q", repo.credential.AccessToken)
	}
	time.Sleep(2 * time.Millisecond)
	past := time.Now().Add(-time.Minute)
	repo.credential.ExpiresAt = &past
	if _, err := manager.Resolve(context.Background(), repo.connection, repo.credential); err != nil {
		t.Fatal(err)
	}
	if len(grants) != 2 || grants[0] != "authorization_code" || grants[1] != "refresh_token" {
		t.Fatalf("unexpected grants %v", grants)
	}
}

func TestStartRegistersPublicClientForToolmuxCallback(t *testing.T) {
	registration := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			RedirectURIs []string `json:"redirect_uris"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if len(request.RedirectURIs) != 1 || request.RedirectURIs[0] != "http://localhost:8080/oauth/callback" {
			t.Fatalf("unexpected redirect URIs: %v", request.RedirectURIs)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"client_id": "toolmux-client", "token_endpoint_auth_method": "none"})
	}))
	defer registration.Close()

	repo := &fakeRepository{
		config:     store.OAuthConfig{AuthorizationURL: "https://identity.example/authorize", TokenURL: "https://identity.example/token", RegistrationURL: registration.URL, RedirectURI: "http://localhost:9000/callback"},
		connection: store.Connection{ID: "connection", AuthMethod: "oauth2"},
	}
	manager := New(repo, "http://localhost:8080")
	target, err := manager.Start(context.Background(), "connection")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Query().Get("client_id") != "toolmux-client" || repo.config.TokenAuthMethod != "none" {
		t.Fatalf("registered client was not used: %s", target)
	}
}
