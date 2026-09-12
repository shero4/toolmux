package operations

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

const modelsScript = `import base64,json,time,urllib.request
from litellm.llms.chatgpt.authenticator import Authenticator
a=Authenticator(); d=a._read_auth_file() or {}
token=d.get('access_token','')
if token and (a._is_token_expired(d,token) or (d.get('expires_at') or 0)-time.time()<300) and d.get('refresh_token'):
 a._refresh_tokens(d['refresh_token']); d=a._read_auth_file() or {}; token=d.get('access_token','')
if not token or a._is_token_expired(d,token):
 print(json.dumps({'error':'Codex authorization is required'})); raise SystemExit(2)
headers={'Authorization':'Bearer '+token,'User-Agent':'toolmux/1.0'}
try:
 parts=token.split('.')
 payload=json.loads(base64.urlsafe_b64decode(parts[1]+'='*(-len(parts[1])%4)))
 account=(payload.get('https://api.openai.com/auth') or {}).get('chatgpt_account_id')
 if account: headers['ChatGPT-Account-Id']=account
except Exception: pass
req=urllib.request.Request('https://chatgpt.com/backend-api/codex/models?client_version=1.0.0',headers=headers)
try:
 with urllib.request.urlopen(req,timeout=15) as response: data=json.load(response)
except Exception as exc:
 print(json.dumps({'error':'Codex model discovery failed: '+str(exc)})); raise SystemExit(3)
models=[]
for item in data.get('models',[]):
 if not isinstance(item,dict): continue
 slug=item.get('slug','').strip() if isinstance(item.get('slug'),str) else ''
 visibility=item.get('visibility','').strip().lower() if isinstance(item.get('visibility'),str) else ''
 if slug and visibility not in ('hide','hidden') and '*' not in slug and len(slug)<=256: models.append(slug)
print(json.dumps({'models':list(dict.fromkeys(models))}))
`

// Models returns the concrete model IDs visible to the authenticated Codex
// subscription. The access and refresh tokens never leave the bridge volume.
func (c *Codex) Models(ctx context.Context) ([]string, error) {
	if !c.work.TryLock() {
		return nil, errors.New("Codex authorization is busy; try again shortly")
	}
	defer c.work.Unlock()
	check, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	data, err := c.command(check, modelsScript).CombinedOutput()
	var result struct {
		Models []string `json:"models"`
		Error  string   `json:"error"`
	}
	if json.Unmarshal(data, &result) != nil {
		if err != nil {
			return nil, fmt.Errorf("Codex model discovery failed")
		}
		return nil, errors.New("Codex returned an invalid model catalog")
	}
	if result.Error != "" {
		return nil, errors.New(result.Error)
	}
	if err != nil {
		return nil, fmt.Errorf("Codex model discovery failed")
	}
	if len(result.Models) == 0 {
		return nil, errors.New("the signed-in Codex account returned no models")
	}
	return result.Models, nil
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
