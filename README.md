# Toolmux

**One place to configure tools and model providers for your AI clients.**

Connect your services once, then give each agent its own Toolmux token. Toolmux
provides a shared MCP endpoint for tools and an inference endpoint for models,
with a web interface to manage connections, access, and activity.

Use it with Claude Code, Codex, OpenClaw, Hermes, your own applications, or other
clients that support compatible MCP or inference APIs. Run it on your computer
or a server; your clients can run wherever you need them.

## What you can do

- **Share tools:** connect MCP servers, HTTP APIs, and installed commands; choose
  which tools each agent can access.
- **Centralize inference:** configure provider endpoints and credentials once,
  maintain their model catalogs centrally, then select concrete models in each
  client.
- **Manage access:** issue and rotate agent tokens, and give people
  administrator, operator, or viewer access to the web interface.
- **See activity:** view call outcomes and authorization status, and receive
  webhook alerts when an integration needs attention.

## Run locally

You need Git and Docker with Docker Compose. Use Docker Desktop on Windows or
macOS, or Docker Engine with the Compose plugin on Linux.

### 1. Get the source and build

```sh
git clone https://github.com/shero4/toolmux.git
cd toolmux
docker build -t toolmux:local .
```

### 2. Configure the encryption key

Copy [`.env.example`](.env.example) to `.env`. Generate a key:

```sh
docker run --rm toolmux:local keygen
```

Replace the `TOOLMUX_MASTER_KEY` line in `.env` with the generated line. Keep
this key private and retain it: saved provider credentials depend on it.
The other defaults are ready for local use.

### 3. Start Toolmux

```sh
docker compose up -d --build
```

Open [localhost:8080](http://localhost:8080) and create your administrator
account. Accounts, connections, and settings persist in the database across
restarts. To stop the application without removing its data, run
`docker compose down`.

The local setup listens on loopback. Use the deployment guides below before
making it available to other machines.

## Connect your first client

1. Add a tool connection in **Connections**, or a model provider in **Models**.
2. Create an identity in **Agents** and copy its token.
3. Assign the tools that identity may use.
4. Enter the endpoint and token in your client's settings.

| Client setting | Local value |
| --- | --- |
| MCP endpoint | `http://localhost:8080/mcp` |
| MCP authorization | `Authorization: Bearer <agent token>` |
| OpenAI-compatible inference base URL | `http://localhost:8080/v1` |
| Inference API key | The same agent token |
| Model | `<provider-prefix>/<model-id>` |

Model providers form a shared catalog available to active agent tokens; tool
access is granted separately. For clients on another machine, use your gateway
hostname instead of `localhost`.

Each runtime sends its exact primary or fallback `provider/model` ID. Record
those selections on the agent page too, so Toolmux can show which agents use a
catalog entry and warn before an operator disables it.

See [client setup](docs/clients.md) for examples and [model providers](MODEL_GATEWAY.md)
for protocol and authentication options.

## Deploy on a server

Choose the layout that fits where your tools run:

| Layout | Guide |
| --- | --- |
| Containers on a Linux server or EC2, using remote tools and APIs | [Container deployment](deploy/EC2.md) |
| A native Linux service with access to installed tools and local client files | [Native deployment](deploy/NATIVE.md) |
| Public MCP and inference over HTTPS, with VPN-only administration | [Public gateway](deploy/GATEWAY.md) |
| Move existing Hermes profiles onto the Toolmux host | [Hermes migration](deploy/HERMES.md) |

The public gateway uses standard ports 443/80. The administration interface can
stay on private port 8080, while clients on the host use loopback. Deployment
guides cover initial setup, DNS, TLS, provider OAuth callbacks, persistent data,
backups, and upgrades.

## Documentation

- [Documentation index](docs/README.md)
- [Connect tools](docs/tools.md) · [Connect clients](docs/clients.md) · [Model providers](MODEL_GATEWAY.md)
- [Users, tokens, and monitoring](docs/administration.md)
- [Configuration reference](docs/configuration.md) · [Dashboard updates](deploy/UPDATES.md)
- [Development](docs/development.md) · [Architecture](DESIGN.md) · [Security](SECURITY.md)
