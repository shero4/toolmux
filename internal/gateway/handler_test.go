package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shero4/toolmux/internal/store"
)

type fakeStore struct {
	provider         store.ModelProvider
	decision, reason string
	input, output    *int64
}

func (f *fakeStore) AuthenticateAgent(_ context.Context, key string) (store.Agent, error) {
	if key != "agent-key" {
		return store.Agent{}, store.ErrNotFound
	}
	return store.Agent{ID: "agent"}, nil
}
func (f *fakeStore) AvailableModels(context.Context) ([]string, error) {
	return []string{"demo/model/with-slash"}, nil
}
func (f *fakeStore) ResolveModel(_ context.Context, model string) (store.ModelProvider, string, string, error) {
	if model != "demo/model/with-slash" {
		return store.ModelProvider{}, "", "", store.ErrNotFound
	}
	return f.provider, "model/with-slash", "provider-key", nil
}
func (f *fakeStore) RecordModelCall(_ context.Context, agent, provider, alias, model, decision, reason string, duration time.Duration, input, output *int64) error {
	f.decision, f.reason, f.input, f.output = decision, reason, input, output
	return nil
}

func request(h http.Handler, path, body, token string) *httptest.ResponseRecorder {
	method := "POST"
	if path == "/v1/models" {
		method = "GET"
	}
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-Leak-Test", "private-client-header")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestGatewayRoutesAndPreservesPayload(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer provider-key" || r.Header.Get("X-Leak-Test") != "" {
			t.Error("wrong outbound headers")
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("wrong path %s", r.URL.Path)
		}
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		if string(body["model"]) != `"model/with-slash"` || string(body["extra"]) != `{"thinking":true}` {
			t.Errorf("payload changed: %s", body)
		}
		_, _ = io.WriteString(w, `{"model":"model/with-slash","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":4}}}`)
	}))
	defer upstream.Close()
	repo := &fakeStore{provider: store.ModelProvider{Name: "Demo", BaseURL: upstream.URL + "/v1", Adapter: "openai", TimeoutSeconds: 10}}
	h := New(repo, nil)
	w := request(h, "/v1/chat/completions", `{"model":"demo/model/with-slash","messages":[],"extra":{"thinking":true}}`, "agent-key")
	if w.Code != 200 || repo.decision != "allowed" || repo.input == nil || *repo.input != 12 || repo.output == nil || *repo.output != 7 {
		t.Fatalf("status=%d audit=%+v body=%s", w.Code, repo, w.Body.String())
	}
	if request(h, "/v1/models", "", "bad").Code != 401 {
		t.Fatal("invalid token accepted")
	}
	if request(h, "/v1/models", "", "agent-key").Code != 200 {
		t.Fatal("catalog unavailable")
	}
	if request(h, "/v1/chat/completions", `{"model":"not-configured"}`, "agent-key").Code != 404 || repo.decision != "denied" {
		t.Fatal("unknown model accepted")
	}
}

func TestGatewayStreamsAndDetectsIncomplete(t *testing.T) {
	for _, complete := range []bool{true, false} {
		t.Run(fmt.Sprint(complete), func(t *testing.T) {
			payload := "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n" +
				"data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n"
			if complete {
				payload += "data: [DONE]\n\n"
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for _, part := range strings.SplitAfter(payload, "\n") {
					_, _ = io.WriteString(w, part)
					w.(http.Flusher).Flush()
				}
			}))
			defer upstream.Close()
			repo := &fakeStore{provider: store.ModelProvider{BaseURL: upstream.URL, Adapter: "litellm", TimeoutSeconds: 10}}
			w := request(New(repo, nil), "/v1/chat/completions", `{"model":"demo/model/with-slash","stream":true}`, "agent-key")
			if w.Body.String() != payload || !w.Flushed {
				t.Fatal("stream was not preserved and flushed")
			}
			want := "error"
			if complete {
				want = "allowed"
			}
			if repo.decision != want {
				t.Fatalf("audit=%s", repo.decision)
			}
			if repo.input == nil || *repo.input != 3 {
				t.Fatal("stream usage missing")
			}
		})
	}
}

func TestGatewayRejectsRedirectAndSanitizesErrors(t *testing.T) {
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("followed redirect") }))
	defer sink.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", sink.URL)
		w.WriteHeader(307)
		_, _ = io.WriteString(w, "provider-key and prompt contents")
	}))
	defer upstream.Close()
	repo := &fakeStore{provider: store.ModelProvider{BaseURL: upstream.URL, Adapter: "openai", TimeoutSeconds: 10}}
	w := request(New(repo, nil), "/v1/responses", `{"model":"demo/model/with-slash","input":"private"}`, "agent-key")
	if w.Code != 502 || strings.Contains(w.Body.String(), "provider-key") || strings.Contains(repo.reason, "private") {
		t.Fatal("unsafe upstream error")
	}
}

func TestUsageHandlesResponsesAndBoundedFragments(t *testing.T) {
	u := &usage{}
	payload := `data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":20,"output_tokens_details":{"reasoning_tokens":5}}}}` + "\n\n"
	for _, b := range []byte(payload) {
		_, _ = u.Write([]byte{b})
	}
	if !u.Complete || u.Input == nil || *u.Input != 10 || u.Output == nil || *u.Output != 20 {
		t.Fatalf("usage=%+v", u)
	}
	_, _ = u.Write([]byte(strings.Repeat("x", 2<<20)))
	if len(u.pending) > 1<<20 {
		t.Fatal("unbounded buffer")
	}
}

func TestDiscoverCatalog(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/models" {
			t.Fatal("wrong discovery request")
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"model/a"},{"id":"model/a"},{"id":"model/b"}]}`)
	}))
	defer upstream.Close()
	models, err := Discover(context.Background(), Client(), store.ModelProvider{Adapter: "openai", BaseURL: upstream.URL + "/v1"}, "key")
	if err != nil || len(models) != 2 {
		t.Fatalf("models=%v err=%v", models, err)
	}
	for _, raw := range []string{"ftp://example.test", "https://user:key@example.test/v1", "https://example.test/v1?key=secret"} {
		if ValidateBaseURL(raw) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
