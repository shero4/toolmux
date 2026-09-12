CREATE TABLE operation_settings (
 id boolean PRIMARY KEY DEFAULT true CHECK(id),
 interval_seconds integer NOT NULL DEFAULT 180 CHECK(interval_seconds BETWEEN 60 AND 3600),
 webhook_enabled boolean NOT NULL DEFAULT false,
 ciphertext bytea, nonce bytea
);
INSERT INTO operation_settings(id) VALUES(true);
CREATE TABLE operation_states (
 kind text NOT NULL, resource_id text NOT NULL, name text NOT NULL,
 status text NOT NULL, checked_at timestamptz NOT NULL DEFAULT now(), authorization_alerted boolean NOT NULL DEFAULT false,
 PRIMARY KEY(kind,resource_id)
);
CREATE TABLE operation_events (
 id bigserial PRIMARY KEY, created_at timestamptz NOT NULL DEFAULT now(),
 kind text NOT NULL, resource_id text NOT NULL DEFAULT '', name text NOT NULL,
 status text NOT NULL, message text NOT NULL
);
CREATE TABLE webhook_deliveries (
 id bigserial PRIMARY KEY, payload jsonb NOT NULL, attempts integer NOT NULL DEFAULT 0,
 next_attempt timestamptz NOT NULL DEFAULT now(), created_at timestamptz NOT NULL DEFAULT now()
);
