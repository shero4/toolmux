# Local discovery and import

These optional helpers reuse supported local configuration layouts. Toolmux
works with other clients through [manual setup](clients.md); automatic discovery
is not required. Run the commands below from the repository root.

When Toolmux runs directly on a computer, **Agents → Discover installed** scans
the current user's standard Hermes and OpenClaw configuration locations. On
Windows it also lists profiles inside WSL distributions. Detection is read-only
and ignores directories that do not contain an agent configuration.

Hermes profiles are imported in full (see below), one at a time or all at once.
OpenClaw agents are added as identities with a token and the server entry to
paste into OpenClaw. Profiles that live inside WSL are shown but can only be
imported by running Toolmux inside that distribution, because their local MCP
servers and files are not reachable from a Windows process.

A container cannot see host files unless they are mounted. Set
`TOOLMUX_HOST_HOME` in `.env`, then include the small discovery override:

```sh
docker compose -f compose.yaml -f compose.discovery.yaml up --build
```

The host home is mounted read-only. Toolmux looks for Hermes' active default
profile and named profiles below its standard data directory, plus OpenClaw agents
below `.openclaw` and named `.openclaw-*` state directories. You can instead set
`TOOLMUX_DISCOVERY_ROOTS` to an OS path-list when running the binary directly.
Hermes' hidden system profile and its group-chat metadata are not imported as
agent identities.

## Hermes profile import

Run Toolmux directly on the host when you want to reuse host-installed stdio
MCPs or CLIs. On **Agents → Discover installed**, choose **Import profile** on the matching
row in Agent profiles. The import is idempotent and:

- creates one Toolmux agent for every Hermes profile;
- keeps separate instances when the same MCP is configured in multiple profiles;
- imports remote MCP, stdio MCP, header, bearer, and reusable OAuth state;
- imports every installed `gws-*` identity as a separate Google Workspace
  connection and makes those connections available to all imported profiles;
- writes each profile's own Toolmux URL and token into its `mcp_servers` map;
- preserves the original YAML beside it as `config.yaml.toolmux.bak`.

Existing upstream MCP entries remain in place during the test period. Remove
them after you are satisfied that calls are flowing through Toolmux. If an
imported credential is expired, the connection page offers **Authorize** for
OAuth or **Replace and check** for a bearer/API key connection.

Re-running the import never downgrades state Toolmux now owns: an OAuth client
that Toolmux registered for its own callback is kept, an older token from
Hermes never replaces a newer one, and a server whose endpoint changed gets its
own connector instead of rewriting the shared one.
