# Development

Toolmux is a Go service with PostgreSQL, server-rendered templates, and static
CSS and JavaScript. There is no frontend build step. See [architecture](../DESIGN.md)
for the request flow and data model.

## Run from source

Use the Go version in `go.mod`. Configure `.env` as described in the
[quickstart](../README.md), start only the development database, then run:

```sh
docker compose up -d db
go run ./cmd/toolmux
```

Stop any other Toolmux process using the same port first. Native execution is
useful for local command tools and client configuration files. PostgreSQL
migrations run at startup.

## Tests

```sh
go test ./...
go vet ./...
```

To include database integration tests, set `TOOLMUX_TEST_DATABASE_URL` to an
isolated PostgreSQL database whose name ends in `_test`. Tests create separate
schemas. Without that setting, database tests skip. Never point tests at the
installation database.

The suite covers tokens, grants, provider adapters, browser accounts and roles,
configuration changes, activity, and operational checks. Local upstream fixtures
keep ordinary tests independent of paid inference and third-party credentials.

For a deployed public gateway, run the read-only route check:

```sh
python tests/e2e/gateway_boundary.py https://toolmux.example.com
```

The [live client test](../tests/e2e/README.md) uses an isolated Hermes profile as
one end-to-end test driver. It is an optional integration fixture, not a required
runtime or a restriction on supported clients. Live inference needs explicitly
supplied credentials and can incur provider charges.

## Contributions

Keep changes focused, test the affected behavior, and update the relevant guide.
Use generic client examples in shared docs; place runtime-specific procedures in
client guides. Keep operational records, credentials, and local test output out
of commits. Report security issues privately as described in [Security](../SECURITY.md).
