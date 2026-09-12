# Container deployment

Run Toolmux and PostgreSQL on a Linux server with an optional Caddy HTTPS
proxy. Use EC2 or another Docker host; adapt the firewall and storage settings
to your provider.
Run commands from the Toolmux checkout on the Linux server. Use Docker Engine
with the Compose plugin, Git and OpenSSL. Use an encrypted persistent EBS disk
for Docker data and retain backups independently of the instance.

## 1. Prepare an isolated installation

Clone the repository and record the revision you deploy:

```sh
git clone https://github.com/shero4/toolmux.git
cd toolmux
git rev-parse HEAD
```

Build before generating secrets, so Compose does not require a key that has not
yet been created:

```sh
docker build -t toolmux:local .
umask 077
printf 'TOOLMUX_DB_PASSWORD=%s\n' "$(openssl rand -hex 32)" > .env
# This outputs TOOLMUX_MASTER_KEY=<base64 value>.
docker run --rm toolmux:local keygen >> .env
cat >> .env <<'ENV'
TOOLMUX_BASE_URL=http://localhost:8080
TOOLMUX_BIND_IP=127.0.0.1
TOOLMUX_PORT=8080
ENV
chmod 600 .env
docker compose -p toolmux -f compose.ec2.yaml config --quiet
docker compose -p toolmux -f compose.ec2.yaml up -d --build
```

These commands are for a fresh server only. Never overwrite an existing `.env`
or regenerate its master key. Use a URL-safe database password (hex is safe).
Changing POSTGRES_PASSWORD does not update the password in an existing database;
perform a deliberate database password rotation instead.

The database has no host port. Only localhost:8080 is published initially.
Keep the EC2 security group restricted to SSH from your administrator IP, or
use your existing private access path. Do not open 5432, 4000, or 8080 publicly.
From your computer, forward a port using your actual SSH identity/host:

```sh
ssh -L 8080:127.0.0.1:8080 <ssh-user>@<ec2-host>
```

Open http://localhost:8080 and create the first administrator while public
access is closed. If local 8080 is occupied, choose a different local forwarded
port and use that localhost port in TOOLMUX_BASE_URL during bootstrap. Set up
other users under Administration → Users and roles.

## 2. Select how clients reach the service

This file's HTTPS profile exposes the full application, including sign-in.
For public agent endpoints with private administration, use the
[native public gateway layout](GATEWAY.md) instead.

**Public HTTPS:** assign a stable address, point a DNS name to it, then add
`TOOLMUX_DOMAIN=toolmux.example.com` and change
`TOOLMUX_BASE_URL=https://toolmux.example.com` in `.env`. Replace the example
with your real domain. Keep `TOOLMUX_BIND_IP=127.0.0.1`.

Allow inbound TCP 80 and 443 to the proxy in the EC2 security group. Caddy needs
a reachable domain for automatic certificate issuance; HTTP redirects to HTTPS.
Leave SSH limited to the administrator's source. Start the HTTPS profile:

```sh
docker compose -p toolmux -f compose.ec2.yaml --profile https up -d --build
```

Caddy preserves the original Host and streams upstream responses without
buffering. TOOLMUX_BASE_URL must be HTTPS for Secure browser cookies and must
match the externally visible origin. OAuth redirect registrations use
`https://toolmux.example.com/oauth/callback`. Root-path hosting is supported;
a URL path prefix is not supported.

**Existing load balancer or private reverse proxy:** omit the `https` profile.
Set TOOLMUX_BIND_IP to the instance's private interface IP and allow 8080 only
from the load balancer/proxy security group. Set TOOLMUX_BASE_URL to the HTTPS
client-facing URL, preserve Host, disable response buffering, and choose idle
timeouts suitable for long inference/tool streams. A TCP health check or
`/healthz` can confirm process availability; `/healthz` is not a database
readiness check. Verify login and an authenticated request as well.

**All interfaces:** TOOLMUX_BIND_IP=0.0.0.0 publishes the application on all IPv4
host interfaces. A specific private IP must actually be assigned to the EC2
network interface. Do not bind an EC2 public/Elastic IP that is implemented by
NAT rather than assigned inside the guest. The security group determines who
can reach a published port. For private-only deployment, use a private route and
TLS rather than transmitting agent tokens over unencrypted public HTTP.

Docker and AWS references: [published ports](https://docs.docker.com/engine/network/port-publishing/),
[EC2 security groups](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/creating-security-group.html).

## 3. Verify before connecting real agents

```sh
docker compose -p toolmux -f compose.ec2.yaml --profile https ps
curl --fail https://toolmux.example.com/healthz
```

Verify setup no longer accepts a new administrator, sign-in works, role gates
hold, an authorized test agent can list models/tools, and a harmless streaming
model/tool request succeeds. Restart the stack and verify the same accounts,
providers and grants remain. Check Administration → Operational logs for
connection authorization and configure webhook alerts deliberately.

## 4. Persistence, backup and upgrades

Keep the same Compose project name (`-p toolmux`). PostgreSQL uses the
`toolmux-data` named volume; Caddy certificates use `caddy-data` and its config
uses `caddy-config`. Restart policies bring services back when Docker starts.
Enable Docker startup on the host. Container logs rotate at 10 MB, three files
per service; application operational events retain 14 days, capped at 2,000.
Call activity has separate retention and is not pruned by the operational log
cap. Plan database storage accordingly.

Back up the database and `.env` securely; the original encryption key is needed
to decrypt upstream credentials and derives the administration token. On Linux:

```sh
umask 077
mkdir -p backups
docker compose -p toolmux -f compose.ec2.yaml exec -T db pg_dump -U toolmux -d toolmux -Fc > backups/toolmux.dump
```

Use a unique dated backup filename in normal operation and copy backups to your
approved encrypted backup destination. Test restoration into an isolated empty
database using pg_restore before relying on the backup. Never run `down -v` on
an installation you intend to retain. Before upgrades, take a backup and retain
the prior image/revision. Migrations run at startup; an older binary may not be
compatible with a newer schema, so rollback may require restoring the matching
backup. Migrate local data only deliberately; do not automatically copy test
profiles or host-specific paths to EC2.

## 5. Local tools and subscription bridges

The stock application image contains only the Go binary. Remote MCP/HTTP tools
and native model-provider requests work without host CLI dependencies.
Client files and CLIs on another computer do not become accessible on the server.
Automatic token replacement only works for supported files on the server that
Toolmux can read and write; remote client configurations need manual updates.

The Codex sign-in UI currently invokes the Docker CLI on the Toolmux host.
The stock distroless application container has neither that CLI nor Docker
daemon access, so it will show Codex authorization as unavailable. Do not mount
the Docker socket merely to hide that limitation. If frontend Codex sign-in is
required for this deployment, use a native Linux Toolmux service alongside the
existing model bridge, or implement a dedicated authenticated bridge-control
service before rollout. The native host must have Docker access and set
TOOLMUX_CODEX_CONTAINER to the actual bridge container name.

The optional `compose.models.yaml` can run a bridge separately with its own
persistent `model-auth` volume. Containerized Toolmux reaches it at
`http://models:4000/v1` when both join the same Compose project/network;
`127.0.0.1:4000` only works from the host. Pin LITELLM_IMAGE to a tested digest,
configure provider keys separately, and authorize the remote bridge explicitly.
Do not assume this computer's signed-in subscription has moved to EC2.
See [model setup](../MODEL_GATEWAY.md) for CLI sign-in and adapter details.
