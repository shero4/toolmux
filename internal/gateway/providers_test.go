package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/shero4/toolmux/internal/store"
)

func TestNativeAnthropic(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			payload := `{"id":"msg_test","type":"message","content":[{"type":"tool_use","id":"t1","name":"lookup","input":{}}],"usage":{"input_tokens":7,"output_tokens":9}}`
			if stream {
				payload = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":1}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":9}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/messages" || r.Header.Get("X-Api-Key") != "provider-key" || r.Header.Get("Authorization") != "" || r.Header.Get("Anthropic-Version") != "2023-06-01" || r.Header.Get("Anthropic-Beta") != "test-feature" {
					t.Error("wrong Anthropic path or headers")
				}
				var body map[string]json.RawMessage
				_ = json.NewDecoder(r.Body).Decode(&body)
				if string(body["model"]) != `"model/with-slash"` || string(body["thinking"]) != `{"type":"enabled","budget_tokens":1024}` {
					t.Error("native payload lost")
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				io.WriteString(w, payload)
			}))
			defer upstream.Close()
			repo := &fakeStore{provider: store.ModelProvider{Adapter: "anthropic", BaseURL: upstream.URL, TimeoutSeconds: 10}}
			h := New(repo, nil)
			r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"demo/model/with-slash","thinking":{"type":"enabled","budget_tokens":1024}}`))
			r.Header.Set("X-Api-Key", "agent-key")
			r.Header.Set("Anthropic-Beta", "test-feature")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 200 || w.Body.String() != payload || repo.decision != "allowed" || repo.input == nil || *repo.input != 7 || repo.output == nil || *repo.output != 9 {
				t.Fatalf("status=%d audit=%+v body=%s", w.Code, repo, w.Body.String())
			}
			if request(h, "/v1/chat/completions", `{"model":"demo/model/with-slash"}`, "agent-key").Code != 400 {
				t.Fatal("protocol mismatch was not rejected")
			}
		})
	}
}

func TestProviderCredentials(t *testing.T) {
	cases := []struct{ kind, header, user, want string }{
		{"bearer", "Authorization", "", "Bearer key"},
		{"header", "X-Custom-Key", "", "key"},
		{"basic", "Authorization", "user", "Basic dXNlcjprZXk="},
		{"none", "Authorization", "", ""},
	}
	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			p := store.ModelProvider{Adapter: "openai", BaseURL: "https://example.test/v1", Options: store.ProviderOptions{AuthType: c.kind, AuthHeader: c.header, Username: c.user}, Headers: map[string]string{"OpenAI-Project": "project"}}
			if err := ValidateProvider(p, "key", p.Headers); err != nil {
				t.Fatal(err)
			}
			r, err := (OpenAI{}).Request(context.Background(), p, "key", "models", nil)
			if err != nil || r.Header.Get(c.header) != c.want || r.Header.Get("OpenAI-Project") != "project" {
				t.Fatalf("request=%v err=%v", r, err)
			}
		})
	}
	p := store.ModelProvider{Adapter: "openai", BaseURL: "https://example.test/v1"}
	for _, headers := range []map[string]string{{"Host": "evil"}, {"Connection": "keep-alive"}, {"X-Test": "value\r\ninjected"}, {"Authorization": "other"}, {"X-Test": "a", "x-test": "b"}} {
		if ValidateProvider(p, "key", headers) == nil {
			t.Fatal("accepted reserved, duplicate, or malformed headers")
		}
	}
}

func TestAzureDeploymentAndVersion(t *testing.T) {
	p := store.ModelProvider{Adapter: "azure", BaseURL: "https://resource.example", Options: store.ProviderOptions{APIVersion: "2025-01-01-preview"}}
	r, err := (Azure{}).Request(context.Background(), p, "key", "chat/completions", strings.NewReader(`{"model":"my-deployment","messages":[]}`))
	if err != nil || r.URL.Path != "/openai/deployments/my-deployment/chat/completions" || r.URL.Query().Get("api-version") != "2025-01-01-preview" || r.Header.Get("Api-Key") != "key" {
		t.Fatalf("request=%v err=%v", r, err)
	}
}

func TestOAuthClientCredentialsCacheAndRotation(t *testing.T) {
	for _, style := range []string{"basic", "body"} {
		t.Run(style, func(t *testing.T) {
			var calls atomic.Int32
			tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_ = r.ParseForm()
				id, secret, ok := r.BasicAuth()
				if style == "body" {
					id = r.FormValue("client_id")
					secret = r.FormValue("client_secret")
					ok = true
				}
				if !ok || id != "client" || (secret != "secret" && secret != "rotated") || r.FormValue("grant_type") != "client_credentials" || r.FormValue("scope") != "inference" {
					t.Error("invalid token request")
				}
				fmt.Fprint(w, `{"access_token":"short-lived","token_type":"Bearer","expires_in":300}`)
			}))
			defer tokenServer.Close()
			p := store.ModelProvider{Options: store.ProviderOptions{AuthType: "oauth2", TokenURL: tokenServer.URL, ClientID: "client", Scope: "inference", TokenAuth: style}}
			var tokens Tokens
			client := Client()
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					token, err := tokens.Resolve(context.Background(), client, p, "secret")
					if err != nil || token != "short-lived" {
						t.Error("token resolve failed", err)
					}
				}()
			}
			wg.Wait()
			if calls.Load() != 1 {
				t.Fatalf("exchanges=%d", calls.Load())
			}
			if _, err := tokens.Resolve(context.Background(), client, p, "rotated"); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 {
				t.Fatal("secret edit reused old token")
			}
		})
	}
}

func TestAnthropicIncompleteAndErrorStreams(t *testing.T) {
	var meter usage
	meter.Write([]byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5}}}\n\n"))
	if meter.Complete {
		t.Fatal("early stream marked complete")
	}
	meter.Write([]byte("data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n"))
	if !meter.Failed {
		t.Fatal("error not recorded")
	}
}
