package checker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/shero4/toolmux/internal/execute"
	"github.com/shero4/toolmux/internal/mcp"
	"github.com/shero4/toolmux/internal/oauth"
	"github.com/shero4/toolmux/internal/store"
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
	if err == nil {
	} else if errors.Is(err, mcp.ErrUnauthorized) {
		check = store.Check{}
		check.Status = "reauthorization_required"
		check.Reachable = true
		check.Detail = "The upstream rejected the configured credential."
	} else {
		check = store.Check{Status: "unreachable", Detail: "The endpoint could not be reached."}
		check.Detail = cleanError(err)
	}
	if err := c.store.SaveCheck(ctx, id, check); err != nil {
		return err
	}
	return nil
}

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
		checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := c.Check(checkCtx, connection.ID)
		cancel()
		if err != nil {
			c.log.Error("scheduled connection check", "connection", connection.ID, "error", err)
		}
	}
}

func cleanError(err error) string {
	message := err.Error()
	if len(message) > 240 {
		message = message[:240]
	}
	return fmt.Sprintf("Connection failed: %s", message)
}
