// Package checker verifies upstream connections and records their health.
package checker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/shero4/toolmux/internal/execute"
	"github.com/shero4/toolmux/internal/mcp"
	"github.com/shero4/toolmux/internal/oauth"
	"github.com/shero4/toolmux/internal/store"
)

const (
	checkTimeout = 45 * time.Second
	parallelism  = 4
)

type Checker struct {
	store    *store.Store
	executor *execute.Router
	oauth    *oauth.Manager
	log      *slog.Logger
}

func New(store *store.Store, executor *execute.Router, oauth *oauth.Manager, log *slog.Logger) *Checker {
	return &Checker{store: store, executor: executor, oauth: oauth, log: log}
}

// Check runs one connection check now and stores the outcome. It returns an
// error only when the connection could not be loaded or the result could not
// be saved; upstream failures are recorded as the connection's status.
func (c *Checker) Check(ctx context.Context, id string) error {
	connection, credential, err := c.store.GetConnection(ctx, id)
	if err != nil {
		return err
	}
	credential, err = c.oauth.Resolve(ctx, connection, credential)
	if errors.Is(err, oauth.ErrReauthorization) {
		return c.store.SaveCheck(ctx, id, store.Check{Status: "reauthorization_required", Reachable: true, Detail: "OAuth authorization is required."})
	}
	if err != nil {
		return err
	}
	check, err := c.executor.Check(ctx, connection, credential)
	switch {
	case err == nil:
	case errors.Is(err, mcp.ErrUnauthorized):
		check = store.Check{Status: "reauthorization_required", Reachable: true, Detail: "The upstream rejected the configured credential."}
	default:
		check = store.Check{Status: "unreachable", Detail: describe(err)}
	}
	return c.store.SaveCheck(ctx, id, check)
}

// CheckMany checks the given connections in the background with bounded
// parallelism. It returns immediately.
func (c *Checker) CheckMany(ids []string) {
	seen := make(map[string]bool, len(ids))
	unique := ids[:0:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	go func() {
		sem := make(chan struct{}, parallelism)
		var wg sync.WaitGroup
		for _, id := range unique {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
				defer cancel()
				if err := c.Check(ctx, id); err != nil {
					c.log.Error("background connection check", "connection", id, "error", err)
				}
			}(id)
		}
		wg.Wait()
	}()
}

// Run checks every enabled connection on the interval until ctx ends.
func (c *Checker) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.checkAll(ctx)
		}
	}
}

func (c *Checker) checkAll(ctx context.Context) {
	connections, err := c.store.ListConnections(ctx)
	if err != nil {
		c.log.Error("list connections for health check", "error", err)
		return
	}
	for _, connection := range connections {
		if connection.Status == "disabled" {
			continue
		}
		checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
		err := c.Check(checkCtx, connection.ID)
		cancel()
		if err != nil {
			c.log.Error("scheduled connection check", "connection", connection.ID, "error", err)
		}
	}
}

func describe(err error) string {
	message := err.Error()
	if len(message) > 240 {
		message = message[:240]
	}
	return "Connection failed: " + message
}
