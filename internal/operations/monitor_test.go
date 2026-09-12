package operations

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestWebhookDoesNotFollowRedirects(t *testing.T) {
	var called atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called.Store(true) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer source.Close()
	if SendWebhook(context.Background(), source.URL, []byte(`{"text":"test"}`), "event-1") || called.Load() {
		t.Fatal("webhook redirect followed")
	}
}
func TestWebhookValidation(t *testing.T) {
	for _, v := range []string{"http://example.com", "https://user:pass@example.com", "file:///tmp/target", "https://example.com/#fragment"} {
		if ValidateWebhook(v) == nil {
			t.Fatal("accepted", v)
		}
	}
	for _, v := range []string{"https://hooks.slack.com/services/example", "http://127.0.0.1:8082/hook"} {
		if ValidateWebhook(v) != nil {
			t.Fatal("rejected", v)
		}
	}
}

func TestWebhookPayloadAndOutcome(t *testing.T) {
	for _, status := range []int{204, 401, 500} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("X-Toolmux-Event-ID") != "event-42" {
				t.Error("incorrect webhook request metadata")
			}
			w.WriteHeader(status)
		}))
		ok := SendWebhook(context.Background(), server.URL, []byte(`{"text":"Authorization required"}`), "event-42")
		server.Close()
		if ok != (status == 204) {
			t.Fatalf("status %d reported success=%v", status, ok)
		}
	}
}
