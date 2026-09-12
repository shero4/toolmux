# Administration

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

## Token rotation

Issue and revoke agent tokens from the agent page. Rotation can offer to update
exact references in supported local client files after showing a preview. Remote
clients and external secret stores need their own updates. Restart clients that
load credentials only at startup. If an update partially fails, both tokens
remain valid; finish updating before revoking the old token.

## Activity and alerts

Activity records tool and inference outcomes, duration, and provider-reported
usage. Operational logs record authorization changes, token updates, and alert
delivery. Neither stores prompts, tool arguments, results, or credentials.
Operational logs retain 14 days, capped at 2,000 events; call activity is separate.

Configure periodic checks and an optional HTTPS webhook in Settings. Alerts
report authorization failures and recovery, with bounded retries. A successful
check establishes connectivity or authorization, not that every operation has
been tested. Use a harmless real request to verify a new integration.

The control endpoint belongs on the private administration network. It is not
forwarded by the public agent gateway.
