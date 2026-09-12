package operations

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"time"
)

type CodexState struct {
	Status    string    `json:"status"`
	Code      string    `json:"code,omitempty"`
	URL       string    `json:"url,omitempty"`
	ExpiresAt int64     `json:"expires_at,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
	Running   bool      `json:"running"`
}
type Codex struct {
	mu        sync.Mutex
	work      sync.Mutex
	state     CodexState
	container string
}

func NewCodex() *Codex {
	container := os.Getenv("TOOLMUX_CODEX_CONTAINER")
	if container == "" {
		container = "toolmux-models-1"
	}
	return &Codex{container: container, state: CodexState{Status: "unknown"}}
}
func (c *Codex) View() CodexState { c.mu.Lock(); defer c.mu.Unlock(); return c.state }
func (c *Codex) command(ctx context.Context, script string) *exec.Cmd {
	return exec.CommandContext(ctx, "docker", "exec", c.container, "python", "-u", "-c", script)
}

const statusScript = `import json,time
import litellm
litellm.request_timeout=20
from litellm.llms.chatgpt.authenticator import Authenticator
a=Authenticator()
d=a._read_auth_file() or {}
status='reauthorization_required'
try:
 if d.get('access_token'):
  expired=a._is_token_expired(d,d['access_token'])
  expiry=d.get('expires_at') or a._get_expires_at(d['access_token']) or 0
  if (expired or expiry-time.time()<300) and d.get('refresh_token'):
   a._refresh_tokens(d['refresh_token'])
   d=a._read_auth_file() or {}
   expired=a._is_token_expired(d,d.get('access_token',''))
  if not expired: status='connected'
except Exception:
 status='reauthorization_required'
print(json.dumps({'status':status,'expires_at':int(d.get('expires_at') or 0)}))
`

func (c *Codex) Check(ctx context.Context) CodexState {
	if !c.work.TryLock() {
		return c.View()
	}
	defer c.work.Unlock()
	c.mu.Lock()
	if c.state.Running {
		v := c.state
		c.mu.Unlock()
		return v
	}
	c.mu.Unlock()
	check, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	data, err := c.command(check, statusScript).Output()
	v := CodexState{Status: "unavailable", CheckedAt: time.Now()}
	if err == nil {
		if json.Unmarshal(data, &v) != nil {
			v.Status = "unavailable"
		}
		v.CheckedAt = time.Now()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.state.Running {
		c.state = v
	}
	return c.state
}

const loginScript = `import json,time
import litellm
litellm.request_timeout=20
from litellm.llms.chatgpt.authenticator import Authenticator,CHATGPT_DEVICE_VERIFY_URL
a=Authenticator()
try:
 d=a._read_auth_file() or {}
 if d.get('access_token') and not a._is_token_expired(d,d['access_token']):
  print(json.dumps({'status':'connected'}),flush=True)
 else:
  device=a._request_device_code()
  a._record_device_code_request()
  print(json.dumps({'status':'awaiting_signin','code':device['user_code'],'url':CHATGPT_DEVICE_VERIFY_URL,'expires_at':int(time.time())+900}),flush=True)
  authorization=a._poll_for_authorization_code(device)
  tokens=a._exchange_code_for_tokens(authorization)
  a._write_auth_file(a._build_auth_record(tokens))
  print(json.dumps({'status':'connected'}),flush=True)
except Exception:
 print(json.dumps({'status':'reauthorization_required'}),flush=True)
 raise SystemExit(1)
`

func (c *Codex) Start(onDone func(string)) error {
	if !c.work.TryLock() {
		return errors.New("authorization check or sign-in is running")
	}
	c.mu.Lock()
	if c.state.Running {
		c.work.Unlock()
		c.mu.Unlock()
		return errors.New("sign-in is already running")
	}
	c.state = CodexState{Status: "starting", Running: true, CheckedAt: time.Now()}
	c.mu.Unlock()
	go func() {
		defer c.work.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 16*time.Minute)
		defer cancel()
		cmd := c.command(ctx, loginScript)
		out, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		connected := false
		if err == nil {
			scan := bufio.NewScanner(out)
			scan.Buffer(make([]byte, 4096), 8192)
			for scan.Scan() {
				var v CodexState
				if json.Unmarshal(scan.Bytes(), &v) != nil {
					continue
				}
				if v.Status != "awaiting_signin" && v.Status != "connected" && v.Status != "reauthorization_required" {
					continue
				}
				if v.Status == "awaiting_signin" && (!regexp.MustCompile(`^[A-Z0-9-]{4,32}$`).MatchString(v.Code) || v.URL != "https://auth.openai.com/codex/device") {
					continue
				}
				v.Running = true
				v.CheckedAt = time.Now()
				c.mu.Lock()
				c.state = v
				c.mu.Unlock()
				connected = v.Status == "connected"
			}
			err = cmd.Wait()
		}
		status := "reauthorization_required"
		if err == nil && connected {
			restart, stop := context.WithTimeout(context.Background(), 45*time.Second)
			err = exec.CommandContext(restart, "docker", "restart", c.container).Run()
			stop()
			if err == nil {
				status = "connected"
			} else {
				status = "unavailable"
			}
		}
		c.mu.Lock()
		c.state = CodexState{Status: status, CheckedAt: time.Now()}
		c.mu.Unlock()
		onDone(status)
	}()
	return nil
}
