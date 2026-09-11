ALTER TABLE connectors DROP CONSTRAINT connectors_kind_check;
ALTER TABLE connectors ADD CONSTRAINT connectors_kind_check
    CHECK (kind IN ('mcp_http', 'mcp_stdio', 'http_api', 'command'));

ALTER TABLE connectors DROP CONSTRAINT connectors_transport_check;
ALTER TABLE connectors ADD CONSTRAINT connectors_transport_check
    CHECK (
        (kind = 'mcp_http' AND transport = 'streamable_http') OR
        (kind = 'mcp_stdio' AND transport = 'stdio') OR
        kind IN ('http_api', 'command')
    );

ALTER TABLE connections ADD COLUMN source_key text UNIQUE;

ALTER TABLE connections DROP CONSTRAINT connections_auth_method_check;
ALTER TABLE connections ADD CONSTRAINT connections_auth_method_check
    CHECK (auth_method IN ('none', 'bearer', 'oauth2', 'header'));

ALTER TABLE oauth_configs ALTER COLUMN client_id DROP NOT NULL;
ALTER TABLE oauth_configs DROP CONSTRAINT oauth_configs_token_auth_method_check;
ALTER TABLE oauth_configs ADD CONSTRAINT oauth_configs_token_auth_method_check
    CHECK (token_auth_method IN ('none', 'client_secret_basic', 'client_secret_post'));
ALTER TABLE oauth_configs ADD COLUMN registration_url text NOT NULL DEFAULT '';
ALTER TABLE oauth_configs ADD COLUMN redirect_uri text NOT NULL DEFAULT '';

CREATE TABLE mcp_stdio_specs (
    connection_id uuid PRIMARY KEY REFERENCES connections(id) ON DELETE CASCADE,
    executable text NOT NULL CHECK (length(executable) BETWEEN 1 AND 1024),
    args jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(args) = 'array'),
    working_directory text NOT NULL DEFAULT ''
);

CREATE TABLE agent_connections (
    agent_id uuid NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    connection_id uuid NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_id, connection_id)
);

CREATE INDEX agent_connections_connection_idx ON agent_connections(connection_id);
