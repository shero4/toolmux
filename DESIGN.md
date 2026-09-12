# Toolmux design

## Boundary

Toolmux is the authorization boundary between an MCP client and tools backed by
remote MCP servers, HTTP APIs, or installed commands. An agent authenticates to
Toolmux. Toolmux authorizes the requested tool, adds the upstream credential,
executes the typed adapter, and records the outcome.

It also provides a model gateway with a shared provider/model catalog; the
client owns model selection. It is not an agent runtime or host sandbox. The
operator trusts the Toolmux host and everything deliberately installed or
configured on it. See [MODEL_GATEWAY.md](MODEL_GATEWAY.md) for the adapter boundary.

Runtime and control traffic are separate. `/mcp` accepts revocable per-agent
tokens and enforces grants. `/admin/mcp` accepts a deterministic installation
admin token derived from the master key and exposes the small set of management
operations also available in the web interface. Ordinary agents cannot
self-grant.

## Request path

1. Hash the inbound bearer token and resolve one active agent identity.
2. For `tools/list`, return only tools joined through an active grant, in a
   stable paginated order.
3. For `tools/call`, resolve the exposed tool name through the same grant.
4. Decrypt the selected connection credential only for the outbound request.
5. Route to one bounded executor: MCP, HTTP, or direct command invocation.
6. Record the decision and outcome without credentials or response payloads.

Toolmux accepts the current stateless MCP 2026-07-28 envelope and the
handshake-era revisions used by existing clients. It probes upstream MCPs for
the modern lifecycle first and falls back to `initialize` on a legacy response.
Standard routing and parameter headers are validated or forwarded, and result
payloads remain structurally intact.

The MVP opens a short upstream session for each operation. This trades a small
amount of latency for simple failure isolation. Session pooling can be introduced
later without changing the database or authorization model.

## Relational model

- `connectors` describe remote MCP, stdio MCP, HTTP API, or command capability
  sources.
- `connections` describe authorized accounts or environments on a connector.
- `connection_credentials` contain encrypted secret material for one connection.
- `tools` are the common catalog and authorization unit.
- `http_tool_specs` contain one declarative HTTP operation per HTTP tool.
- `command_tool_specs` contain one shell-free process definition per command tool.
- `mcp_stdio_specs` contain one executable, argument array, and working directory
  per local MCP connection.
- `agents` are runtime-independent machine identities.
- `agent_installations` link an identity to a discovered Hermes or OpenClaw
  profile without mixing runtime metadata into the core identity table.
- `agent_tokens` authenticate agents; only token hashes are retained.
- `agent_connections` retain imported connection assignments so newly discovered
  tools continue to flow to the same agents.
- `grants` are atomic agent-to-tool permissions.
- `connection_checks` retain the most recent fifty results per connection.
- `audit_events` retain authorization and invocation outcomes.

Typed specifications are separated from the common catalog so grants, names,
schemas, and audit history do not depend on an executor. Tool grants reference
tool rows rather than patterns. Manual connections require explicit grants.
Imported connection assignments grant newly discovered tools to their assigned
agents, so upstream catalog changes do not silently break an imported runtime.
Tools missing from a later discovery remain recorded but are disabled,
preserving grant and audit history.

## Agent discovery

Discovery is a read-only adapter outside the request path. It recognizes the
documented Hermes default and named-profile layouts, OpenClaw default and named
state directories, and agent directories within those states. A native Windows
process can also run fixed, read-only probes inside installed WSL distributions.

Candidates are transient and identified by a stable hash of runtime, profile,
environment, and configuration path. Generic agent import creates a normal agent
and an `agent_installations` row in one database transaction.

Hermes' root profile is considered an agent only when it is the active default
profile. Hidden system-profile and nested group-chat metadata are deliberately
excluded from identity discovery. Agent slugs derive from the runtime and
profile, plus the environment when a candidate was found outside the local
computer, so the same profile name in two places never collides.

Candidates inside WSL are listed for visibility but skipped by the importer
when Toolmux runs natively on Windows: their files and local MCP executables
are not usable from that process.

The dedicated Hermes import deliberately goes further: it reads each profile's
MCP definitions and local OAuth artifacts, encrypts the credentials in Toolmux,
persists each profile-to-connection assignment, and adds a per-profile Toolmux
entry to the YAML. It first keeps an untouched `.toolmux.bak` copy and preserves
the original upstream entries. Repeating the import updates the same rows by
stable source key rather than creating duplicates. Containers require explicit
mounts, and host executables are only reusable when Toolmux itself runs on that
host.

## Credential handling

Credential payloads use AES-256-GCM with a random nonce per write. Bearer tokens,
OAuth client secrets, access tokens, and refresh tokens remain inside that
payload. The encryption key is supplied at runtime and never stored in
PostgreSQL. Agent tokens use 256 bits of randomness and are stored as SHA-256
hashes.

OAuth authorization uses an expiring, single-use state value and PKCE S256. The
stored state is hashed; its verifier is encrypted. Access tokens refresh before
expiry. Imported MCPs may reuse dynamic client registration metadata to create
a Toolmux callback client. A rejected refresh changes the connection to
`reauthorization_required` rather than silently dropping the integration.

For production, supply the master key through container secrets. Database
backups are insufficient to decrypt credentials without the master key. The
administration interface requires a persistent administrator account created
at first run. Passwords use bcrypt cost 12; browser session tokens are random,
stored only as hashes, expire after 12 hours, and are revoked for the affected
user when their password or access changes. Administrator, Operator, and Viewer
roles gate browser routes server-side. User management requires Administrator;
Viewer routes allow reads and personal password changes only. Role changes are
serialized to preserve at least one active administrator. Login attempts are limited in PostgreSQL per remote address.
Sessions use HttpOnly, SameSite=Lax cookies (for OAuth callbacks), same-origin
POST validation, and Secure cookies when the
configured public URL uses HTTPS. Browser pages are never cached.

## Administration interface

Pages are Go templates rendered on the server: one layout, shared partials, and
one template per page, each parsed into its own template set so page bodies
cannot collide. A single stylesheet and a small script are embedded in the
binary; there is no bundler or framework. The script only adds conditional form
fields, confirmations, clipboard copy, and immediate saving of tool grants;
every action also works as a plain form submission.

One-time messages, including a freshly issued token and its setup snippet, are
kept in an in-memory store for ten minutes and referenced from the redirect
URL by an opaque id that is consumed on first read. Tokens therefore never
appear in browser history, referrers, or server logs.

Browser POSTs are accepted only when their `Origin` or `Referer` matches the
request host, or when neither header is present (non-browser clients).

## Execution boundaries

HTTP tools can use paths relative to a connection or complete URLs. Templates
read top-level call inputs without requiring code. Connections support bearer,
OAuth 2.0, and arbitrary named credential headers.

Command tools hold an operator-configured executable plus an argument array and
optional working directory. They inherit the host environment so an existing
CLI installation and login work without a second credential system. Agents can
invoke granted definitions but cannot choose a different executable.

Stdio MCP definitions use the same shell-free process boundary. Toolmux speaks
JSON-RPC over stdin/stdout, imports only explicitly configured environment
values, and terminates the child when the bounded operation ends.

## Health semantics

A check distinguishes four facts instead of collapsing them into one ping:

- reachability: an HTTP exchange completed;
- protocol: the source-specific probe completed;
- authorization: the upstream accepted the configured credential;
- capability: the discovered tool catalog was reconciled successfully.

The connection status is `connected` only when all four are true. HTTP 401 and
403 become `reauthorization_required`; other failures become `unreachable` or
`degraded` depending on how far the check progressed.

## Deliberate omissions

- Automatic model selection, provider failover, pricing, and budget enforcement
- prompt, memory, and agent orchestration
- arbitrary shell strings or agent-selected executables
- Kubernetes operators
- a policy language
- WebSockets and legacy HTTP+SSE
- generalized host synchronization beyond the explicit Hermes importer
