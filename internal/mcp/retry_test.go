package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDoWithRetryReplaysBodyAfter429(t *testing.T) {
	retrySleep = func(context.Context, time.Duration) error { return nil }
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data := make([]byte, 16)
		n, _ := r.Body.Read(data)
		seen = append(seen, string(data[:n]))
		if len(seen) < 3 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	req, _ := http.NewRequest(http.MethodPost, server.URL, strings.NewReader("payload"))
	resp, err := DoWithRetry(context.Background(), server.Client(), req, []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || len(seen) != 3 || seen[2] != "payload" {
		t.Fatalf("status %d, attempts %v", resp.StatusCode, seen)
	}
}

func TestDoWithRetryGivesUpOnLongRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	resp, err := DoWithRetry(context.Background(), server.Client(), req, nil)
	if err != nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected the 429 to be returned, got %v %v", resp, err)
	}
}
