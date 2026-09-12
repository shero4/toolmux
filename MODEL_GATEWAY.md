# Model gateway

Toolmux is a shared inference entry point. The client chooses its model,
including auxiliary models and fallbacks; Toolmux never chooses these for it.

## Provider setup

1. Sign in and open **Models → Add provider**.
2. Set a display name and stable prefix, for example `work-provider`.
3. Select the protocol, enter its base URL, and configure authentication.
4. Choose provider discovery, signed-in Codex account discovery, or manual
   model IDs. Save, then choose **Discover models**.
5. In an agent, configure a custom OpenAI-compatible provider with base URL
   `http://localhost:8080/v1` and its existing Toolmux agent token as the API key.
6. Select a returned model ID such as `work-provider/model-id`.

Every active agent token can list and invoke every configured model on enabled
providers. MCP tool grants are independent. Revoke the token or disable the
agent to stop both tool and model access. Pausing a provider prevents new calls
through that provider; already-running requests finish normally.

The model catalog persists in PostgreSQL. Successful discovery adds new IDs and
marks disappeared discovered IDs unavailable. Manual IDs are preserved, and a
failed discovery leaves the last known-good catalog routable. Wildcard routing
entries are never published as models. Changing a provider's display name
leaves its model prefix intact.

## Protocols and authentication

Protocol and authentication are configured independently.
Native JSON parameters, tool definitions, thinking,
images, and streaming events are forwarded without a lossy translation layer.

| Protocol | Client endpoints | Base URL |
| --- | --- | --- |
| OpenAI-compatible | chat/completions, responses, completions, embeddings | API version URL, e.g. https://api.openai.com/v1 |
| Anthropic-compatible | messages, messages/count_tokens | Root or /v1 URL |
| Azure deployment API | chat/completions, completions, embeddings | Resource root; configure API version and deployment names |
| LiteLLM | OpenAI and Anthropic endpoints supported by the bridge | Proxy API version URL |

Every endpoint above is under Toolmux's `/v1/`. `GET /v1/models` returns the
shared catalog. Authenticate with `Authorization: Bearer <agent-token>` or
`x-api-key: <agent-token>`; incoming credentials never pass upstream. For an
Anthropic SDK, set its base URL to the Toolmux root (without `/v1`). For an
OpenAI SDK, use the Toolmux root plus `/v1`.

Native adapters preserve their own protocol. OpenAI requests to a direct
Anthropic provider return a clear protocol mismatch. Use LiteLLM when clients
need a different protocol than the provider speaks. Azure's new `/openai/v1`
API uses the OpenAI-compatible adapter; the Azure adapter is for the classic
versioned deployment API. Its model IDs are deployment names entered manually.

Authentication choices:

- Protocol default: Bearer for OpenAI/LiteLLM, x-api-key for Anthropic,
  api-key for Azure.
- Bearer token, custom API-key header, HTTP Basic, or no built-in auth.
- OAuth 2.0 client credentials: token endpoint, client ID, client secret,
  optional scope/audience, and Basic or request-body client authentication.
  Short-lived bearer tokens are cached until shortly before expiry. Secret
  or auth-setting changes invalidate reuse. No expiry means no cache reuse.
- Additional headers for organizations, projects, beta features, or custom
  authorization. All values are encrypted and never returned in forms. Blank
  preserves saved headers; `{}` clears them. Select no built-in authentication
  when supplying your own Authorization header. Transport headers are rejected.

Secrets are encrypted in PostgreSQL. Blank edits retain credentials; an explicit
clear option removes them. Anthropic version/beta headers may be supplied by
clients; a configured provider value takes precedence. Other client headers,
including cookies and credentials, are not forwarded.

## Optional LiteLLM bridge

The included `compose.models.yaml` runs a separate, loopback-bound LiteLLM
service with persistent subscription-token storage. This keeps Toolmux's Go
runtime small while delegating provider translations and cloud SDK identity to
an established library. Gemini, Bedrock, Vertex, and other SDK-based providers
are configured in LiteLLM using their documented credentials or workload identity.

1. Copy `deploy/litellm.example.yaml` to an ignored local file under `tmp/`.
   Set `LITELLM_CONFIG` to that file. Its `chatgpt/*` wildcard is a routing
   rule; Toolmux never exposes the wildcard to clients.
2. Set a separate random `LITELLM_MASTER_KEY` in your environment. Use provider
   environment variables or a secrets manager; do not commit real credentials.
3. Run `docker compose -f compose.models.yaml up -d`. The default image follows
   LiteLLM's `main-stable` channel; set `LITELLM_IMAGE` to a tested version or digest
   for reproducible deployment.
4. Add a Toolmux provider with protocol LiteLLM, URL `http://127.0.0.1:4000/v1`
   for native Toolmux, and the bridge's master key. If both services run through
   the combined Compose files, use `http://models:4000/v1` from the Toolmux container.
5. For ordinary LiteLLM aliases, choose provider discovery. For Codex
   subscription access, choose signed-in Codex account discovery. Toolmux then
   exposes concrete IDs such as `codex-bridge/gpt-5.6-terra`, while LiteLLM
   routes the selected suffix to `chatgpt/gpt-5.6-terra`.

ChatGPT/Codex subscription access uses LiteLLM's `chatgpt/` provider. The first
local request prints a verification URL and device code in the bridge logs;
complete the sign-in yourself. Follow `docker compose -f compose.models.yaml logs -f models`
while making that request. LiteLLM owns token refresh and keeps its auth in the
`model-auth` Docker volume. Toolmux reads the available catalog through the
authenticated bridge process; access and refresh tokens never leave that
volume. Toolmux does not copy desktop client credentials.
Responses is the native API; Chat Completions is translated for supported models.
On a native host with Docker access, Toolmux's **Codex sign-in** page starts
device authorization and displays its status. Set `TOOLMUX_CODEX_CONTAINER` to
the actual bridge container name. The stock application container does not
include the Docker CLI needed by this control. For CLI sign-in, run
`python tools/model_login.py --env-file PATH_TO_PRIVATE_BRIDGE_ENV` after starting
the bridge. It prints the device instructions without printing saved tokens.
Use a Responses-capable client and verify a streamed request with a model
available to the account. Device authorization alone does not establish
inference compatibility. Model selection remains in the client.

Toolmux does not offer Claude Code subscription sign-in. Configure Claude
providers with API keys or supported cloud-provider authentication.

This is an extensible set of protocols and authentication mechanisms, not a claim
that every endpoint or identity scheme is interchangeable. AWS request signing,
Google workload identity, and provider-specific OAuth belong in the bridge.
Arbitrary protocol translation, generic interactive OAuth, mTLS, and API-key query
parameters are not implemented in Toolmux's native adapters.

Provider references:

- [LiteLLM provider catalog](https://docs.litellm.ai/docs/providers)
- [LiteLLM Anthropic-compatible API](https://docs.litellm.ai/docs/anthropic_unified)
- [LiteLLM ChatGPT subscription authentication](https://docs.litellm.ai/docs/providers/chatgpt)
- [Claude authentication restrictions](https://code.claude.com/docs/en/legal-and-compliance#authentication-and-credential-use)

## Activity and operational limits

Tool and model calls share Activity and the overview's 24-hour counts. Model
events retain the provider name, requested model ID, upstream model, outcome,
elapsed time, and input/output tokens if reported. Missing usage is shown as
an em dash, not zero. For streaming chat, a client can request usage with
`stream_options.include_usage` if its provider supports it. No token prices or
cost estimates are inferred.

The gateway has a 16 MiB request limit, 32 MiB non-streaming response limit,
and a per-provider timeout between 10 seconds and one hour. Streams are
forwarded incrementally with bounded usage inspection. Client cancellation
cancels the upstream request. Failed or incomplete streams are recorded as
errors. Provider redirects are rejected; inbound cookies and arbitrary client
headers are never forwarded. Provider HTTP errors are returned with their
status but a sanitized message.

There is no automatic retry, failover, budget enforcement, prompt tracing, or
background Responses retrieval. Provider keys use the installation encryption
key; back it up separately from PostgreSQL. All provider URLs and host
executables are administrator-controlled.

## Verification

Run `go test ./...` and `go vet ./...`. Database integration tests additionally
require `TOOLMUX_TEST_DATABASE_URL` pointing to an isolated PostgreSQL database
whose name ends in `_test`. Each test creates and removes its own schema.
These tests cover first-run setup, session persistence, protected pages,
provider creation/discovery, inference and activity, paused providers,
revoked agent access, password changes, and login attempt limits.

See [development](docs/development.md) for the test workflow and optional
live-client harness.
