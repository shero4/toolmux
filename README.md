# 🪼 Toolmux

Toolmux is a small, self-hosted tool access broker for agents. Connect an MCP
server, HTTP API, or installed CLI once, issue each agent its own token, and
explicitly grant the tools that agent may discover and call. Every capability is
presented to agents through one MCP endpoint.

Toolmux deliberately does not route models, run agents, manage prompts, act as
an AI gateway, or sandbox its host. The machine or container is trusted. The
security boundary is the agent-facing MCP endpoint and its per-agent grants.

## What works

- One Streamable HTTP endpoint at `/mcp` for every agent
- MCP 2026-07-28 stateless discovery plus handshake-era compatibility through
  2025-11-25
- Hashed, individually revocable agent tokens
- Remote Streamable HTTP MCP connections with automatic tool discovery
- Local stdio MCP processes with imported arguments and environment
- Declarative HTTP API tools with typed input schemas and safe URL/body templates
- Declarative CLI tools that use the host's installed binaries, environment, and
  optional working directory
- No-auth, bearer-token, and OAuth 2.0 upstream authentication
- Authorization Code + PKCE, encrypted refresh tokens, and refresh before expiry
- Tool discovery with stable `connection__tool` names
- Per-agent connection assignments and individual tool grants, enforced on
  discovery and invocation
- Read-only discovery of Hermes and OpenClaw agents on the host and in WSL
- One-click Hermes import for profiles, MCP instances, OAuth state, API keys,
  and local `gws-*` identities
- Encrypted upstream credentials
- Active reachability, protocol, authorization, and capability checks
- Append-only audit events
- A server-rendered administration interface
- daisyUI components compiled to one embedded CSS asset; no browser framework
- PostgreSQL migrations applied atomically at startup

## Run locally

Build the image, then generate the required encryption key:

```sh
docker compose build
docker compose run --rm --no-deps toolmux keygen
```

Copy `.env.example` to `.env`, replace the key, then run:

```sh
docker compose up --build
```

Open `http://localhost:8080`. Toolmux assumes its host and network are trusted,
so the administration interface has no separate login. When exposing it from a
remote machine, keep it on a private network or place it behind your existing
reverse proxy authentication.

### Discover installed agents

When Toolmux runs directly on a computer, the **Discover agents** button scans
the current user's standard Hermes and OpenClaw configuration locations. On
Windows it also inspects WSL distributions. Detection is read-only and ignores
directories that do not contain an agent configuration.

A container cannot see host files unless they are mounted. Set
`TOOLMUX_HOST_HOME` in `.env`, then include the small discovery override:

```sh
docker compose -f compose.yaml -f compose.discovery.yaml up --build
```

The host home is mounted read-only. Toolmux looks for Hermes' active default
profile and named profiles below its standard data directory, plus OpenClaw agents
below `.openclaw` and named `.openclaw-*` state directories. You can instead set
`TOOLMUX_DISCOVERY_ROOTS` to an OS path-list when running the binary directly.
Hermes' hidden system profile and its group-chat metadata are not imported as
agent identities.

### Import Hermes

Run Toolmux directly on the host when you want to reuse host-installed stdio
MCPs or CLIs. On **Connections**, choose **Import Hermes**. The import is
idempotent and:

- creates one Toolmux agent for every Hermes profile;
- keeps separate instances when the same MCP is configured in multiple profiles;
- imports remote MCP, stdio MCP, header, bearer, and reusable OAuth state;
- imports every installed `gws-*` identity as a separate Google Workspace
  connection and makes those connections available to all imported profiles;
- writes each profile's own Toolmux URL and token into its `mcp_servers` map;
- preserves the original YAML beside it as `config.yaml.toolmux.bak`.

Existing upstream MCP entries remain in place during the test period. Remove
them after you are satisfied that calls are flowing through Toolmux. If an
imported credential is expired, the Connections page shows **Authorize** for
OAuth or **Replace credential** for a bearer/API key connection.

## Connect an agent

Create an agent in the web interface and copy its token when shown. Configure
Hermes, OpenClaw, or another MCP client with:

```text
URL: http://localhost:8080/mcp
Authorization: Bearer <agent token>
```

Tokens are displayed once. Toolmux stores only their SHA-256 hashes.

No prompt text or copied tool catalog is required. The MCP client calls
`tools/list`, and Toolmux returns only that token's current grants. Assigning a
whole connection keeps newly discovered capabilities in sync automatically.
Issue another token from the agent page when the same agent identity runs in a
second place.

### Codex

Put the token in an environment variable, then add this to `config.toml`:

```toml
[mcp_servers.toolmux]
url = "http://localhost:8080/mcp"
bearer_token_env_var = "TOOLMUX_AGENT_TOKEN"
required = true
```

### Claude Code

```sh
claude mcp add --transport http toolmux http://localhost:8080/mcp \
  --header "Authorization: Bearer $TOOLMUX_AGENT_TOKEN"
```

### Generic clients

Use Streamable HTTP with the Toolmux URL and
`Authorization: Bearer <agent token>`. Toolmux supports both the current
2026-07-28 stateless protocol and older clients that use
`initialize`/`notifications/initialized`.

## Let an agent configure Toolmux

Toolmux keeps runtime access and administrative access separate:

- `/mcp` uses an agent token and can only discover or call assigned capabilities.
- `/admin/mcp` uses the installation's admin token and can create agents, issue
  their runtime tokens, assign connections, check health, and run the Hermes
  import.

Print the deterministic admin token from the same installation key:

```sh
toolmux admin-token
```

Connect a trusted setup agent to `http://localhost:8080/admin/mcp` with that
token. It discovers the management operations through MCP like any other tool
server. Do not put the admin token in ordinary agent profiles. The web interface
uses the same store operations, so connection assignments and token issuance
behave identically from the UI and the control endpoint.

Imported Hermes profiles are configured automatically by the dedicated import
flow. The generic discovery flow remains read-only and shows setup instructions
for Hermes and OpenClaw.

For an OAuth connection, register this redirect URL with the provider:

```text
http://localhost:8080/oauth/callback
```

Use the public `TOOLMUX_BASE_URL` instead of localhost when Toolmux is behind
TLS. Toolmux uses Authorization Code with PKCE and refreshes access tokens one
minute before expiry. When an imported MCP publishes a dynamic
client-registration endpoint, Toolmux registers its own callback client before
starting authorization. Otherwise, supply a client ID and, when required, a
client secret.

## Expose an HTTP API

Create an `HTTP API` connection with the service's base URL and authorization.
Then define each permitted operation on the Tools page. Paths, query values,
headers, and JSON bodies may reference top-level inputs as `${input_name}`.
An operation may use a relative path or a complete HTTP URL. Authorization can
be a bearer token, OAuth 2.0 token, or any named credential header.

## Expose a CLI

Create an `Installed command` connection, then define a tool with an executable
and a JSON array of arguments. Arguments may reference top-level inputs as
`${input_name}`. Toolmux calls the executable directly, inherits the host
environment so existing CLI authorization keeps working, and supports an
optional working directory. The stock image is intentionally minimal; extend it
with the CLI binaries you use, or run the binary directly on a configured host.
Per-tool timeouts prevent accidental hangs and may be set up to one hour.

## Design constraints

- The service fails closed when identity, grants, or credentials cannot be read.
- Agents never receive upstream credentials.
- Unauthorized tools are absent from `tools/list` and rejected by `tools/call`.
- Connection checks discover MCP tools, send `HEAD` to an API health path, or
  verify that a declarative command executable exists. A stdio MCP is started
  long enough to initialize and list its tools.
- The default deployment is one application container and one PostgreSQL
  container. There is no queue, cache, policy engine, or browser application
  runtime.
- Tailwind and daisyUI run only in the image build stage. Node is not present in
  the production image and the interface remains server-rendered.
- Connection health does not revoke grants. A configured agent keeps its access;
  health status explains why an upstream may currently be unavailable.
- Import assignments are durable. Tools discovered later on an assigned
  connection are granted to the same agents automatically.
- Tool listing is deterministic and paginated. Tool definitions retain titles,
  schemas, annotations, and icons; tool results pass through text, images, audio,
  resource links, embedded resources, structured content, errors, and modern
  multi-round-trip input requests without payload rewriting.

See [DESIGN.md](DESIGN.md) for the architecture and data model.
