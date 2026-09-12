# Connect tools

A connection represents an upstream service or account. Tools are its callable
actions. Add and authorize a source in **Connections**, then assign access on
the agent page.

## MCP servers

Add a remote MCP endpoint and its authentication. Toolmux discovers the catalog
and exposes granted tools through its own MCP endpoint. Local stdio servers
require their executables and dependencies where Toolmux runs. See
[local import](local-clients.md) for reusing supported existing configurations.

## Assign access

Whole-connection assignments include newly discovered tools. Individual grants
allow a selected subset. Pausing a connection hides its tools without removing
grants. Removing a tool from a whole-connection assignment switches that agent
to individual grants for the remaining tools.

## Expose an HTTP API

Create an `HTTP API` connection with the service's base URL and authorization.
Then open that connection and choose **Define tool** for each permitted operation. Paths, query values,
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

## OAuth connections

Register `<administration-base-url>/oauth/callback` with the upstream provider.
Toolmux uses Authorization Code with PKCE and refreshes saved credentials before
expiry. Use **Authorize** when sign-in is needed again. A provider may support
dynamic client registration; otherwise supply its client ID and required secret.
Keep the callback on the administration address in a split gateway deployment.

Use **Connections → Browse all tools** to search the combined catalog. Define
and inspect actions inside a connection; assign agent access from **Agents**.
