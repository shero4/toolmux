package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Candidate is an agent installation found on the host. ID is a stable hash
// of runtime, environment, profile, and configuration path.
type Candidate struct {
	ID, Runtime, Profile, Name, Environment, ConfigPath string
}

const (
	localEnvironment = "This computer"
	mountEnvironment = "Mounted host"
	wslPrefix        = "WSL · "
)

// InWSL reports whether the candidate lives inside a WSL distribution, which a
// native Windows process can list but cannot open or run.
func (c Candidate) InWSL() bool {
	return strings.HasPrefix(c.Environment, wslPrefix)
}

// Identity is a human-readable, slug-friendly identity that stays unique when
// the same profile name exists in more than one environment.
func (c Candidate) Identity() string {
	if c.Environment == localEnvironment || c.Environment == mountEnvironment {
		return c.Runtime + "-" + c.Profile
	}
	return c.Runtime + "-" + strings.TrimPrefix(c.Environment, wslPrefix) + "-" + c.Profile
}

type Scanner struct {
	roots []string
}

func New(roots []string) *Scanner {
	return &Scanner{roots: roots}
}

func (s *Scanner) Scan(ctx context.Context) []Candidate {
	seen := make(map[string]Candidate)
	if home, err := os.UserHomeDir(); err == nil {
		s.scanHome(home, "This computer", seen)
	}
	for _, root := range s.roots {
		s.scanHome(root, "Mounted host", seen)
	}
	if base := strings.TrimSpace(os.Getenv("HERMES_HOME")); base != "" {
		s.scanHermes(base, "This computer", seen)
	}
	if state := strings.TrimSpace(os.Getenv("OPENCLAW_STATE_DIR")); state != "" {
		s.scanOpenClaw(state, profileFromState(state), "This computer", seen)
	}
	if runtime.GOOS == "windows" {
		s.scanWSL(ctx, seen)
	}
	result := make([]Candidate, 0, len(seen))
	for _, candidate := range seen {
		result = append(result, candidate)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Runtime != result[j].Runtime {
			return result[i].Runtime < result[j].Runtime
		}
		if result[i].Environment != result[j].Environment {
			return result[i].Environment < result[j].Environment
		}
		return result[i].Name < result[j].Name
	})
	return result
}

func (s *Scanner) Find(ctx context.Context, id string) (Candidate, bool) {
	for _, candidate := range s.Scan(ctx) {
		if candidate.ID == id {
			return candidate, true
		}
	}
	return Candidate{}, false
}

func (s *Scanner) scanHome(home, environment string, seen map[string]Candidate) {
	home = filepath.Clean(home)
	s.scanHermes(filepath.Join(home, ".hermes"), environment, seen)
	s.scanHermes(filepath.Join(home, "AppData", "Local", "hermes"), environment, seen)
	s.scanOpenClaw(filepath.Join(home, ".openclaw"), "default", environment, seen)
	if p := filepath.Join(home, ".codex", "config.toml"); isFile(p) {
		add(seen, "codex", "default", "Codex · default", environment, p)
	}
	if p := filepath.Join(home, ".claude.json"); isFile(p) {
		add(seen, "claude", "default", "Claude Code · default", environment, p)
	}
	if p := filepath.Join(home, ".mcp.json"); isFile(p) {
		add(seen, "claude", "project", "Claude Code · project", environment, p)
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".openclaw-*"))
	for _, state := range matches {
		s.scanOpenClaw(state, profileFromState(state), environment, seen)
	}
}

func (s *Scanner) scanHermes(base, environment string, seen map[string]Candidate) {
	profilePath := filepath.Join(base, "profile.yaml")
	activeProfile := strings.TrimSpace(readText(filepath.Join(base, "active_profile")))
	_, description := yamlMetadata(profilePath)
	hiddenDefault := activeProfile != "" && !strings.EqualFold(activeProfile, "default") || strings.Contains(strings.ToLower(description), "hidden default system profile")
	if !hiddenDefault && (isFile(filepath.Join(base, "config.yaml")) || isFile(profilePath)) {
		name, _ := yamlMetadata(profilePath)
		if name == "" {
			name = "Hermes · default"
		}
		add(seen, "hermes", "default", name, environment, filepath.Join(base, "config.yaml"))
	}
	profiles, _ := os.ReadDir(filepath.Join(base, "profiles"))
	for _, entry := range profiles {
		if !entry.IsDir() {
			continue
		}
		profileDir := filepath.Join(base, "profiles", entry.Name())
		if !isFile(filepath.Join(profileDir, "config.yaml")) && !isFile(filepath.Join(profileDir, "profile.yaml")) {
			continue
		}
		name, _ := yamlMetadata(filepath.Join(profileDir, "profile.yaml"))
		if name == "" {
			name = "Hermes · " + entry.Name()
		}
		add(seen, "hermes", entry.Name(), name, environment, filepath.Join(profileDir, "config.yaml"))
	}
}

func (s *Scanner) scanOpenClaw(state, profile, environment string, seen map[string]Candidate) {
	configPath := filepath.Join(state, "openclaw.json")
	agents := openClawConfigAgents(configPath)
	agentDirs, _ := os.ReadDir(filepath.Join(state, "agents"))
	for _, entry := range agentDirs {
		if entry.IsDir() && isDir(filepath.Join(state, "agents", entry.Name(), "agent")) {
			agents[entry.Name()] = ""
		}
	}
	if len(agents) == 0 && isFile(configPath) {
		agents["main"] = ""
	}
	for id, configuredName := range agents {
		name := configuredName
		if name == "" {
			name = "OpenClaw · " + id
		}
		if profile != "default" {
			name += " (" + profile + ")"
		}
		add(seen, "openclaw", profile+":"+id, name, environment, configPath)
	}
}

func openClawConfigAgents(path string) map[string]string {
	result := make(map[string]string)
	data, err := os.ReadFile(path)
	if err != nil {
		return result
	}
	var config struct {
		Agents struct {
			List    []struct{ ID, Name string }
			Entries []struct{ ID, Name string }
		}
	}
	if json.Unmarshal(data, &config) != nil {
		return result
	}
	for _, agent := range append(config.Agents.List, config.Agents.Entries...) {
		if id := strings.TrimSpace(agent.ID); id != "" {
			result[id] = strings.TrimSpace(agent.Name)
		}
	}
	return result
}

func (s *Scanner) scanWSL(ctx context.Context, seen map[string]Candidate) {
	listCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	output, err := exec.CommandContext(listCtx, "wsl.exe", "--list", "--quiet").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(output), "\x00", ""), "\n") {
		distro := strings.TrimSpace(line)
		if distro == "" || strings.EqualFold(distro, "docker-desktop") {
			continue
		}
		s.scanWSLDistro(ctx, distro, seen)
	}
}

func (s *Scanner) scanWSLDistro(ctx context.Context, distro string, seen map[string]Candidate) {
	const script = `
home=$HOME
active=default; [ -f "$home/.hermes/active_profile" ] && active=$(cat "$home/.hermes/active_profile")
if [ "$active" = default ] && { [ -f "$home/.hermes/config.yaml" ] || [ -f "$home/.hermes/profile.yaml" ]; }; then printf 'hermes\tdefault\tHermes - default\t%s\n' "$home/.hermes/config.yaml"; fi
for p in "$home/.hermes/profiles"/*; do [ -d "$p" ] || continue; if [ -f "$p/config.yaml" ] || [ -f "$p/profile.yaml" ]; then n=${p##*/}; printf 'hermes\t%s\tHermes - %s\t%s\n' "$n" "$n" "$p/config.yaml"; fi; done
for state in "$home/.openclaw" "$home"/.openclaw-*; do [ -d "$state" ] || continue; profile=${state##*/}; profile=${profile#.openclaw}; profile=${profile#-}; [ -n "$profile" ] || profile=default; found=0; for a in "$state/agents"/*/agent; do [ -d "$a" ] || continue; id=$(basename "$(dirname "$a")"); printf 'openclaw\t%s:%s\tOpenClaw - %s\t%s\n' "$profile" "$id" "$id" "$state/openclaw.json"; found=1; done; if [ "$found" = 0 ] && [ -f "$state/openclaw.json" ]; then printf 'openclaw\t%s:main\tOpenClaw - main\t%s\n' "$profile" "$state/openclaw.json"; fi; done
`
	scanCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(scanCtx, "wsl.exe", "-d", distro, "--", "sh", "-lc", script).Output()
	if err != nil {
		return
	}
	environment := "WSL · " + distro
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 4 {
			continue
		}
		add(seen, fields[0], fields[1], strings.ReplaceAll(fields[2], " - ", " · "), environment, fields[3])
	}
}

func add(seen map[string]Candidate, runtimeName, profile, name, environment, configPath string) {
	source := runtimeName + "\x00" + environment + "\x00" + profile + "\x00" + configPath
	sum := sha256.Sum256([]byte(source))
	id := hex.EncodeToString(sum[:12])
	seen[id] = Candidate{ID: id, Runtime: runtimeName, Profile: profile, Name: name, Environment: environment, ConfigPath: configPath}
}

func yamlMetadata(path string) (name, description string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimLeft(line, " \t") != line {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		switch key {
		case "name":
			name = strings.Trim(strings.TrimSpace(value), `"'`)
		case "description":
			description = strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return name, description
}

func readText(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

func profileFromState(path string) string {
	name := filepath.Base(filepath.Clean(path))
	if name == ".openclaw" {
		return "default"
	}
	if profile := strings.TrimPrefix(name, ".openclaw-"); profile != name && profile != "" {
		return profile
	}
	return name
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
