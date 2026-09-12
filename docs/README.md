# Documentation

Start with the [README](../README.md) to run Toolmux and connect a client.
These guides cover the next steps without depending on a particular agent runtime.

| Task | Guide |
| --- | --- |
| Connect an application to MCP or inference | [Clients](clients.md) |
| Add MCP servers, HTTP APIs, or commands | [Tools](tools.md) |
| Configure model providers and authentication | [Model gateway](../MODEL_GATEWAY.md) |
| Manage users, tokens, and alerts | [Administration](administration.md) |
| Set addresses and deployment variables | [Configuration](configuration.md) |
| Deploy with containers | [Container deployment](../deploy/EC2.md) |
| Run a native Linux service | [Native deployment](../deploy/NATIVE.md) |
| Separate public agents from private administration | [Public gateway](../deploy/GATEWAY.md) |
| Reuse supported installed-client configurations | [Discovery and import](local-clients.md) |
| Check and install application updates | [Updates](../deploy/UPDATES.md) |
| Contribute or run tests | [Development](development.md) |
| Understand implementation and trust boundaries | [Architecture](../DESIGN.md), [Security](../SECURITY.md) |

## Terms

**Agent** is a client identity, not a process Toolmux runs. **User** is a person
who signs into the web interface. A **connection** is a tool source and its
account; a **tool** is one action. A **provider** supplies model inference.
Toolmux keeps these configured centrally while each client owns its workflow.
