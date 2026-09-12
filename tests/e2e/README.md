# Live client integration test

This harness uses Hermes as one concrete test client. It is optional: Toolmux
clients are not required to use that runtime. For general development and test
setup, see [Development](../../docs/development.md).

This opt-in test uses a separate `toolmux-e2e` Hermes profile, a running local
Toolmux instance, its PostgreSQL database, Python, Go, and an explicitly supplied
GLM key. It makes paid inference requests only when you run the Hermes or
streaming checks. Existing business profiles and connections are not imported
or granted. The test GLM provider follows the shared catalog design and is visible
to all active agent tokens while enabled.

## Setup

Run from the Toolmux checkout. Create the profile once:

```text
hermes profile create toolmux-e2e --no-alias --no-skills
python tests/e2e/fixture.py http
```

Keep the fixture running in its own terminal. It binds only `127.0.0.1:8082`.
In another terminal, set `TOOLMUX_E2E_GLM_KEY` from your existing credential source
without putting the value in command arguments or source files. Use the GLM base
URL already configured for that key; the general and coding endpoints differ.
Toolmux's usual `.env` / `TOOLMUX_*` settings select its database and encryption key.

```text
go run ./tests/e2e -profile PATH_TO_HERMES_PROFILES/toolmux-e2e -python ABSOLUTE_PYTHON_EXECUTABLE -upstream GLM_BASE_URL -model glm-5
```

If Go is available only in Docker on Windows, compile first and run the resulting
executable natively, so discovery and local commands use the Windows environment:

```powershell
docker run --rm -e GOOS=windows -e GOARCH=amd64 -e CGO_ENABLED=0 -v "${PWD}:/src" -w /src golang:1.24-alpine go build -o tmp/e2e-setup.exe ./tests/e2e
```

The setup harness uses Toolmux's store/import/checker functions to arrange the
fixtures, then tests the **running HTTP endpoints** with an issued agent token.
It discovers and imports only the selected profile; stores the GLM key encrypted;
configures Hermes inference and MCP to use Toolmux; checks six tools across remote
MCP, stdio MCP, HTTP and command connections; removes/restores a connection grant;
and verifies a disposable token stops working for both MCP and models after
revocation. Rerunning intentionally restores the test profile's six fixture grants.
It writes only non-secret IDs to ignored `tmp/e2e/metadata.json`.
It also configures named `toolmux-glm` (Chat Completions) and `toolmux-codex`
(Responses) providers. Both use the same local proxy and agent token.

## Live agent test

```text
hermes -p toolmux-e2e chat --query-file tests/e2e/query.txt --oneshot --max-turns 6 --run-budget 180 --toolsets mcp-toolmux --provider custom --ignore-rules -Q
```

Expect two products of **703**, a command sum of **56**, and matching remote-MCP
and HTTP verification codes. The stdio nonce can differ because it runs in a
separate process. Verify the actual six tool events and inference events in
Toolmux Activity, filtered to `Hermes · toolmux-e2e`; do not rely only on the
model's written claims. Each active MCP entry and the model URL in this profile
must point to the local Toolmux instance.

For browser checks, remove a fixture tool's checkbox, verify a direct MCP call is
denied, then restore it. Refreshing its catalog must not undo the removal. Removing
one tool converts a whole-connection assignment into individual grants, preserving
the other granted tools. Also exercise model discovery in the provider screen.

## Codex device sign-in

Start the optional bridge using `compose.models.yaml` and a private local env file
containing its separate `LITELLM_MASTER_KEY`. Then run:

```text
python tools/model_login.py --env-file PATH_TO_PRIVATE_BRIDGE_ENV
```

Complete the printed device sign-in yourself. The helper delegates authentication
and refresh to the installed LiteLLM implementation. Tokens persist in the
`model-auth` Docker volume. Never copy them into Hermes or test reports.
After successful sign-in, the helper restarts only the bridge to load the saved
session. An unfinished device flow can impose LiteLLM's cooldown before a new
code is issued.

Optionally set `TOOLMUX_E2E_BRIDGE_KEY` to the bridge's key when running the setup
harness. This creates a paused `codex-bridge/codex` route pointing at local port
4000. After sign-in succeeds, rerun with `-enable-codex` or enable the provider in
the UI. Use `/v1/responses` for this route; provider-native API capabilities still
apply. Device authorization alone is not an inference test: verify a real request
and its Activity entry before marking Codex passed.

Run the six-tool conversation through the named Responses provider:

```text
hermes -p toolmux-e2e chat --query-file tests/e2e/query.txt --oneshot --max-turns 6 --run-budget 180 --toolsets mcp-toolmux --provider custom:toolmux-codex --model codex-bridge/codex --ignore-rules -Q
```

Use a named provider: this Hermes version deliberately ignores a persisted
Responses override on a plain `custom` provider pointing at a non-OpenAI host.
The named provider's explicit transport is honored. Configure the underlying
LiteLLM model to one available to your signed-in account; this run used
`chatgpt/gpt-5.6-terra`, since the sample `gpt-5.3-codex` was rejected.

Live API smoke checks (use Python with PyYAML, such as Hermes' environment):

```text
python tests/e2e/inference.py --profile PATH_TO_HERMES_PROFILES/toolmux-e2e --model e2e-glm/glm-5 --api chat-stream
python tests/e2e/inference.py --profile PATH_TO_HERMES_PROFILES/toolmux-e2e --model codex-bridge/codex --api responses
```

For this Codex bridge/model, use streamed Responses. The tested LiteLLM image
returned SSE even for a non-streaming Responses request, and its Chat Completions
translation was unreliable when the final event omitted assembled output items.
The streamed Responses test collects text deltas and checks the terminal event.

## Automated coverage and limits

```text
go test ./...
```

Set `TOOLMUX_TEST_DATABASE_URL` to an isolated PostgreSQL database whose name ends
in `_test` to enable persistence, setup/login, roles, session invalidation,
last-admin protection, provider editing/redaction, activity, and multipart grant
regressions. Without that variable the database tests skip. Each test uses its own
temporary schema. Provider unit tests use local upstream fixtures for Anthropic,
Azure, header/basic/no-auth, OAuth client credentials and streaming failures.
These are contract tests, not proof of live access to every third-party account.

The test profile, encrypted provider, connections and audit records remain for
inspection. Stop the fixture when finished, and pause its connections/provider in
Toolmux if you do not want to keep the demonstration active. Remove only this
test profile and explicitly named test resources when cleaning up; do not run a
global Hermes import or broad database cleanup.
