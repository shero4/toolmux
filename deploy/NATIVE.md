# Native Linux deployment

This deployment runs Toolmux as a native systemd service so it can discover local
agent files and control the optional Codex Docker bridge. PostgreSQL and LiteLLM
run in Docker. Clients can run on the same host or connect from elsewhere.

## Prepare the host

Use a Linux server with systemd, Docker Engine and its Compose plugin, Git,
Python 3, OpenSSL, and curl. The installer builds the Go binary in Docker.

### Optional EC2 provisioning

Use `ec2-host.yaml` with CloudFormation in `us-east-1`. Supply the current Ubuntu
24.04 amd64 AMI, an existing SSH key name, and the administrator public IP as a
`/32`. The template creates a dedicated VPC, subnet, stable public IP, SSH-only
security group, SSM instance role and encrypted persistent root disk.

The template defaults to **t3.xlarge, 4 vCPUs, 16 GiB RAM, 80 GiB gp3**.
This is a template choice, not a minimum requirement for Toolmux. Size the host
for concurrent requests and any local tools or applications it also runs.
CPU credits use Standard mode: no surplus-credit billing, but sustained CPU use
above the credit allowance can throttle. Resize only after measuring CPU credit
balance, memory pressure and browser concurrency. Root storage is retained if
the instance is terminated; deleting the CloudFormation stack does not erase
that retained disk. Backups and retained disks must be managed deliberately.

## Install a reviewed checkout

The instance bootstrap installs Docker, Compose, Git, Python and the `toolmux`
service user. Clone the Toolmux repository into `/opt/toolmux` and make
that directory owned by `toolmux:toolmux`.

Deploy a committed revision from GitHub so the installed source is reproducible:

```sh
sudo git clone https://github.com/shero4/toolmux.git /opt/toolmux
sudo chown -R toolmux:toolmux /opt/toolmux
sudo -u toolmux git -C /opt/toolmux rev-parse HEAD
```

Record the deployed commit in your private operations record. Never copy `.env`,
local profiles, tokens, caches or database files into the checkout.

For a fresh installation, run from the checkout:

```sh
cd /opt/toolmux
sudo bash deploy/install-host.sh http://localhost:8081
```

This builds the application on the host, generates independent encryption,
database and bridge secrets in `/etc/toolmux/toolmux.env`, installs the service,
and starts the dependencies. Existing environment files are never overwritten.
The service user has Docker access and is a trusted host operator.

On your computer, keep an SSH tunnel running using the actual key and instance:

```sh
ssh -N -L 8081:127.0.0.1:8080 -i <private-key> ubuntu@<instance-address>
```

Open http://localhost:8081. Create the administrator before exposing any public
application port. If installation automation creates the initial account, retain
its generated password in a private local credential file, not in this guide or
Git. No ingress is permitted for the application, PostgreSQL or LiteLLM; the
browser's HTTP traffic travels inside encrypted SSH. A later domain/HTTPS rollout
must update TOOLMUX_BASE_URL, OAuth callbacks and network rules together.

## Verify and operate

```sh
sudo systemctl status toolmux
sudo journalctl -u toolmux --since '10 minutes ago'
sudo docker compose --env-file /etc/toolmux/toolmux.env -p toolmux-host -f /opt/toolmux/deploy/compose.host.yaml ps
curl --fail http://127.0.0.1:8080/healthz
```

Verify login, closed first-run setup, role restrictions, model/tool catalogs,
unauthenticated gateway rejection, and persistence after restarting Toolmux and
its dependencies. A fresh installation has no migrated agents or upstream tool
credentials. Codex bridge installation does not sign into the subscription:
complete the device authorization from Models → Codex sign-in when required.

Toolmux listens on `127.0.0.1:8080`. For native deployments, TOOLMUX_ADDR selects
the actual interface; `0.0.0.0:8080` would expose all IPv4 interfaces, subject to
firewall/security-group rules. TOOLMUX_BASE_URL selects the administration URL,
not the listening interface. PostgreSQL and LiteLLM publish localhost ports only.

Back up the PostgreSQL database and original master key securely. Keep the Compose
project name `toolmux-host` stable. The model-auth volume contains subscription
state once authorized. Upgrades must preserve these volumes and `/etc/toolmux`.
Build a new binary, stop Toolmux, install the binary, and restart; verify migrations
and login after a database backup. Do not rerun the fresh-install script to upgrade.
The [container guide](EC2.md) explains backup and restore principles. For this
layout, use `-p toolmux-host -f /opt/toolmux/deploy/compose.host.yaml` and
`--env-file /etc/toolmux/toolmux.env` when running database backup commands.
Inspect `systemctl cat toolmux` before upgrades: an ExecStart override may
select a versioned binary instead of `/usr/local/bin/toolmux`.

For public agent access with private administration, continue with the
[public gateway guide](GATEWAY.md).
