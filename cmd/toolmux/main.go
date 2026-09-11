package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shero4/toolmux/internal/checker"
	"github.com/shero4/toolmux/internal/config"
	"github.com/shero4/toolmux/internal/discovery"
	"github.com/shero4/toolmux/internal/execute"
	"github.com/shero4/toolmux/internal/mcp"
	"github.com/shero4/toolmux/internal/migrate"
	"github.com/shero4/toolmux/internal/oauth"
	"github.com/shero4/toolmux/internal/secretbox"
	"github.com/shero4/toolmux/internal/store"
	webui "github.com/shero4/toolmux/internal/web"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "keygen" {
		printSecrets()
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load()
	if err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	box, err := secretbox.New(cfg.MasterKey)
	if err != nil {
		log.Error("initialize credential encryption", "error", err)
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	db, err := store.Open(ctx, cfg.DatabaseURL, box)
	if err != nil {
		log.Error("open store", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	migrations, err := migrate.All()
	if err != nil {
		log.Error("load migrations", "error", err)
		os.Exit(1)
	}
	migrationSet := make(map[int]string, len(migrations))
	for _, migration := range migrations {
		migrationSet[migration.Version] = migration.SQL
	}
	if err := db.Migrate(ctx, migrationSet); err != nil {
		log.Error("migrate store", "error", err)
		os.Exit(1)
	}
	mcpClient := mcp.NewClient()
	executor := execute.New(db, mcpClient)
	oauthManager := oauth.New(db, cfg.BaseURL)
	health := checker.New(db, executor, oauthManager, log)
	detector := discovery.New(cfg.DiscoveryRoots)
	ui, err := webui.New(db, health, oauthManager, detector, cfg.BaseURL, log)
	if err != nil {
		log.Error("initialize web interface", "error", err)
		os.Exit(1)
	}
	mcpHandler := mcp.NewHandler(db, executor, oauthManager, log)
	server := &http.Server{Addr: cfg.Addr, Handler: ui.Handler(mcpHandler), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 45 * time.Second, IdleTimeout: 60 * time.Second}
	go health.Run(ctx, 5*time.Minute)
	go func() {
		log.Info("toolmux started", "addr", cfg.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", "error", err)
			cancel()
		}
	}()
	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", "error", err)
	}
}

func printSecrets() {
	master := make([]byte, 32)
	if _, err := rand.Read(master); err != nil {
		panic(err)
	}
	fmt.Printf("TOOLMUX_MASTER_KEY=%s\n", base64.StdEncoding.EncodeToString(master))
}
