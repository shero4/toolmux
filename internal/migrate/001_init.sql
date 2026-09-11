CREATE TABLE connectors (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9-]*$'),
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
    endpoint_url text NOT NULL,
    transport text NOT NULL DEFAULT 'streamable_http' CHECK (transport = 'streamable_http'),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE connections (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    connector_id uuid NOT NULL REFERENCES connectors(id) ON DELETE RESTRICT,
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
    slug text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9-]*$'),
    auth_method text NOT NULL DEFAULT 'none' CHECK (auth_method IN ('none', 'bearer', 'oauth2')),
    status text NOT NULL DEFAULT 'unchecked' CHECK (status IN ('unchecked', 'checking', 'connected', 'degraded', 'unreachable', 'reauthorization_required', 'disabled')),
    last_checked_at timestamptz,
    last_error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (connector_id, name)
);

CREATE TABLE connection_credentials (
    connection_id uuid PRIMARY KEY REFERENCES connections(id) ON DELETE CASCADE,
    ciphertext bytea NOT NULL,
    nonce bytea NOT NULL,
    key_version smallint NOT NULL DEFAULT 1 CHECK (key_version > 0),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE oauth_configs (
    connection_id uuid PRIMARY KEY REFERENCES connections(id) ON DELETE CASCADE,
    authorization_url text NOT NULL,
    token_url text NOT NULL,
    client_id text NOT NULL,
    scopes text NOT NULL DEFAULT '',
    token_auth_method text NOT NULL DEFAULT 'client_secret_basic'
        CHECK (token_auth_method IN ('client_secret_basic', 'client_secret_post'))
);

CREATE TABLE oauth_states (
    state_hash bytea PRIMARY KEY,
    connection_id uuid NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
    verifier_ciphertext bytea NOT NULL,
    verifier_nonce bytea NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX oauth_states_expiry_idx ON oauth_states (expires_at);

CREATE TABLE agents (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9-]*$'),
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE agent_tokens (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id uuid NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    label text NOT NULL DEFAULT 'default' CHECK (length(label) BETWEEN 1 AND 80),
    token_prefix text NOT NULL,
    token_hash bytea NOT NULL UNIQUE,
    expires_at timestamptz,
    last_used_at timestamptz,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX agent_tokens_active_hash_idx ON agent_tokens (token_hash) WHERE revoked_at IS NULL;

CREATE TABLE tools (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    connection_id uuid NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
    upstream_name text NOT NULL,
    exposed_name text NOT NULL UNIQUE,
    title text,
    description text NOT NULL DEFAULT '',
    input_schema jsonb NOT NULL DEFAULT '{}'::jsonb,
    output_schema jsonb,
    annotations jsonb,
    schema_hash bytea NOT NULL,
    enabled boolean NOT NULL DEFAULT true,
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (connection_id, upstream_name)
);

CREATE INDEX tools_connection_enabled_idx ON tools (connection_id, enabled);

CREATE TABLE grants (
    agent_id uuid NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    tool_id uuid NOT NULL REFERENCES tools(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_id, tool_id)
);

CREATE TABLE connection_checks (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    connection_id uuid NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
    status text NOT NULL,
    reachable boolean NOT NULL,
    protocol_ok boolean NOT NULL,
    authorized boolean NOT NULL,
    capability_ok boolean NOT NULL,
    tool_count integer CHECK (tool_count >= 0),
    detail text,
    checked_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX connection_checks_recent_idx ON connection_checks (connection_id, checked_at DESC);

CREATE TABLE audit_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    agent_id uuid REFERENCES agents(id) ON DELETE SET NULL,
    connection_id uuid REFERENCES connections(id) ON DELETE SET NULL,
    tool_id uuid REFERENCES tools(id) ON DELETE SET NULL,
    method text NOT NULL,
    decision text NOT NULL CHECK (decision IN ('allowed', 'denied', 'error')),
    reason text,
    duration_ms integer CHECK (duration_ms IS NULL OR duration_ms >= 0),
    trace_id uuid NOT NULL DEFAULT gen_random_uuid(),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_events_recent_idx ON audit_events (created_at DESC);
CREATE INDEX audit_events_agent_recent_idx ON audit_events (agent_id, created_at DESC);
