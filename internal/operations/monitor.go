// Package operations performs bounded authorization checks and transition alerts.
package operations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/shero4/toolmux/internal/checker"
	"github.com/shero4/toolmux/internal/gateway"
	"github.com/shero4/toolmux/internal/store"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Monitor struct {
	Store   *store.Store
	Checker *checker.Checker
	Codex   *Codex
	mu      sync.Mutex
}

func New(db *store.Store, check *checker.Checker) *Monitor {
	return &Monitor{Store: db, Checker: check, Codex: NewCodex()}
}
func ValidateWebhook(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"))) {
		return fmt.Errorf("use HTTPS, or HTTP on localhost for testing")
	}
	return nil
}
func (m *Monitor) Run(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			m.deliver(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			v := m.Codex.Check(ctx)
			if !v.Running {
				_ = m.Store.Observe(ctx, "subscription", "codex", "Local Codex bridge", v.Status)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	var next time.Time
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if time.Now().After(next) {
			settings, err := m.Store.OperationSettings(ctx)
			if err == nil {
				m.Check(ctx)
				next = time.Now().Add(time.Duration(settings.IntervalSeconds) * time.Second)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (m *Monitor) Check(ctx context.Context) {
	if !m.mu.TryLock() {
		return
	}
	defer m.mu.Unlock()
	ctx, stop := context.WithTimeout(ctx, 90*time.Second)
	defer stop()
	conns, err := m.Store.ListConnections(ctx)
	if err != nil {
		return
	}
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	run := func(fn func(context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			c, stop := context.WithTimeout(ctx, 45*time.Second)
			defer stop()
			fn(c)
		}()
	}
	for _, conn := range conns {
		conn := conn
		if conn.Status == "disabled" {
			_ = m.Store.Observe(ctx, "connection", conn.ID, conn.Name, "disabled")
			continue
		}
		run(func(c context.Context) {
			if m.Checker == nil {
				return
			}
			if m.Checker.Check(c, conn.ID) != nil {
				return
			}
			current, e := m.Store.GetConnectionSummary(c, conn.ID)
			if e == nil {
				_ = m.Store.Observe(c, "connection", conn.ID, conn.Name, current.Status)
			}
		})
	}
	providers, _ := m.Store.ListModelProviders(ctx)
	for _, p := range providers {
		p := p
		if !p.Enabled {
			_ = m.Store.Observe(ctx, "provider", p.ID, p.Name, "disabled")
			continue
		}
		run(func(c context.Context) {
			status := "unknown"
			credential, e := m.Store.ModelProviderCredential(c, p.ID)
			if e != nil {
				return
			}
			p.Headers = credential.Headers
			if p.Adapter != "azure" {
				_, e = gateway.Discover(c, gateway.Client(), p, credential.BearerToken)
				if e == nil {
					status = "connected"
				} else if errors.Is(e, gateway.ErrProviderAuthorization) {
					status = "reauthorization_required"
				} else if strings.Contains(e.Error(), "could not reach") {
					status = "unreachable"
				}
			}
			_ = m.Store.Observe(c, "provider", p.ID, p.Name, status)
		})
	}
	wg.Wait()
	_ = m.Store.PruneOperations(ctx)
}
func SendWebhook(ctx context.Context, endpoint string, payload []byte, id string) bool {
	if ValidateWebhook(endpoint) != nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(payload))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Toolmux-Authorization-Monitor/1")
	req.Header.Set("X-Toolmux-Event-ID", id)
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
func (m *Monitor) deliver(ctx context.Context) {
	endpoint, err := m.Store.WebhookURL(ctx)
	if err != nil || endpoint == "" {
		return
	}
	for i := 0; i < 5; i++ {
		d, err := m.Store.NextDelivery(ctx)
		if err != nil {
			return
		}
		ok := SendWebhook(ctx, endpoint, d.Payload, fmt.Sprint(d.ID))
		_ = m.Store.FinishDelivery(ctx, d, ok)
		status, message := "delivered", "Authorization alert delivered."
		if !ok {
			status = "retrying"
			message = "Webhook failed; a bounded retry is scheduled."
			if d.Attempts >= 2 {
				status = "failed"
				message = "Webhook failed after three attempts. Check the endpoint in Settings."
			}
		}
		_ = m.Store.LogOperation(ctx, "webhook", "", "Authorization alerts", status, message)
	}
}
