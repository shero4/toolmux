# 🪼 Toolmux

Toolmux is a small, self-hosted tool access broker for agents. Connect an MCP
server, HTTP API, or installed CLI once, issue each agent its own token, and
explicitly grant the tools that agent may discover and call. Every capability is
presented to agents through one MCP endpoint.

Toolmux also exposes a shared inference proxy: configure model providers once,
then let each agent select a `provider/model` through an OpenAI-compatible
endpoint. Toolmux does not run agents, choose their models, manage prompts, or
sandbox its host. The machine or container remains trusted.

## What works

- Multiple users with Administrator, Operator, and Viewer roles; first-run
  administrator creation, persistent password sign-in, revocable
  12-hour browser sessions, sign-out, and password changes
- A Settings page for account management and installation/client configuration
- Encrypted model-provider keys, manual and discovered model catalogs, and
  pause/resume through the provider form
- Shared `/v1/models`, `/v1/chat/completions`, `/v1/responses`,
  `/v1/completions`, and `/v1/embeddings` endpoints, including streaming
- Model-call activity with provider, selected model, outcome, duration, and
  reported input/output tokens; no prompt or completion storage

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
- Append-only audit events with a 24-hour summary on the overview
- Connection pages with tool catalog, agents with access, check history,
  credential replacement, pause/resume, and delete
- Agent pages with tokens, whole-connection assignment, per-tool grants that
  save immediately, and disable/enable/delete
- Tool pages showing the input schema, typed specification, and grants
- One-time token reveal that never places the token in a URL
- A server-rendered administration interface: Go templates, one stylesheet,
  one small script, no Node build step or browser framework
- PostgreSQL migrations applied atomically at startup

## Native EC2 host for Hermes

For the host deployment supporting local agent configuration and Codex bridge
controls, follow [Native EC2 installation](deploy/NATIVE.md). The reproducible
infrastructure template is `deploy/ec2-host.yaml`; the installer is
`deploy/install-host.sh`. The template defaults to 4 vCPUs and 16 GiB; size it for your workload.
Agent migration is separate from installing Toolmux.
Keep instance details, VPN topology and deployment records outside version control.

For public agent access with VPN-only administration, follow the
[split gateway deployment](deploy/GATEWAY.md). It uses standard HTTPS for MCP and
inference while keeping the web UI on private port 8080.

## Deployment and network interfaces

See [EC2 deployment](deploy/EC2.md) for a private first-run setup, optional HTTPS,
interface selection, persistent storage, backups and upgrade verification.
The remote configuration is `compose.ec2.yaml`; `compose.yaml` is for development.

| Setting | Meaning | Default |
| --- | --- | --- |
| `TOOLMUX_ADDR` | Native server listening address, including port | `127.0.0.1:8080` |
| `TOOLMUX_BIND_IP` | Docker host interface publishing the application | `127.0.0.1` |
| `TOOLMUX_PORT` | Docker host application port | `8080` |
| `TOOLMUX_GATEWAY_URL` | Advertised agent URL; defaults to the administration URL | unset |
| `TOOLMUX_BASE_URL` | Administration URL, OAuth callbacks and cookie security | `http://localhost:8080` locally |

Use `0.0.0.0` for all IPv4 interfaces, or an IP assigned to the host for one
interface. For native mode include the port: `TOOLMUX_ADDR=0.0.0.0:8080`.
For Docker set `TOOLMUX_BIND_IP=0.0.0.0`; the process still listens on port 8080
inside the container. A bind address is not a client URL: never use `0.0.0.0`
as the public base URL. Changing the base URL does not change the listener.
Restart/recreate the server after changing deployment settings.

## Run locally

Build the image independently of Compose, then generate the required encryption key:

```sh
docker build -t toolmux:local .
docker run --rm toolmux:local keygen
```

Copy `.env.example` to `.env`, replace the key, then run:

```sh
docker compose up --build
```

Open `http://localhost:8080` and create your administrator username and password.
The account is stored in PostgreSQL and survives restarts. Complete first-run
setup on a private host before making the service reachable remotely. Use TLS
and set `TOOLMUX_BASE_URL` to the HTTPS URL for Secure session cookies.
Existing installations show setup once after upgrading; agents keep using
their existing bearer tokens independently of browser login.

To run the binary directly instead, point it at a PostgreSQL database with the
same environment variables (a local `.env` file is read as a fallback):

```sh
go run ./cmd/toolmux
```

### Discover installed agents

When Toolmux runs directly on a computer, **Agents → Discover installed** scans
the current user's standard Hermes and OpenClaw configuration locations. On
Windows it also lists profiles inside WSL distributions. Detection is read-only
and ignores directories that do not contain an agent configuration.

Hermes profiles are imported in full (see below), one at a time or all at once.
OpenClaw agents are added as identities with a token and the server entry to
paste into OpenClaw. Profiles that live inside WSL are shown but can only be
imported by running Toolmux inside that distribution, because their local MCP
servers and files are not reachable from a Windows process.

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
MCPs or CLIs. On **Agents → Discover installed**, choose **Import** on a
profile or **Import all Hermes profiles**. The import is idempotent and:

- creates one Toolmux agent for every Hermes profile;
- keeps separate instances when the same MCP is configured in multiple profiles;
- imports remote MCP, stdio MCP, header, bearer, and reusable OAuth state;
- imports every installed `gws-*` identity as a separate Google Workspace
  connection and makes those connections available to all imported profiles;
- writes each profile's own Toolmux URL and token into its `mcp_servers` map;
- preserves the original YAML beside it as `config.yaml.toolmux.bak`.

Existing upstream MCP entries remain in place during the test period. Remove
them after you are satisfied that calls are flowing through Toolmux. If an
imported credential is expired, the connection page offers **Authorize** for
OAuth or **Replace and check** for a bearer/API key connection.

Re-running the import never downgrades state Toolmux now owns: an OAuth client
that Toolmux registered for its own callback is kept, an older token from
Hermes never replaces a newer one, and a server whose endpoint changed gets its
own connector instead of rewriting the shared one.

## Connect an agent

Create an agent in the web interface and copy its token when shown; it is
revealed once and never placed in a URL. Configure Hermes, OpenClaw, or another
MCP client with:

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
  runtime. The image is a single static Go binary.
- Connection health does not revoke grants. A configured agent keeps its access;
  health status explains why an upstream may currently be unavailable. Pausing
  a connection hides its tools from agents without touching grants.
- Browser POSTs to the administration interface must come from the same host
  that served the page. Agents and scripts talk to `/mcp` and `/admin/mcp` with
  bearer tokens instead.
- Import assignments are durable. Tools discovered later on an assigned
  connection are granted to the same agents automatically.
- Tool listing is deterministic and paginated. Tool definitions retain titles,
  schemas, annotations, and icons; tool results pass through text, images, audio,
  resource links, embedded resources, structured content, errors, and modern
  multi-round-trip input requests without payload rewriting.

See [DESIGN.md](DESIGN.md) for the architecture and data model.

See [MODEL_GATEWAY.md](MODEL_GATEWAY.md) for provider setup, Hermes/client
configuration, supported inference endpoints, and gateway limits.

## Users and roles

Administrators manage accounts from **Administration → Users and roles**.
The same section groups Settings and Operational logs; sign-out is at the bottom of the sidebar.
Create an account with an initial password and role; users can change their own
password in Settings. Accounts and roles persist across restarts.

| Role | Permissions |
| --- | --- |
| Administrator | All configuration, plus create users, change roles, and disable accounts |
| Operator | Manage agents, tokens, tool grants, connections, credentials, and model providers |
| Viewer | Read dashboards, configuration, and activity; no configuration changes or checks |

Permissions are enforced by the server, and unavailable controls are hidden.
Saving access revokes that user's sessions. Password changes revoke only that
user's sessions. At least one active administrator must remain, including during
concurrent role changes. Existing installations retain their administrator's
password and active sessions when upgrading.

Browser users are separate from agent bearer tokens. Disabling a user does not
revoke independently issued agent tokens; manage those from the agent page.

Model providers support native OpenAI, Anthropic Messages, and classic Azure
OpenAI deployment APIs, with independent authentication settings. An optional
LiteLLM bridge supplies cross-protocol translation, cloud-provider identity,
and ChatGPT/Codex device sign-in. See [Model gateway](MODEL_GATEWAY.md) for the
support matrix, bridge setup, and subscription limitations.
