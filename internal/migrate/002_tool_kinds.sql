ALTER TABLE connectors
    ADD COLUMN kind text NOT NULL DEFAULT 'mcp_http'
        CHECK (kind IN ('mcp_http', 'http_api', 'command')),
    ADD COLUMN health_path text NOT NULL DEFAULT '';

ALTER TABLE connectors ALTER COLUMN endpoint_url DROP NOT NULL;
ALTER TABLE connectors DROP CONSTRAINT connectors_transport_check;
ALTER TABLE connectors ADD CONSTRAINT connectors_transport_check
    CHECK ((kind = 'mcp_http' AND transport = 'streamable_http') OR kind <> 'mcp_http');

ALTER TABLE tools
    ADD COLUMN kind text NOT NULL DEFAULT 'mcp'
        CHECK (kind IN ('mcp', 'http', 'command'));

CREATE TABLE http_tool_specs (
    tool_id uuid PRIMARY KEY REFERENCES tools(id) ON DELETE CASCADE,
    method text NOT NULL CHECK (method IN ('GET', 'POST', 'PUT', 'PATCH', 'DELETE')),
    path_template text NOT NULL CHECK (left(path_template, 1) = '/'),
    query_template jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(query_template) = 'object'),
    headers_template jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(headers_template) = 'object'),
    body_template jsonb,
    timeout_ms integer NOT NULL DEFAULT 15000 CHECK (timeout_ms BETWEEN 100 AND 60000),
    max_response_bytes integer NOT NULL DEFAULT 1048576 CHECK (max_response_bytes BETWEEN 1024 AND 4194304)
);

CREATE TABLE command_tool_specs (
    tool_id uuid PRIMARY KEY REFERENCES tools(id) ON DELETE CASCADE,
    executable text NOT NULL CHECK (length(executable) BETWEEN 1 AND 1024),
    args_template jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(args_template) = 'array'),
    stdin_mode text NOT NULL DEFAULT 'none' CHECK (stdin_mode IN ('none', 'json')),
    credential_env text CHECK (credential_env IS NULL OR credential_env ~ '^[A-Z_][A-Z0-9_]*$'),
    timeout_ms integer NOT NULL DEFAULT 15000 CHECK (timeout_ms BETWEEN 100 AND 60000),
    max_output_bytes integer NOT NULL DEFAULT 1048576 CHECK (max_output_bytes BETWEEN 1024 AND 4194304)
);

CREATE INDEX tools_kind_idx ON tools (kind) WHERE enabled;
