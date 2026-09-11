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
	"time"

	"github.com/sentinel-mcp/sentinel/internal/store"
)

var ErrReauthorization = errors.New("OAuth reauthorization required")

type repository interface {
	GetOAuthConfig(context.Context, string) (store.OAuthConfig, error)
	CreateOAuthState(context.Context, string, string, string, time.Time) error
	ConsumeOAuthState(context.Context, string) (string, string, error)
	GetConnection(context.Context, string) (store.Connection, store.Credential, error)
	SaveCredential(context.Context, string, store.Credential) error
}

type Manager struct {
	store   repository
	baseURL string
	http    *http.Client
}

func New(store repository, baseURL string) *Manager {
	return &Manager{store: store, baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 15 * time.Second}}
}

func (m *Manager) Start(ctx context.Context, connectionID string) (string, error) {
	config, err := m.store.GetOAuthConfig(ctx, connectionID)
	if err != nil {
		return "", err
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
	query.Set("redirect_uri", m.baseURL+"/oauth/callback")
	query.Set("state", state)
	query.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
	query.Set("code_challenge_method", "S256")
	if config.Scopes != "" {
		query.Set("scope", config.Scopes)
	}
	target.RawQuery = query.Encode()
	return target.String(), nil
}

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
	if response.ExpiresIn > 0 {
		expires := time.Now().Add(time.Duration(response.ExpiresIn) * time.Second)
		credential.ExpiresAt = &expires
	}
	if err := m.store.SaveCredential(ctx, connectionID, credential); err != nil {
		return "", err
	}
	return connectionID, nil
}

func (m *Manager) Resolve(ctx context.Context, connection store.Connection, credential store.Credential) (store.Credential, error) {
	if connection.AuthMethod != "oauth2" {
		return credential, nil
	}
	if credential.AccessToken != "" && (credential.ExpiresAt == nil || credential.ExpiresAt.After(time.Now().Add(time.Minute))) {
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
	if response.ExpiresIn > 0 {
		expires := time.Now().Add(time.Duration(response.ExpiresIn) * time.Second)
		credential.ExpiresAt = &expires
	} else {
		credential.ExpiresAt = nil
	}
	if err := m.store.SaveCredential(ctx, connection.ID, credential); err != nil {
		return credential, err
	}
	return credential, nil
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
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, config.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	if config.TokenAuthMethod == "client_secret_post" {
		form.Set("client_secret", credential.ClientSecret)
		request.Body = io.NopCloser(strings.NewReader(form.Encode()))
		request.ContentLength = int64(len(form.Encode()))
	} else if credential.ClientSecret != "" {
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
