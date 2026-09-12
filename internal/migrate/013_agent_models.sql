ALTER TABLE agents
    ADD COLUMN primary_model text NOT NULL DEFAULT '',
    ADD COLUMN fallback_model text NOT NULL DEFAULT '';

CREATE INDEX agents_primary_model_idx ON agents(primary_model) WHERE primary_model <> '';
CREATE INDEX agents_fallback_model_idx ON agents(fallback_model) WHERE fallback_model <> '';
