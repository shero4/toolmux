# Connect a client

Create an agent in the web interface and copy its token when shown; it is
revealed once and never placed in a URL. Configure any compatible remote MCP client with:

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

Inference clients send an exact `provider/model` ID on every request. The
agent page can record the runtime's primary and fallback selections for catalog
visibility; this record does not override the model requested by the client.

## Client examples

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

## Inference clients

Add a provider in **Models**, then configure your client's inference settings:

```text
OpenAI-compatible base URL: http://localhost:8080/v1
API key: <agent token>
Model: <provider-prefix>/<model-id>
```

Anthropic-compatible clients use the Toolmux root URL without `/v1`. The
upstream must support the client's protocol, or use a translation bridge.
See [model providers](../MODEL_GATEWAY.md) for the supported combinations.

## Addresses and local helpers

Use localhost only when the client and Toolmux run on the same machine. Remote
clients use the gateway's HTTPS hostname. A container's loopback belongs to that
container; use a shared network or an explicitly configured host address.

These examples are conveniences, not a client allowlist. Any application with
compatible MCP or inference settings can connect. Each client chooses its own
models and workflow. [Discovery and import](local-clients.md) can assist with
supported installed-client layouts; manual setup works independently of them.
