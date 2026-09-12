# Public agent gateway, private administration

Use this layout for a native Linux host. MCP and inference use standard HTTPS;
no separate public MCP port is necessary.

| Consumer | URL | Access |
| --- | --- | --- |
| Remote agent tools | `https://mcp.example.com/mcp` | Agent bearer token |
| Remote agent inference | `https://mcp.example.com/v1` | Same agent token |
| VPN administrator | `http://PRIVATE_IP:8080` | User sign-in and role |
| Agent on this host | `http://127.0.0.1:8080/mcp` or `/v1` | Same agent token |

Loopback means the agent's own machine. An agent on a different computer must
use the public hostname or VPN address. Containers need an explicitly configured
host route; their loopback is not the host's loopback.

## Configuration

Keep `TOOLMUX_BASE_URL=http://PRIVATE_IP:8080` for administration cookies and
upstream OAuth callbacks. Set `TOOLMUX_GATEWAY_URL=https://mcp.example.com` for
advertised agent endpoints and imports. If omitted, the gateway URL defaults to
the administration URL. `TOOLMUX_ADDR=0.0.0.0:8080` allows both VPN and loopback;
restrict TCP 8080 in the security group to the actual VPN source only. Do not
allow public IPv4 or IPv6 ingress to 8080. Restart Toolmux after changing its env.

Set `TOOLMUX_DOMAIN=mcp.example.com` in `/etc/toolmux/toolmux.env`, then run:

```sh
sudo docker compose --env-file /etc/toolmux/toolmux.env -p toolmux-gateway -f /opt/toolmux/deploy/compose.gateway.yaml up -d
```

Allow public TCP 80 and 443 in the instance security group. For the starter
CloudFormation template these are explicit post-install rules (the initial stack
allows only SSH). Record them alongside the separately managed VPN rules and
preserve them during stack updates:

```sh
aws ec2 authorize-security-group-ingress --region REGION --group-id SECURITY_GROUP --protocol tcp --port 80 --cidr 0.0.0.0/0
aws ec2 authorize-security-group-ingress --region REGION --group-id SECURITY_GROUP --protocol tcp --port 443 --cidr 0.0.0.0/0
```

 Port 80 redirects to
HTTPS and handles certificate validation; agents must send credentials only to
HTTPS. Caddy forwards only exact `/mcp` and `/v1/*` paths to loopback port 8080,
preserving authorization headers and streaming responses. Everything else,
including `/admin/mcp`, login, setup, settings, and OAuth callbacks, returns 404.
Caddy's management interface remains loopback-only. Keep its persistent volumes
for certificates and renewals. Do not substitute the full-UI `Caddyfile` from the
single-surface container deployment.

## DNS and TLS

Create an A record for the chosen name pointing to the instance's Elastic IP.
Use DNS-only initially so certificate issuance and direct-origin access can be
verified. Avoid an AAAA record unless IPv6 routing and firewall rules are also
configured. Caddy obtains and renews the certificate automatically once public
DNS and ports 80/443 work. DNS changes are a prerequisite, not evidence that TLS
has succeeded. If later enabling Cloudflare proxying, use Full (strict) SSL,
bypass cache and interactive challenges for gateway paths, and verify streaming
requests within Cloudflare's request-duration limits. Never use Flexible SSL.

Upstream OAuth login is initiated by an administrator on the VPN; its registered
callback remains the administration URL. Providers that require an HTTPS callback
need a private HTTPS administration hostname or an SSH/local callback supported
by that provider. Do not expose all administration routes to make a callback work.

## Verification

From outside the VPN, verify a trusted certificate for the hostname, a redirect
on HTTP, token rejection on `/mcp` and `/v1/models`, and 404 on `/`, `/login`,
`/setup`, `/settings`, `/admin/mcp`, and `/oauth/callback`. Confirm public 8080 is
unreachable. With an issued test agent token, check allowed tool discovery and
model discovery, then revoke it. Verify the VPN UI still supports login and that
loopback agents have the same grants as public agents. Never log request bodies,
authorization headers, or tokens when testing.

Reference: [Caddy route isolation](https://caddyserver.com/docs/caddyfile/directives/handle)
and [automatic HTTPS](https://caddyserver.com/docs/automatic-https).

Run the read-only boundary check with `python tests/e2e/gateway_boundary.py https://mcp.example.com`.
