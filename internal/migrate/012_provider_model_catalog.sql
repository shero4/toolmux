ALTER TABLE provider_models
    ADD COLUMN source text NOT NULL DEFAULT 'manual' CHECK(source IN ('manual','discovered')),
    ADD COLUMN enabled boolean NOT NULL DEFAULT true,
    ADD COLUMN last_seen_at timestamptz;

CREATE INDEX provider_models_available_idx
    ON provider_models(provider_id, enabled, model);
