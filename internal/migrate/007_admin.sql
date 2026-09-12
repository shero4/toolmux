CREATE TABLE admin_account (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    username text NOT NULL CHECK (length(username) BETWEEN 1 AND 120),
    password_hash bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE admin_sessions (
    token_hash bytea PRIMARY KEY,
    expires_at timestamptz NOT NULL
);
CREATE INDEX admin_sessions_expiry_idx ON admin_sessions(expires_at);
CREATE TABLE login_attempts (
    address text PRIMARY KEY,
    attempts integer NOT NULL,
    reset_at timestamptz NOT NULL
);
