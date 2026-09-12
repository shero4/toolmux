# Migrating Hermes profiles to a Toolmux host

This runbook moves existing Hermes profiles without changing their Hermes
version. Toolmux owns model and MCP credentials; Hermes keeps its local memory,
browser, terminal, cron, and messaging runtime.

## Host layout

Use one unprivileged `hermes` account and one isolated home per profile:

```text
/opt/hermes-agent/                  pinned Hermes checkout
/opt/hermes-venv/                   shared pinned runtime
/var/lib/hermes/profiles/<profile>/ profile state
/var/lib/hermes/browser/<profile>/  persistent Chromium data
/etc/hermes/profiles/<profile>.env  root-managed runtime secrets
```

Run each profile through `hermes-gateway@<profile>.service`. Install
`deploy/hermes-fleet-supervisor.py` and
`deploy/hermes-fleet-supervisor.service` to watch only enabled work-profile
units. A staged profile remains stopped until its unit is explicitly enabled.
The supervisor restarts inactive gateways immediately, and restarts a gateway
after two stale-heartbeat or disconnected-Slack checks. Its cooldown and restart
limit prevent a crash loop.

```bash
sudo install -m 0755 deploy/hermes-fleet-supervisor.py /usr/local/lib/
sudo install -m 0644 deploy/hermes-fleet-supervisor.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now hermes-fleet-supervisor.service
```

Set `HERMES_HOME`, `HERMES_CONFIG`, `CHROME_USER_DATA_DIR`, and the matching
environment file in the unit. Give each unit memory and task limits, and avoid
running many browser-heavy jobs at once on smaller hosts.

Give every profile its own persistent Chromium directory. On a small shared
host, use a locked selector that stops the current browser before opening the
requested profile, then point Computer Use at the shared private display. This
keeps browser-heavy jobs serialized while preserving separate cookies and
storage. A Windows Chrome directory cannot be copied to Linux as a working
signed-in profile because its secrets are protected by Windows; open each
remote profile through the private viewer and sign in once after migration.

Keep the desktop, VNC server, and browser viewer on the private network. Bind
the public Toolmux gateway to HTTPS separately. A profile on the same host uses
`http://127.0.0.1:8080`; remote clients use the public gateway URL.

## Profile configuration

Issue a different Toolmux token for each profile. Configure a single Toolmux
MCP server and point model traffic at Toolmux:

```yaml
model:
  provider: toolmux-codex
  default: codex-bridge/gpt-5.6-terra
  base_url: http://127.0.0.1:8080/v1
  api_mode: codex_responses

fallback_providers:
  - provider: toolmux-openai
    model: glm/glm-5.3-flash
    base_url: http://127.0.0.1:8080/v1
    api_mode: chat_completions

mcp_servers:
  toolmux:
    url: http://127.0.0.1:8080/mcp
    headers:
      Authorization: Bearer ${TOOLMUX_AGENT_TOKEN}
```

The agent sends the concrete model ID. Record the same primary and fallback on
its Toolmux agent page so catalog changes show affected agents. Connection
grants govern tools and do not affect model access.

Retain only the Toolmux token, the profile's messaging gateway credentials, and
credentials for capabilities that remain native to Hermes. Remove direct model
and MCP vendor credentials from the migrated copy.

## Copy and normalize state

1. Copy prompts, configuration, memory, skills, workspaces, scripts, hooks,
   plugins, browser data, and routing/session databases over encrypted SSH.
2. Use SQLite's backup API while the source is running. For the final delta,
   stop the source gateway and scheduler first, then take the backups.
3. Exclude logs, caches, downloads, request dumps, PID/lock files, generated
   model caches, and old Toolmux backups.
4. Recreate Windows junctions as Linux symlinks to one shared skills tree.
5. Rewrite drive-letter paths and check every symlink before starting Hermes.
6. Rewrite every enabled cron job's provider and model to the concrete Toolmux
   provider and model IDs. Port active helper scripts away from local vendor
   CLIs when Toolmux owns that connection.
7. Compare database integrity and important row counts on both hosts.

Use a SQLite runtime containing the WAL reset fix and FTS5. Confirm both before
starting services:

```bash
python - <<'PY'
import sqlite3
db = sqlite3.connect(':memory:')
print(sqlite3.sqlite_version)
print(db.execute("select sqlite_compileoption_used('ENABLE_FTS5')").fetchone()[0])
PY
```

## Preflight and cutover

Before stopping a local profile, verify with its Toolmux token:

- `/v1/models` returns concrete IDs and no wildcard entries.
- Primary inference and fallback inference both succeed.
- `tools/list` contains only its granted connections.
- Required OAuth connections are connected and refreshable.
- The remote profile can read its existing memory and session databases.
- Computer Use can capture and control the selected isolated browser profile.
- Every enabled cron helper runs on Linux and contains no source-machine path.

Cut over one profile at a time:

1. Stop the local Slack gateway and scheduler cleanly.
   Remove that profile from any local watchdog and disable its login/startup
   entry first; wait longer than the watchdog interval and verify the process
   does not return.
2. Copy the final database and file delta; compare checksums and counts.
3. Start the remote gateway and cron scheduler together.
4. Send a unique test request in its dedicated Slack channel.
5. Confirm exactly one reply with the expected identity and channel, one model
   call through Toolmux, and a harmless read/write tool check.
6. Observe the service and Toolmux Activity for at least 15 minutes before
   starting the next profile.

Preserve disabled jobs as disabled. Start intentionally deferred jobs only at
that profile's cutover.

## Rollback

Keep the local profile unchanged during the observation period. To roll back,
stop and disable its remote service, then restart the local gateway and
scheduler. Do not run both Slack gateways at once. Retain an encrypted rollback
archive until the operator explicitly approves its removal.
