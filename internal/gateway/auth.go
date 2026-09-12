package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/shero4/toolmux/internal/store"
)

var headerName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

func validHeader(name, value string) bool {
	if !headerName.MatchString(name) || len(value) > 8192 {
		return false
	}
	for _, c := range value {
		if c < 32 && c != '\t' || c == 127 {
			return false
		}
	}
	switch strings.ToLower(name) {
	case "host", "connection", "content-length", "transfer-encoding", "upgrade", "trailer", "te", "proxy-authorization", "proxy-connection", "cookie", "set-cookie", "content-type", "accept-encoding":
		return false
	}
	return !strings.HasPrefix(strings.ToLower(name), "sec-")
}

func AuthType(p store.ModelProvider) string {
	if p.Options.AuthType != "" && p.Options.AuthType != "auto" {
		return p.Options.AuthType
	}
	if p.Adapter == "anthropic" || p.Adapter == "azure" {
		return "header"
	}
	return "bearer"
}

func authHeader(p store.ModelProvider) string {
	if p.Options.AuthHeader != "" {
		return p.Options.AuthHeader
	}
	if p.Adapter == "azure" {
		return "api-key"
	}
	return "x-api-key"
}

func ValidateProvider(p store.ModelProvider, key string, headers map[string]string) error {
	if err := ValidateBaseURL(p.BaseURL); err != nil {
		return err
	}
	if _, err := adapterFor(p.Adapter); err != nil {
		return err
	}
	switch AuthType(p) {
	case "none", "bearer":
	case "header":
		if !validHeader(authHeader(p), key) {
			return errors.New("use a valid authentication header")
		}
	case "basic":
		if strings.Contains(p.Options.Username, ":") || p.Options.Username == "" {
			return errors.New("enter a Basic authentication username without a colon")
		}
	case "oauth2":
		if err := ValidateBaseURL(p.Options.TokenURL); err != nil {
			return errors.New("enter a valid OAuth token URL")
		}
		if p.Options.ClientID == "" {
			return errors.New("enter the OAuth client ID")
		}
		if p.Options.TokenAuth != "" && p.Options.TokenAuth != "basic" && p.Options.TokenAuth != "body" {
			return errors.New("choose Basic or request body for OAuth client authentication")
		}
	default:
		return errors.New("choose a supported authentication method")
	}
	if len(key) > 16384 || strings.ContainsAny(key, "\r\n") {
		return errors.New("credential is too long or contains a newline")
	}
	if len(headers) > 32 {
		return errors.New("use at most 32 custom headers")
	}
	seen := map[string]bool{}
	for name, value := range headers {
		canonical := http.CanonicalHeaderKey(name)
		if !validHeader(name, value) || seen[canonical] {
			return errors.New("custom headers contain an invalid, reserved, or duplicate header")
		}
		seen[canonical] = true
		if AuthType(p) != "none" && (canonical == "Authorization" || canonical == http.CanonicalHeaderKey(authHeader(p))) {
			return errors.New("configure the authentication header through Authentication, not additional headers")
		}
	}
	if len(p.Options.APIVersion) > 80 || strings.ContainsAny(p.Options.APIVersion, "\r\n") {
		return errors.New("invalid API version")
	}
	if p.Adapter == "azure" && p.Options.APIVersion == "" {
		return errors.New("enter the Azure API version supported by your deployment")
	}
	return nil
}

func applyCredential(req *http.Request, p store.ModelProvider, key string) {
	for name, value := range p.Headers {
		req.Header.Set(name, value)
	}
	switch AuthType(p) {
	case "bearer", "oauth2":
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	case "header":
		if key != "" {
			req.Header.Set(authHeader(p), key)
		}
	case "basic":
		req.SetBasicAuth(p.Options.Username, key)
	}
}

type cachedToken struct {
	gate    chan struct{}
	token   string
	expires time.Time
}

// Tokens keeps short-lived client-credentials tokens in memory. Cache keys
// include the saved auth configuration and secret, so editing them invalidates
// reuse. Failed exchanges are never cached or retried as inference calls.
type Tokens struct {
	mu    sync.Mutex
	cache map[[32]byte]*cachedToken
}

func (t *Tokens) Resolve(ctx context.Context, client *http.Client, p store.ModelProvider, key string) (string, error) {
	if AuthType(p) != "oauth2" {
		return key, nil
	}
	options, _ := json.Marshal(p.Options)
	digest := sha256.Sum256(append(options, []byte("\x00"+key)...))
	t.mu.Lock()
	if t.cache == nil {
		t.cache = make(map[[32]byte]*cachedToken)
	}
	hit := t.cache[digest]
	if hit == nil {
		if len(t.cache) >= 256 {
			clear(t.cache)
		}
		hit = &cachedToken{gate: make(chan struct{}, 1)}
		t.cache[digest] = hit
	}
	t.mu.Unlock()
	select {
	case hit.gate <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-hit.gate }()
	if time.Now().Before(hit.expires) {
		return hit.token, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	values := url.Values{"grant_type": {"client_credentials"}}
	if p.Options.Scope != "" {
		values.Set("scope", p.Options.Scope)
	}
	if p.Options.Audience != "" {
		values.Set("audience", p.Options.Audience)
	}
	if p.Options.TokenAuth == "body" {
		values.Set("client_id", p.Options.ClientID)
		values.Set("client_secret", key)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", p.Options.TokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return "", errors.New("invalid token endpoint")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if p.Options.TokenAuth != "body" {
		req.SetBasicAuth(url.QueryEscape(p.Options.ClientID), url.QueryEscape(key))
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New("OAuth token endpoint unavailable")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return "", ErrProviderAuthorization
	}
	if resp.StatusCode == 400 && len(data) <= 1<<20 {
		var problem struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &problem) == nil && (problem.Error == "invalid_client" || problem.Error == "invalid_grant") {
			return "", ErrProviderAuthorization
		}
	}
	if err != nil || len(data) > 1<<20 || resp.StatusCode != 200 {
		return "", errors.New("OAuth token exchange failed; check client credentials and scope")
	}
	var token struct {
		Access  string `json:"access_token"`
		Type    string `json:"token_type"`
		Seconds int64  `json:"expires_in"`
	}
	if json.Unmarshal(data, &token) != nil || token.Access == "" || !strings.EqualFold(token.Type, "bearer") || strings.ContainsAny(token.Access, "\r\n") {
		return "", errors.New("OAuth endpoint did not return a bearer token")
	}
	// No stated expiry means no reuse. Bound retention even if an issuer reports
	// an unexpectedly large lifetime; short tokens keep a proportional skew.
	if token.Seconds > 0 {
		if token.Seconds > 86400 {
			token.Seconds = 86400
		}
		lifetime := time.Duration(token.Seconds) * time.Second
		skew := lifetime / 10
		if skew > 30*time.Second {
			skew = 30 * time.Second
		}
		hit.token, hit.expires = token.Access, time.Now().Add(lifetime-skew)
	}
	return token.Access, nil
}
