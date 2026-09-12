// Package oauth runs Authorization Code + PKCE flows for upstream connections
// and refreshes access tokens before they expire.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/shero4/toolmux/internal/store"
)

var ErrReauthorization = errors.New("OAuth reauthorization required")

type repository interface {
	GetOAuthConfig(context.Context, string) (store.OAuthConfig, error)
	CreateOAuthState(context.Context, string, string, string, time.Time) error
	ConsumeOAuthState(context.Context, string) (string, string, error)
	GetConnection(context.Context, string) (store.Connection, store.Credential, error)
	SaveCredential(context.Context, string, store.Credential) error
	SaveOAuthClient(context.Context, string, string, string, string, string) error
}

type Manager struct {
	store      repository
	baseURL    string
	http       *http.Client
	refreshing sync.Map // connection ID -> *sync.Mutex
}

func New(store repository, baseURL string) *Manager {
	return &Manager{store: store, baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 15 * time.Second}}
}

// Start prepares an authorization request and returns the provider URL to
// redirect the operator to.
func (m *Manager) Start(ctx context.Context, connectionID string) (string, error) {
	config, err := m.store.GetOAuthConfig(ctx, connectionID)
	if err != nil {
		return "", err
	}
	callback := m.baseURL + "/oauth/callback"
	if config.ClientID == "" || (config.RegistrationURL != "" && config.RedirectURI != callback) {
		client, err := m.register(ctx, config.RegistrationURL, callback)
		if err != nil {
			return "", err
		}
		if err := m.store.SaveOAuthClient(ctx, connectionID, client.ClientID, client.ClientSecret, client.TokenAuthMethod, callback); err != nil {
			return "", err
		}
		config.ClientID = client.ClientID
		config.TokenAuthMethod = client.TokenAuthMethod
		config.RedirectURI = callback
	}
	if config.AuthorizationURL == "" || config.TokenURL == "" || config.ClientID == "" {
		return "", errors.New("OAuth metadata is incomplete")
	}
	state, err := random(32)
	if err != nil {
		return "", err
	}
	verifier, err := random(48)
	if err != nil {
		return "", err
	}
	if err := m.store.CreateOAuthState(ctx, connectionID, state, verifier, time.Now().Add(10*time.Minute)); err != nil {
		return "", err
	}
	challenge := sha256.Sum256([]byte(verifier))
	target, err := url.Parse(config.AuthorizationURL)
	if err != nil {
		return "", err
	}
	query := target.Query()
	query.Set("response_type", "code")
	query.Set("client_id", config.ClientID)
	query.Set("redirect_uri", callback)
	query.Set("state", state)
	query.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
	query.Set("code_challenge_method", "S256")
	if config.Scopes != "" {
		query.Set("scope", config.Scopes)
	}
	target.RawQuery = query.Encode()
	return target.String(), nil
}

type registeredClient struct {
	ClientID        string `json:"client_id"`
	ClientSecret    string `json:"client_secret"`
	TokenAuthMethod string `json:"token_endpoint_auth_method"`
}

func (m *Manager) register(ctx context.Context, registrationURL, redirectURI string) (registeredClient, error) {
	if registrationURL == "" {
		return registeredClient{}, errors.New("this provider did not advertise dynamic client registration")
	}
	payload, err := json.Marshal(map[string]any{
		"client_name":                "Toolmux",
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	if err != nil {
		return registeredClient{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, registrationURL, strings.NewReader(string(payload)))
	if err != nil {
		return registeredClient{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := m.http.Do(request)
	if err != nil {
		return registeredClient{}, fmt.Errorf("OAuth client registration: %w", err)
	}
	defer response.Body.Close()
	var client registeredClient
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&client); err != nil {
		return registeredClient{}, fmt.Errorf("decode OAuth client registration: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || client.ClientID == "" {
		return registeredClient{}, fmt.Errorf("OAuth client registration returned %s", response.Status)
	}
	if client.TokenAuthMethod == "" {
		client.TokenAuthMethod = "none"
	}
	if client.TokenAuthMethod != "none" && client.TokenAuthMethod != "client_secret_basic" && client.TokenAuthMethod != "client_secret_post" {
		return registeredClient{}, fmt.Errorf("unsupported OAuth token authentication %q", client.TokenAuthMethod)
	}
	return client, nil
}

// Complete exchanges the callback code for tokens and stores them.
func (m *Manager) Complete(ctx context.Context, stateValue, code string) (string, error) {
	connectionID, verifier, err := m.store.ConsumeOAuthState(ctx, stateValue)
	if err != nil {
		return "", err
	}
	config, err := m.store.GetOAuthConfig(ctx, connectionID)
	if err != nil {
		return "", err
	}
	_, credential, err := m.store.GetConnection(ctx, connectionID)
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {m.baseURL + "/oauth/callback"}, "client_id": {config.ClientID}, "code_verifier": {verifier}}
	response, err := m.exchange(ctx, config, credential, form)
	if err != nil {
		return "", err
	}
	credential.AccessToken = response.AccessToken
	credential.TokenType = response.TokenType
	credential.RefreshToken = response.RefreshToken
	credential.ExpiresAt = expiry(response.ExpiresIn)
	if err := m.store.SaveCredential(ctx, connectionID, credential); err != nil {
		return "", err
	}
	return connectionID, nil
}

// Resolve returns a credential that is valid for the next minute, refreshing
// it first when needed. Refreshes for one connection are serialized so
// concurrent calls never spend the same refresh token twice.
func (m *Manager) Resolve(ctx context.Context, connection store.Connection, credential store.Credential) (store.Credential, error) {
	if connection.AuthMethod != "oauth2" || fresh(credential) {
		return credential, nil
	}
	lock, _ := m.refreshing.LoadOrStore(connection.ID, &sync.Mutex{})
	mutex := lock.(*sync.Mutex)
	mutex.Lock()
	defer mutex.Unlock()
	// Another caller may have refreshed while this one waited for the lock.
	_, latest, err := m.store.GetConnection(ctx, connection.ID)
	if err != nil {
		return credential, err
	}
	credential = latest
	if fresh(credential) {
		return credential, nil
	}
	if credential.RefreshToken == "" {
		return credential, ErrReauthorization
	}
	config, err := m.store.GetOAuthConfig(ctx, connection.ID)
	if err != nil {
		return credential, err
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {credential.RefreshToken}, "client_id": {config.ClientID}}
	response, err := m.exchange(ctx, config, credential, form)
	if err != nil {
		return credential, ErrReauthorization
	}
	credential.AccessToken = response.AccessToken
	credential.TokenType = response.TokenType
	if response.RefreshToken != "" {
		credential.RefreshToken = response.RefreshToken
	}
	credential.ExpiresAt = expiry(response.ExpiresIn)
	if err := m.store.SaveCredential(ctx, connection.ID, credential); err != nil {
		return credential, err
	}
	return credential, nil
}

func fresh(credential store.Credential) bool {
	return credential.AccessToken != "" && (credential.ExpiresAt == nil || credential.ExpiresAt.After(time.Now().Add(time.Minute)))
}

func expiry(expiresIn int64) *time.Time {
	if expiresIn <= 0 {
		return nil
	}
	expires := time.Now().Add(time.Duration(expiresIn) * time.Second)
	return &expires
}

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int64  `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func (m *Manager) exchange(ctx context.Context, config store.OAuthConfig, credential store.Credential, form url.Values) (tokenResponse, error) {
	if config.TokenAuthMethod == "client_secret_post" {
		form.Set("client_secret", credential.ClientSecret)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, config.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	if config.TokenAuthMethod == "client_secret_basic" && credential.ClientSecret != "" {
		request.SetBasicAuth(config.ClientID, credential.ClientSecret)
	}
	response, err := m.http.Do(request)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("OAuth token endpoint: %w", err)
	}
	defer response.Body.Close()
	var token tokenResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&token); err != nil {
		return tokenResponse{}, fmt.Errorf("decode OAuth token response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || token.Error != "" {
		detail := token.ErrorDescription
		if detail == "" {
			detail = token.Error
		}
		if detail == "" {
			detail = response.Status
		}
		return tokenResponse{}, fmt.Errorf("OAuth token exchange failed: %s", detail)
	}
	if token.AccessToken == "" {
		return tokenResponse{}, errors.New("OAuth token response contained no access token")
	}
	return token, nil
}

func random(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}
