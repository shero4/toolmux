# Security

Please do not report suspected vulnerabilities in a public issue. Send a
private report to the repository owner with the affected version, impact, and
reproduction steps.

Toolmux's agent-facing MCP endpoint is an authorization boundary. Its
administration interface requires a user account, with Administrator, Operator,
and Viewer permissions enforced on the server. The first account is an
administrator. Only administrators can create users or change their access. Complete
setup privately before exposing the service. Inference uses the same agent
tokens as MCP, with access to all enabled model providers. Deploy traffic behind
TLS, configure the HTTPS public base URL, restrict database access,
and back up the encryption key separately from the database. The included
Compose file is intended for a private single-host deployment and does not
configure public TLS.
