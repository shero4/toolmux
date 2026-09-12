package updater

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// Revision is injected by the reproducible build.
var Revision string
var sha = regexp.MustCompile(`^[a-f0-9]{40}$`)

const repository = "https://api.github.com/repos/shero4/toolmux/commits/main"
const stateFile = "/var/lib/toolmux-updater/status.json"

type State struct {
	Current   string    `json:"current"`
	Latest    string    `json:"latest"`
	Available bool      `json:"available"`
	Enabled   bool      `json:"enabled"`
	Checked   time.Time `json:"checked"`
	Error     string    `json:"error,omitempty"`
	Phase     string    `json:"phase,omitempty"`
	Message   string    `json:"message,omitempty"`
}
type Manager struct {
	mu       sync.Mutex
	state    State
	client   *http.Client
	endpoint string
	checking bool
	pending  time.Time
}

func New() *Manager {
	revision := Revision
	if revision == "" {
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, item := range info.Settings {
				if item.Key == "vcs.revision" {
					revision = item.Value
				}
				if item.Key == "vcs.modified" && item.Value == "true" {
					return &Manager{state: State{Current: "development"}, client: &http.Client{Timeout: 10 * time.Second}, endpoint: repository}
				}
			}
		}
	}
	return &Manager{state: State{Current: revision, Enabled: os.Getenv("TOOLMUX_UPDATES_ENABLED") == "true" && sha.MatchString(revision)}, client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}, endpoint: repository}
}
func (m *Manager) View() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.state
	if s.Enabled {
		var job struct {
			Phase   string `json:"phase"`
			Message string `json:"message"`
		}
		if data, err := os.ReadFile(stateFile); err == nil && len(data) < 8192 {
			if json.Unmarshal(data, &job) == nil {
				s.Phase = job.Phase
				s.Message = job.Message
				if info, err := os.Stat(stateFile); err == nil && s.Phase == "running" && time.Since(info.ModTime()) > 46*time.Minute {
					s.Phase = "failed"
					s.Message = "The update timed out. Check the host update log before retrying."
				}
			}
		}
		if !m.pending.IsZero() && time.Since(m.pending) < 30*time.Second && s.Phase != "running" {
			s.Phase = "running"
			s.Message = "Starting update"
		}
	}
	return s
}
func (m *Manager) Check(ctx context.Context) {
	m.mu.Lock()
	if m.checking || time.Since(m.state.Checked) < time.Minute {
		m.mu.Unlock()
		return
	}
	m.checking = true
	m.mu.Unlock()
	req, _ := http.NewRequestWithContext(ctx, "GET", m.endpoint, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Toolmux-update-check")
	response, err := m.client.Do(req)
	var latest string
	if err == nil {
		defer response.Body.Close()
		if response.StatusCode != 200 {
			err = errors.New("GitHub unavailable")
		} else {
			var value struct {
				SHA string `json:"sha"`
			}
			err = json.NewDecoder(io.LimitReader(response.Body, 65536)).Decode(&value)
			latest = value.SHA
			if !sha.MatchString(latest) {
				err = errors.New("invalid revision")
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.checking = false
	m.state.Checked = time.Now()
	m.state.Error = ""
	if err != nil {
		m.state.Error = "Could not check GitHub. Toolmux will retry automatically."
		return
	}
	m.state.Latest = latest
	m.state.Available = sha.MatchString(m.state.Current) && latest != m.state.Current
}
func (m *Manager) Run(ctx context.Context) {
	m.Check(ctx)
	t := time.NewTicker(15 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Check(ctx)
		}
	}
}
func (m *Manager) Install(ctx context.Context, expected string) error {
	s := m.View()
	if !s.Enabled || !s.Available || expected != s.Latest || !sha.MatchString(expected) || s.Phase == "running" {
		return errors.New("No installable update is available, or an update is already running.")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.pending.IsZero() && time.Since(m.pending) < time.Minute {
		return errors.New("An update is already starting.")
	}
	// The privileged helper accepts no command, path, or repository from the request.
	if err := os.WriteFile("/var/lib/toolmux/update-request", []byte(strings.TrimSpace(expected)), 0600); err != nil {
		return errors.New("Update helper is not configured.")
	}
	cmd := exec.CommandContext(ctx, "sudo", "-n", "/usr/bin/systemctl", "start", "--no-block", "toolmux-update.service")
	if cmd.Run() != nil {
		return errors.New("Could not start the update service. Check the host setup.")
	}
	m.pending = time.Now()
	return nil
}
