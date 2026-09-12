// Package gateway exposes a shared model catalog and bounded inference proxy.
// Provider selection belongs to the client, through <provider>/<model> IDs.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shero4/toolmux/internal/store"
)

// Adapter is the provider wire boundary. LiteLLM is an optional external
// translation service using the same OpenAI wire format as direct providers.
type Adapter interface {
	Request(context.Context, store.ModelProvider, string, string, io.Reader) (*http.Request, error)
}

type OpenAI struct{}
type Anthropic struct{}
type Azure struct{}

func Supports(kind, path string) bool {
	switch kind {
	case "anthropic":
		return path == "messages" || path == "messages/count_tokens" || path == "models"
	case "azure":
		return path == "chat/completions" || path == "completions" || path == "embeddings"
	case "litellm":
		return true
	default:
		return path != "messages" && path != "messages/count_tokens"
	}
}

func ValidateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("use an HTTP or HTTPS API base URL without credentials, query, or fragment")
	}
	return nil
}

func (OpenAI) Request(ctx context.Context, p store.ModelProvider, key, path string, body io.Reader) (*http.Request, error) {
	if err := ValidateBaseURL(p.BaseURL); err != nil {
		return nil, err
	}
	method := http.MethodPost
	if path == "models" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(p.BaseURL, "/")+"/"+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	applyCredential(req, p, key)
	return req, nil
}

func (Anthropic) Request(ctx context.Context, p store.ModelProvider, key, path string, body io.Reader) (*http.Request, error) {
	if !strings.HasSuffix(strings.TrimRight(p.BaseURL, "/"), "/v1") {
		p.BaseURL = strings.TrimRight(p.BaseURL, "/") + "/v1"
	}
	req, err := (OpenAI{}).Request(ctx, p, key, path, body)
	if err != nil {
		return nil, err
	}
	version := p.Options.APIVersion
	if version == "" {
		version = "2023-06-01"
	}
	if req.Header.Get("Anthropic-Version") == "" {
		req.Header.Set("Anthropic-Version", version)
	}
	return req, nil
}

func (Azure) Request(ctx context.Context, p store.ModelProvider, key, path string, body io.Reader) (*http.Request, error) {
	if path == "models" {
		return nil, errors.New("Azure uses deployment names; add your deployment names manually")
	}
	// Azure's classic deployment API encodes the deployment name in the URL.
	data, err := io.ReadAll(io.LimitReader(body, (16<<20)+1))
	if err != nil || len(data) > 16<<20 {
		return nil, errors.New("invalid request body")
	}
	var input struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(data, &input) != nil || input.Model == "" {
		return nil, errors.New("missing deployment name")
	}
	if err = ValidateBaseURL(p.BaseURL); err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(p.BaseURL, "/") + "/openai/deployments/" + url.PathEscape(input.Model) + "/" + path + "?" + url.Values{"api-version": {p.Options.APIVersion}}.Encode()
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(string(data)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	applyCredential(req, p, key)
	return req, nil
}

func adapterFor(kind string) (Adapter, error) {
	switch kind {
	case "openai", "litellm":
		return OpenAI{}, nil
	case "anthropic":
		return Anthropic{}, nil
	case "azure":
		return Azure{}, nil
	default:
		return nil, errors.New("unsupported provider adapter")
	}
}

func Client() *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, MaxIdleConns: 100, MaxIdleConnsPerHost: 10, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 90 * time.Second}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

var ErrProviderAuthorization = errors.New("provider authorization required")
var discoveryTokens Tokens

func Discover(ctx context.Context, client *http.Client, p store.ModelProvider, key string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if p.Adapter == "azure" {
		return nil, errors.New("Azure uses deployment names; add deployment names manually")
	}
	if err := ValidateProvider(p, key, p.Headers); err != nil {
		return nil, err
	}
	key, err := discoveryTokens.Resolve(ctx, client, p, key)
	if err != nil {
		return nil, err
	}
	adapter, err := adapterFor(p.Adapter)
	if err != nil {
		return nil, err
	}
	req, err := adapter.Request(ctx, p, key, "models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("could not reach provider")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, ErrProviderAuthorization
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("model discovery returned HTTP %d; check the API key and base URL, or add models manually", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20+1))
	if err != nil || len(data) > 4<<20 {
		return nil, errors.New("model catalog exceeds the response limit")
	}
	var catalog struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &catalog) != nil || catalog.Data == nil {
		return nil, errors.New("provider did not return a supported model catalog; add model IDs manually")
	}
	models := []string{}
	seen := map[string]bool{}
	for _, m := range catalog.Data {
		if m.ID != "" && len(m.ID) <= 256 && !strings.Contains(m.ID, "*") && !seen[m.ID] {
			models = append(models, m.ID)
			seen[m.ID] = true
		}
	}
	return models, nil
}
