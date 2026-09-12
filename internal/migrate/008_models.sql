CREATE TABLE model_providers (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL UNIQUE CHECK(length(name) BETWEEN 1 AND 120),
    slug text NOT NULL UNIQUE CHECK(slug ~ '^[a-z0-9][a-z0-9-]{0,79}$'),
    base_url text NOT NULL,
    adapter text NOT NULL DEFAULT 'openai' CHECK(adapter IN ('openai','litellm')),
    ciphertext bytea NOT NULL,
    nonce bytea NOT NULL,
    enabled boolean NOT NULL DEFAULT true,
    timeout_seconds integer NOT NULL DEFAULT 300 CHECK(timeout_seconds BETWEEN 10 AND 3600),
    last_checked_at timestamptz,
    last_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE provider_models (
    provider_id uuid NOT NULL REFERENCES model_providers(id) ON DELETE CASCADE,
    model text NOT NULL CHECK(length(model) BETWEEN 1 AND 256),
    PRIMARY KEY(provider_id,model)
);
ALTER TABLE audit_events
    ADD COLUMN model_provider text,
    ADD COLUMN model_alias text,
    ADD COLUMN model_name text,
    ADD COLUMN input_tokens bigint CHECK(input_tokens>=0),
    ADD COLUMN output_tokens bigint CHECK(output_tokens>=0);
