CREATE TABLE users (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    username text NOT NULL UNIQUE CHECK(length(username) BETWEEN 1 AND 120),
    password_hash bytea NOT NULL,
    role text NOT NULL CHECK(role IN ('admin','operator','viewer')),
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO users(username,password_hash,role,created_at)
    SELECT username,password_hash,'admin',created_at FROM admin_account;
ALTER TABLE admin_sessions ADD COLUMN user_id uuid REFERENCES users(id) ON DELETE CASCADE;
UPDATE admin_sessions SET user_id=(SELECT id FROM users LIMIT 1);
ALTER TABLE admin_sessions ALTER COLUMN user_id SET NOT NULL;
CREATE INDEX admin_sessions_user_idx ON admin_sessions(user_id);
DROP TABLE admin_account;
