# Toolmux design

## Boundary

Toolmux is the authorization boundary between an MCP client and tools backed by
remote MCP servers, HTTP APIs, or installed commands. An agent authenticates to
Toolmux. Toolmux authorizes the requested tool, adds the upstream credential,
executes the typed adapter, and records the outcome.

It is not an agent runtime, identity provider, secret manager, model gateway, or
host sandbox. The operator trusts the Toolmux host and everything deliberately
installed or configured on it.

## Request path

1. Hash the inbound bearer token and resolve one active agent identity.
2. For `tools/list`, return only tools joined through an active grant.
3. For `tools/call`, resolve the exposed tool name through the same grant.
4. Decrypt the selected connection credential only for the outbound request.
5. Route to one bounded executor: MCP, HTTP, or direct command invocation.
6. Record the decision and outcome without credentials or response payloads.

The MVP opens a short upstream session for each operation. This trades a small
amount of latency for simple failure isolation. Session pooling can be introduced
later without changing the database or authorization model.

## Relational model

- `connectors` describe MCP, HTTP API, or command capability sources.
- `connections` describe authorized accounts or environments on a connector.
- `connection_credentials` contain encrypted secret material for one connection.
- `tools` are the common catalog and authorization unit.
- `http_tool_specs` contain one declarative HTTP operation per HTTP tool.
- `command_tool_specs` contain one shell-free process definition per command tool.
- `agents` are runtime-independent machine identities.
- `agent_installations` link an identity to a discovered Hermes or OpenClaw
  profile without mixing runtime metadata into the core identity table.
- `agent_tokens` authenticate agents; only token hashes are retained.
- `grants` are atomic agent-to-tool permissions.
- `connection_checks` retain health history.
- `audit_events` retain authorization and invocation outcomes.

Typed specifications are separated from the common catalog so grants, names,
schemas, and audit history do not depend on an executor. Tool grants reference
tool rows rather than patterns. A newly discovered tool is
therefore denied until explicitly granted. Tools missing from a later discovery
remain recorded but are disabled, preserving grant and audit history.

## Agent discovery

Discovery is a read-only adapter outside the request path. It recognizes the
documented Hermes default and named-profile layouts, OpenClaw default and named
state directories, and agent directories within those states. A native Windows
process can also run fixed, read-only probes inside installed WSL distributions.

Candidates are transient and identified by a stable hash of runtime, profile,
environment, and configuration path. Importing creates a normal agent and an
`agent_installations` row in one database transaction. Toolmux never stores or
copies the runtime's own secrets and does not rewrite external configuration.
Containers require an explicit read-only host mount because host paths do not
otherwise exist inside the container boundary.

## Credential handling

Credential payloads use AES-256-GCM with a random nonce per write. Bearer tokens,
OAuth client secrets, access tokens, and refresh tokens remain inside that
payload. The encryption key is supplied at runtime and never stored in
PostgreSQL. Agent tokens use 256 bits of randomness and are stored as SHA-256
hashes.

OAuth authorization uses an expiring, single-use state value and PKCE S256. The
stored state is hashed; its verifier is encrypted. Access tokens refresh before
expiry. A rejected refresh changes the connection to
`reauthorization_required` rather than silently dropping the integration.

For production, supply the master key through container secrets. Database
backups are insufficient to decrypt credentials without the master key. The
administration interface relies on host or reverse-proxy access control.

## Execution boundaries

HTTP tools can use paths relative to a connection or complete URLs. Templates
read top-level call inputs without requiring code. Connections support bearer,
OAuth 2.0, and arbitrary named credential headers.

Command tools hold an operator-configured executable plus an argument array and
optional working directory. They inherit the host environment so an existing
CLI installation and login work without a second credential system. Agents can
invoke granted definitions but cannot choose a different executable.

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

- LLM routing and metering
- prompt, memory, and agent orchestration
- arbitrary shell strings or agent-selected executables
- Kubernetes operators
- a policy language
- WebSockets and legacy HTTP+SSE
- automatic granting of newly discovered tools
