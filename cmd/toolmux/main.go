package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/shero4/toolmux/internal/checker"
	"github.com/shero4/toolmux/internal/config"
	"github.com/shero4/toolmux/internal/control"
	"github.com/shero4/toolmux/internal/discovery"
	"github.com/shero4/toolmux/internal/execute"
	"github.com/shero4/toolmux/internal/gateway"
	"github.com/shero4/toolmux/internal/importer"
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
	if (len(os.Args) == 3 || len(os.Args) == 4) && os.Args[1] == "gws-call" {
		configDir := ""
		if len(os.Args) == 4 {
			configDir = os.Args[3]
		}
		if err := runGWS(os.Args[2], configDir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load()
	if err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	if len(os.Args) == 2 && os.Args[1] == "admin-token" {
		fmt.Println(control.Token(cfg.MasterKey))
		return
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
	// OAuth providers require a stable HTTPS callback. The operator UI may be
	// private, while the narrowly exposed gateway accepts the callback itself.
	oauthManager := oauth.New(db, cfg.GatewayURL)
	health := checker.New(db, executor, oauthManager, log)
	detector := discovery.New(cfg.DiscoveryRoots)
	hermesImporter := importer.New(db, detector, cfg.GatewayURL)
	ui, err := webui.New(db, health, oauthManager, detector, hermesImporter, cfg.BaseURL, log)
	if err != nil {
		log.Error("initialize web interface", "error", err)
		os.Exit(1)
	}
	ui.SetGatewayURL(cfg.GatewayURL)
	mcpHandler := mcp.NewHandler(db, executor, oauthManager, log)
	adminHandler := control.New(db, health, hermesImporter, control.Token(cfg.MasterKey))
	// Tool calls may legitimately run for up to an hour, so responses are not
	// given a fixed write deadline; each executor bounds its own work.
	server := &http.Server{Addr: cfg.Addr, Handler: ui.Handler(mcpHandler, adminHandler, gateway.New(db, log)), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	go ui.RunOperations(ctx)
	go ui.RunUpdates(ctx)
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

var gwsPart = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

type gwsRequest struct {
	Service      string          `json:"service"`
	Resource     string          `json:"resource"`
	SubResource  string          `json:"sub_resource"`
	SubResources []string        `json:"sub_resources"`
	Method       string          `json:"method"`
	Params       json.RawMessage `json:"params"`
	Body         json.RawMessage `json:"body"`
	PageAll      bool            `json:"page_all"`
	PageLimit    int             `json:"page_limit"`
}

func runGWS(executable, configDir string) error {
	var request gwsRequest
	decoder := json.NewDecoder(os.Stdin)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return fmt.Errorf("decode Google Workspace request: %w", err)
	}
	for label, value := range map[string]string{"service": request.Service, "resource": request.Resource, "method": request.Method} {
		if !gwsPart.MatchString(value) {
			return fmt.Errorf("%s must be a simple API name", label)
		}
	}
	if request.SubResource != "" && !gwsPart.MatchString(request.SubResource) {
		return errors.New("sub_resource must be a simple API name")
	}
	for _, value := range request.SubResources {
		if !gwsPart.MatchString(value) {
			return errors.New("sub_resources entries must be simple API names")
		}
	}
	arguments := []string{request.Service, request.Resource}
	if request.SubResource != "" {
		arguments = append(arguments, request.SubResource)
	}
	arguments = append(arguments, request.SubResources...)
	arguments = append(arguments, request.Method)
	if value, err := compactJSON(request.Params); err != nil {
		return fmt.Errorf("params: %w", err)
	} else if value != "" {
		arguments = append(arguments, "--params", value)
	}
	if value, err := compactJSON(request.Body); err != nil {
		return fmt.Errorf("body: %w", err)
	} else if value != "" {
		if gwsNeedsDirectUpload(request, value) {
			return runGWSGmailUpload(executable, configDir, request)
		}
		if len(value) > gwsArgvLimit {
			return fmt.Errorf("body is %d bytes; the Google Workspace CLI takes it as one argument and Linux caps that at 128 KiB", len(value))
		}
		arguments = append(arguments, "--json", value)
	}
	if request.PageAll {
		arguments = append(arguments, "--page-all")
	}
	if request.PageLimit > 0 {
		if request.PageLimit > 100 {
			return errors.New("page_limit cannot exceed 100")
		}
		arguments = append(arguments, "--page-limit", fmt.Sprint(request.PageLimit))
	}
	command := exec.Command(executable, arguments...)
	command.Env = os.Environ()
	if configDir != "" {
		command.Env = setProcessEnv(command.Env, "GOOGLE_WORKSPACE_CLI_CONFIG_DIR", configDir)
	}
	// Mirror the CLI's output to our own streams, but keep a bounded copy so a
	// failure can report WHY: the Google Workspace CLI writes its error JSON to
	// stdout and exits 1, and the outer command tool only forwards stderr, so
	// without this the agent just sees "exit status 1".
	var captured bytes.Buffer
	limited := &boundedWriter{limit: 4096, buf: &captured}
	command.Stdout = io.MultiWriter(os.Stdout, limited)
	command.Stderr = io.MultiWriter(os.Stderr, limited)
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(captured.String())
		if detail == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, detail)
	}
	return nil
}

// boundedWriter keeps the first “limit“ bytes written to it and drops the rest.
type boundedWriter struct {
	limit int
	buf   *bytes.Buffer
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	remaining := w.limit - w.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			w.buf.Write(p[:remaining])
		} else {
			w.buf.Write(p)
		}
	}
	return len(p), nil
}

func setProcessEnv(environment []string, name, value string) []string {
	prefix := name + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}

func compactJSON(value json.RawMessage) (string, error) {
	if len(value) == 0 || string(value) == "null" {
		return "", nil
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, value); err != nil {
		return "", err
	}
	return compact.String(), nil
}

func printSecrets() {
	master := make([]byte, 32)
	if _, err := rand.Read(master); err != nil {
		panic(err)
	}
	fmt.Printf("TOOLMUX_MASTER_KEY=%s\n", base64.StdEncoding.EncodeToString(master))
}
