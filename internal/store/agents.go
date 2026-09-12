package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"

	"github.com/jackc/pgx/v5"
)

const agentColumns = `
	SELECT a.id, a.slug, a.name, a.status, a.created_at,
	       coalesce(i.source_key,''), coalesce(i.runtime,''), coalesce(i.profile,''), coalesce(i.environment,''), coalesce(i.config_path,''),
	       (SELECT count(*) FROM grants g JOIN tools t ON t.id=g.tool_id WHERE g.agent_id=a.id AND t.enabled),
	       (SELECT count(*) FROM agent_tokens k WHERE k.agent_id=a.id AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at>now())),
	       (SELECT max(k.last_used_at) FROM agent_tokens k WHERE k.agent_id=a.id)
	FROM agents a LEFT JOIN agent_installations i ON i.agent_id=a.id`

func scanAgent(row pgx.Row) (Agent, error) {
	var a Agent
	err := row.Scan(&a.ID, &a.Slug, &a.Name, &a.Status, &a.CreatedAt, &a.SourceKey, &a.Runtime, &a.Profile, &a.Environment, &a.ConfigPath, &a.ToolCount, &a.TokenCount, &a.LastUsedAt)
	return a, err
}

func (s *Store) ListAgents(ctx context.Context) ([]Agent, error) {
	rows, err := s.pool.Query(ctx, agentColumns+` ORDER BY a.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

func (s *Store) GetAgent(ctx context.Context, id string) (Agent, error) {
	a, err := scanAgent(s.pool.QueryRow(ctx, agentColumns+` WHERE a.id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Agent{}, ErrNotFound
	}
	return a, err
}

func (s *Store) CreateAgent(ctx context.Context, name, slug string) (Agent, string, error) {
	return s.createAgent(ctx, name, slug, "", "", "", "", "")
}

func (s *Store) CreateDiscoveredAgent(ctx context.Context, name, slug, sourceKey, runtime, profile, environment, configPath string) (Agent, string, error) {
	return s.createAgent(ctx, name, slug, sourceKey, runtime, profile, environment, configPath)
}

// EnsureDiscoveredAgent returns the agent recorded for sourceKey, creating it
// when absent. The token is only returned for a newly created agent.
func (s *Store) EnsureDiscoveredAgent(ctx context.Context, name, slug, sourceKey, runtime, profile, environment, configPath string) (Agent, string, bool, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT agent_id FROM agent_installations WHERE source_key=$1`, sourceKey).Scan(&id)
	if err == nil {
		agent, err := s.GetAgent(ctx, id)
		return agent, "", false, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Agent{}, "", false, err
	}
	agent, token, err := s.createAgent(ctx, name, slug, sourceKey, runtime, profile, environment, configPath)
	return agent, token, err == nil, err
}

func (s *Store) createAgent(ctx context.Context, name, slug, sourceKey, runtime, profile, environment, configPath string) (Agent, string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Agent{}, "", err
	}
	defer tx.Rollback(ctx)
	var a Agent
	err = tx.QueryRow(ctx, `INSERT INTO agents(name, slug) VALUES ($1,$2) RETURNING id, slug, name, status, created_at`, name, slug).Scan(&a.ID, &a.Slug, &a.Name, &a.Status, &a.CreatedAt)
	if err != nil {
		return Agent{}, "", err
	}
	if sourceKey != "" {
		if _, err := tx.Exec(ctx, `INSERT INTO agent_installations(agent_id, source_key, runtime, profile, environment, config_path) VALUES ($1,$2,$3,$4,$5,$6)`, a.ID, sourceKey, runtime, profile, environment, configPath); err != nil {
			return Agent{}, "", err
		}
		a.SourceKey, a.Runtime, a.Profile, a.Environment, a.ConfigPath = sourceKey, runtime, profile, environment, configPath
	}
	token, hash, err := newToken()
	if err != nil {
		return Agent{}, "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_tokens(agent_id, token_prefix, token_hash) VALUES ($1,$2,$3)`, a.ID, token[:12], hash); err != nil {
		return Agent{}, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return Agent{}, "", err
	}
	a.TokenCount = 1
	return a, token, nil
}

// newToken returns a fresh agent token and its SHA-256 hash.
func newToken() (string, []byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	token := "tmx_" + base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(token))
	return token, hash[:], nil
}

func (s *Store) IssueAgentToken(ctx context.Context, agentID, label string) (string, error) {
	token, hash, err := newToken()
	if err != nil {
		return "", err
	}
	if label == "" {
		label = "default"
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO agent_tokens(agent_id,label,token_prefix,token_hash) VALUES ($1,$2,$3,$4)`, agentID, label, token[:12], hash); err != nil {
		return "", err
	}
	return token, nil
}

func (s *Store) ListAgentTokens(ctx context.Context, agentID string) ([]AgentToken, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id,label,token_prefix,created_at,last_used_at
		FROM agent_tokens
		WHERE agent_id=$1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())
		ORDER BY created_at DESC`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []AgentToken
	for rows.Next() {
		var token AgentToken
		if err := rows.Scan(&token.ID, &token.Label, &token.Prefix, &token.CreatedAt, &token.LastUsedAt); err != nil {
			return nil, err
		}
		result = append(result, token)
	}
	return result, rows.Err()
}

func (s *Store) RevokeAgentTokenByID(ctx context.Context, agentID, tokenID string) error {
	command, err := s.pool.Exec(ctx, `UPDATE agent_tokens SET revoked_at=now() WHERE id=$1 AND agent_id=$2 AND revoked_at IS NULL`, tokenID, agentID)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) RevokeAgentToken(ctx context.Context, token string) error {
	hash := sha256.Sum256([]byte(token))
	_, err := s.pool.Exec(ctx, `UPDATE agent_tokens SET revoked_at=now() WHERE token_hash=$1 AND revoked_at IS NULL`, hash[:])
	return err
}

func (s *Store) AgentTokenActive(ctx context.Context, agentID, token string) (bool, error) {
	hash := sha256.Sum256([]byte(token))
	var active bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM agent_tokens WHERE agent_id=$1 AND token_hash=$2 AND revoked_at IS NULL
		AND (expires_at IS NULL OR expires_at > now()))`, agentID, hash[:]).Scan(&active)
	return active, err
}

func (s *Store) MatchesAgentToken(ctx context.Context, agentID, tokenID, token string) (bool, error) {
	hash := sha256.Sum256([]byte(token))
	var found bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_tokens k JOIN agents a ON a.id=k.agent_id WHERE k.id::text=$1 AND a.id::text=$2 AND a.status='active' AND k.token_hash=$3 AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at>now()))`, tokenID, agentID, hash[:]).Scan(&found)
	return found, err
}

// AuthenticateAgent resolves a bearer token to its active agent and records the
// use. Disabled agents and revoked or expired tokens fail with ErrNotFound.
func (s *Store) AuthenticateAgent(ctx context.Context, token string) (Agent, error) {
	hash := sha256.Sum256([]byte(token))
	var a Agent
	err := s.pool.QueryRow(ctx, `
		UPDATE agent_tokens t SET last_used_at = now()
		FROM agents a
		WHERE t.agent_id=a.id AND t.token_hash=$1 AND t.revoked_at IS NULL
		  AND (t.expires_at IS NULL OR t.expires_at > now()) AND a.status='active'
		RETURNING a.id, a.slug, a.name, a.status`, hash[:]).Scan(&a.ID, &a.Slug, &a.Name, &a.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Agent{}, ErrNotFound
	}
	return a, err
}

// DisableAgent stops the agent and revokes every token it holds.
func (s *Store) DisableAgent(ctx context.Context, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	command, err := tx.Exec(ctx, `UPDATE agents SET status='disabled' WHERE id=$1 AND status='active'`, id)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_tokens SET revoked_at=now() WHERE agent_id=$1 AND revoked_at IS NULL`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// EnableAgent reactivates a disabled agent. Its previous tokens stay revoked,
// so a new token must be issued afterwards.
func (s *Store) EnableAgent(ctx context.Context, id string) error {
	command, err := s.pool.Exec(ctx, `UPDATE agents SET status='active' WHERE id=$1 AND status='disabled'`, id)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteAgent(ctx context.Context, id string) error {
	command, err := s.pool.Exec(ctx, `DELETE FROM agents WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ConnectionsForAgent lists every connection with this agent's assignment and
// grant count filled in.
func (s *Store) ConnectionsForAgent(ctx context.Context, agentID string) ([]Connection, error) {
	rows, err := s.pool.Query(ctx, connectionColumns+`,
		       (SELECT count(*) FROM grants g JOIN tools t ON t.id=g.tool_id WHERE g.agent_id=$1 AND t.connection_id=c.id AND t.enabled),
		       EXISTS (SELECT 1 FROM agent_connections ac WHERE ac.connection_id=c.id AND ac.agent_id=$1)
		FROM connections c JOIN connectors x ON x.id=c.connector_id
		ORDER BY c.name`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Connection
	for rows.Next() {
		c, err := scanConnection(rows, true)
		if err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

// SetAgentConnection assigns or removes a whole connection. Assignment grants
// every enabled tool now and keeps future discoveries in sync.
func (s *Store) SetAgentConnection(ctx context.Context, agentID, connectionID string, enabled bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT id FROM connections WHERE id=$1 FOR UPDATE`, connectionID); err != nil {
		return err
	}
	if enabled {
		if _, err := tx.Exec(ctx, `INSERT INTO agent_connections(agent_id,connection_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, agentID, connectionID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO grants(agent_id,tool_id) SELECT $1,id FROM tools WHERE connection_id=$2 AND enabled ON CONFLICT DO NOTHING`, agentID, connectionID); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(ctx, `DELETE FROM agent_connections WHERE agent_id=$1 AND connection_id=$2`, agentID, connectionID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM grants USING tools WHERE grants.agent_id=$1 AND grants.tool_id=tools.id AND tools.connection_id=$2`, agentID, connectionID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// SetVisibleGrants replaces the agent's grants for exactly the visible tools:
// visible tools listed in granted are granted, the rest are revoked. Tools
// outside the visible set are untouched, so a filtered page can be saved safely.
func (s *Store) SetVisibleGrants(ctx context.Context, agentID string, visible, granted []string) error {
	if len(visible) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Serialize with catalog refresh. Removing an individual tool changes the
	// connection to individual access so future refreshes cannot re-grant it.
	if _, err := tx.Exec(ctx, `SELECT id FROM connections WHERE id IN
		(SELECT connection_id FROM tools WHERE id=ANY($1::uuid[])) ORDER BY id FOR UPDATE`, visible); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM agent_connections WHERE agent_id=$1 AND connection_id IN
		(SELECT connection_id FROM tools WHERE id=ANY($2::uuid[]) AND NOT(id=ANY(COALESCE($3::uuid[],'{}'))))`, agentID, visible, granted); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM grants WHERE agent_id=$1 AND tool_id=ANY($2::uuid[])`, agentID, visible); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO grants(agent_id,tool_id)
		SELECT $1, t.id FROM tools t
		WHERE t.enabled AND t.id=ANY($2::uuid[]) AND t.id=ANY($3::uuid[])
		ON CONFLICT DO NOTHING`, agentID, visible, granted); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
