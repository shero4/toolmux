# Security

Please do not report suspected vulnerabilities in a public issue. Send a
private report to the repository owner with the affected version, impact, and
reproduction steps.

Sentinel's agent-facing MCP endpoint is an authorization boundary. Its
administration interface trusts the host network and has no application login.
Keep that interface private or protect it with your existing reverse proxy when
running remotely. Deploy agent traffic behind TLS, restrict database access,
and back up the encryption key separately from the database. The included
Compose file is intended for a private single-host deployment and does not
configure public TLS.
