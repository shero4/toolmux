package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The exchange directory is the only place files cross between Toolmux (user
// toolmux) and the Hermes agents (user hermes): Toolmux saves downloads there
// with group-readable permissions and reads uploads the agents drop there.
// Without it the Workspace CLI wrote Drive downloads into Toolmux's own
// working directory where no agent could reach them.
func exchangeDir() string {
	if value := os.Getenv("TOOLMUX_EXCHANGE_DIR"); value != "" {
		return value
	}
	return "/var/lib/hermes/shared/exchange"
}

// exchangePath accepts a bare file name, or a relative path without ".."
// so agents can keep per-task folders, and resolves it inside the exchange
// directory.
func exchangePath(name string) (string, error) {
	cleaned := filepath.Clean(strings.TrimSpace(name))
	if cleaned == "" || cleaned == "." || filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") {
		return "", errors.New("download_to/upload_file must be a file name relative to the exchange directory")
	}
	full := filepath.Join(exchangeDir(), cleaned)
	if err := os.MkdirAll(filepath.Dir(full), 0o775); err != nil {
		return "", fmt.Errorf("exchange directory: %w", err)
	}
	// MkdirAll honours the service umask (0077), which would leave folders
	// the agents cannot enter; open every folder below the exchange root.
	for dir := filepath.Dir(full); strings.HasPrefix(dir, exchangeDir()) && dir != exchangeDir(); dir = filepath.Dir(dir) {
		_ = os.Chmod(dir, 0o2775)
	}
	return full, nil
}

// runGWSGmailAttachmentDownload runs the CLI call (normally
// messages.attachments.get, which returns base64url in a "data" field),
// decodes the payload to the exchange file, and prints a small summary
// instead of megabytes of base64 that the agent could not use anyway.
func runGWSGmailAttachmentDownload(executable, configDir string, arguments []string, downloadPath string) error {
	command := exec.Command(executable, arguments...)
	command.Env = os.Environ()
	if configDir != "" {
		command.Env = setProcessEnv(command.Env, "GOOGLE_WORKSPACE_CLI_CONFIG_DIR", configDir)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, truncateText(strings.TrimSpace(stderr.String()+" "+stdout.String()), 2048))
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		return fmt.Errorf("download_to: the CLI response is not a JSON object: %w", err)
	}
	var encoded string
	for _, key := range []string{"data", "raw"} {
		if raw, ok := payload[key]; ok {
			_ = json.Unmarshal(raw, &encoded)
			break
		}
	}
	if encoded == "" {
		return errors.New("download_to: the response carries no base64 \"data\" or \"raw\" field (use it with messages.attachments.get or messages.get format=raw)")
	}
	decoded, err := decodeBase64Loose(encoded)
	if err != nil {
		return fmt.Errorf("download_to: %w", err)
	}
	if err := os.WriteFile(downloadPath, decoded, 0o644); err != nil {
		return fmt.Errorf("download_to: %w", err)
	}
	_ = os.Chmod(downloadPath, 0o644)
	summary := map[string]any{"status": "success", "saved_file": downloadPath, "bytes": len(decoded)}
	for key, raw := range payload {
		if key != "data" && key != "raw" {
			summary[key] = raw
		}
	}
	return json.NewEncoder(os.Stdout).Encode(summary)
}
