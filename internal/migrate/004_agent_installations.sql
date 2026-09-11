CREATE TABLE agent_installations (
    agent_id uuid PRIMARY KEY REFERENCES agents(id) ON DELETE CASCADE,
    source_key text NOT NULL UNIQUE,
    runtime text NOT NULL CHECK (runtime IN ('hermes', 'openclaw')),
    profile text NOT NULL,
    environment text NOT NULL,
    config_path text NOT NULL,
    discovered_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX agent_installations_runtime_idx ON agent_installations (runtime, environment);
