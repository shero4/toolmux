ALTER TABLE model_providers DROP CONSTRAINT model_providers_adapter_check;
ALTER TABLE model_providers ADD CONSTRAINT model_providers_adapter_check
    CHECK(adapter IN ('openai','anthropic','azure','litellm'));
ALTER TABLE model_providers ADD COLUMN options jsonb NOT NULL DEFAULT '{}';
