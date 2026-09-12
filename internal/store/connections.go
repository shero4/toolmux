package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// connectionColumns selects the shared Connection fields plus tool and agent
// counts. Callers append FROM/JOIN/WHERE clauses (and, for per-agent views,
// two more columns: granted count and assignment).
const connectionColumns = `
	SELECT c.id, c.slug, x.slug, x.name, c.name, x.kind, coalesce(x.endpoint_url,''), x.health_path, c.auth_method, coalesce(c.auth_name,''),
	       c.status, coalesce(c.source_key,''), coalesce(c.last_error,''), c.last_checked_at, c.created_at,
	       (SELECT count(*) FROM tools t WHERE t.connection_id=c.id AND t.enabled),
	       (SELECT count(DISTINCT g.agent_id) FROM grants g JOIN tools t ON t.id=g.tool_id WHERE t.connection_id=c.id AND t.enabled)`

const connectionFrom = ` FROM connections c JOIN connectors x ON x.id=c.connector_id`

func scanConnection(row pgx.Row, perAgent bool) (Connection, error) {
	var c Connection
	targets := []any{&c.ID, &c.Slug, &c.ConnectorSlug, &c.ConnectorName, &c.Name, &c.Kind, &c.EndpointURL, &c.HealthPath, &c.AuthMethod, &c.AuthName,
		&c.Status, &c.SourceKey, &c.LastError, &c.LastCheckedAt, &c.CreatedAt, &c.ToolCount, &c.AgentCount}
	if perAgent {
		targets = append(targets, &c.GrantedCount, &c.Assigned)
	}
	return c, row.Scan(targets...)
}

// ListConnections orders connections so those needing attention come first.
func (s *Store) ListConnections(ctx context.Context) ([]Connection, error) {
	rows, err := s.pool.Query(ctx, connectionColumns+connectionFrom+`
		ORDER BY CASE c.status WHEN 'reauthorization_required' THEN 0 WHEN 'unreachable' THEN 1 WHEN 'degraded' THEN 2 WHEN 'unchecked' THEN 3 WHEN 'connected' THEN 5 ELSE 4 END, c.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Connection
	for rows.Next() {
		c, err := scanConnection(rows, false)
		if err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

// GetConnectionSummary returns one connection without touching its credential.
func (s *Store) GetConnectionSummary(ctx context.Context, id string) (Connection, error) {
	c, err := scanConnection(s.pool.QueryRow(ctx, connectionColumns+connectionFrom+` WHERE c.id=$1`, id), false)
	if errors.Is(err, pgx.ErrNoRows) {
		return Connection{}, ErrNotFound
	}
	return c, err
}

// GetConnection returns a connection together with its decrypted credential.
func (s *Store) GetConnection(ctx context.Context, id string) (Connection, Credential, error) {
	c, err := s.GetConnectionSummary(ctx, id)
	if err != nil {
		return Connection{}, Credential{}, err
	}
	credential, err := s.credential(ctx, s.pool, id)
	if err != nil {
		return Connection{}, Credential{}, err
	}
	return c, credential, nil
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Store) credential(ctx context.Context, q querier, connectionID string) (Credential, error) {
	var ciphertext, nonce []byte
	err := q.QueryRow(ctx, `SELECT ciphertext,nonce FROM connection_credentials WHERE connection_id=$1`, connectionID).Scan(&ciphertext, &nonce)
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, nil
	}
	if err != nil {
		return Credential{}, err
	}
	return s.openCredential(ciphertext, nonce)
}

func (s *Store) CreateConnection(ctx context.Context, kind, connectorName, connectorSlug, endpointURL, healthPath, connectionName, connectionSlug, authMethod, authName, secret string, oauth *OAuthConfig) (string, error) {
	if authMethod == "oauth2" && oauth == nil {
		return "", errors.New("OAuth configuration is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var connectorID string
	err = tx.QueryRow(ctx, `
		INSERT INTO connectors(name,slug,kind,endpoint_url,health_path) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (slug) DO UPDATE SET name=excluded.name, kind=excluded.kind, endpoint_url=excluded.endpoint_url, health_path=excluded.health_path
		RETURNING id`, connectorName, connectorSlug, kind, nullable(endpointURL), healthPath).Scan(&connectorID)
	if err != nil {
		return "", err
	}
	var connectionID string
	err = tx.QueryRow(ctx, `INSERT INTO connections(connector_id,name,slug,auth_method,auth_name) VALUES ($1,$2,$3,$4,$5) RETURNING id`, connectorID, connectionName, connectionSlug, authMethod, nullable(authName)).Scan(&connectionID)
	if err != nil {
		return "", err
	}
	if authMethod != "none" {
		credential := Credential{BearerToken: secret}
		if authMethod == "oauth2" {
			credential = Credential{ClientSecret: secret}
		}
		if err := s.upsertCredential(ctx, tx, connectionID, credential); err != nil {
			return "", err
		}
	}
	if authMethod == "oauth2" {
		_, err := tx.Exec(ctx, `INSERT INTO oauth_configs(connection_id,authorization_url,token_url,client_id,scopes,token_auth_method) VALUES ($1,$2,$3,$4,$5,$6)`, connectionID, oauth.AuthorizationURL, oauth.TokenURL, oauth.ClientID, oauth.Scopes, oauth.TokenAuthMethod)
		if err != nil {
			return "", err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return connectionID, nil
}

// ImportConnection creates or updates a connection keyed by its source. Repeated
// imports are idempotent and never downgrade state Toolmux now owns: a
// connector that already describes a different endpoint gets its own slug, an
// OAuth client Toolmux registered itself is kept, and an older imported token
// never replaces a newer one.
func (s *Store) ImportConnection(ctx context.Context, input ImportedConnection) (string, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx)

	transport := "streamable_http"
	if input.Kind == "mcp_stdio" {
		transport = "stdio"
	}
	connectorSlug, err := resolveConnectorSlug(ctx, tx, input)
	if err != nil {
		return "", false, err
	}
	var connectorID string
	err = tx.QueryRow(ctx, `
		INSERT INTO connectors(name,slug,kind,endpoint_url,transport) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (slug) DO UPDATE SET name=excluded.name
		RETURNING id`, input.ConnectorName, connectorSlug, input.Kind, nullable(input.EndpointURL), transport).Scan(&connectorID)
	if err != nil {
		return "", false, err
	}

	var connectionID string
	var created bool
	err = tx.QueryRow(ctx, `
		INSERT INTO connections(connector_id,name,slug,auth_method,auth_name,source_key)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (source_key) DO UPDATE SET connector_id=excluded.connector_id,name=excluded.name,auth_method=excluded.auth_method,auth_name=excluded.auth_name
		RETURNING id,(xmax=0)`, connectorID, input.ConnectionName, input.ConnectionSlug, input.AuthMethod, nullable(input.AuthName), input.SourceKey).Scan(&connectionID, &created)
	if err != nil {
		return "", false, err
	}

	// An OAuth client that Toolmux registered for its own callback owns the
	// tokens issued to it; imported Hermes tokens belong to a different client.
	toolmuxOwnsOAuth := false
	if input.OAuth != nil && !created {
		var existingRedirect string
		err := tx.QueryRow(ctx, `SELECT redirect_uri FROM oauth_configs WHERE connection_id=$1`, connectionID).Scan(&existingRedirect)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", false, err
		}
		toolmuxOwnsOAuth = existingRedirect != "" && existingRedirect != input.OAuth.RedirectURI
	}

	switch {
	case toolmuxOwnsOAuth:
		// Keep the credential Toolmux manages.
	case input.Credential.Empty():
		if input.AuthMethod == "none" {
			if _, err := tx.Exec(ctx, `DELETE FROM connection_credentials WHERE connection_id=$1`, connectionID); err != nil {
				return "", false, err
			}
		}
	default:
		write := true
		if !created && input.Credential.ExpiresAt != nil {
			existing, err := s.credential(ctx, tx, connectionID)
			if err != nil {
				return "", false, err
			}
			if existing.AccessToken != "" && existing.ExpiresAt != nil && !existing.ExpiresAt.Before(*input.Credential.ExpiresAt) {
				write = false
			}
		}
		if write {
			if err := s.upsertCredential(ctx, tx, connectionID, input.Credential); err != nil {
				return "", false, err
			}
		}
	}

	if input.OAuth != nil {
		if toolmuxOwnsOAuth {
			_, err = tx.Exec(ctx, `UPDATE oauth_configs SET authorization_url=$2,token_url=$3,registration_url=$4,scopes=$5 WHERE connection_id=$1`,
				connectionID, input.OAuth.AuthorizationURL, input.OAuth.TokenURL, input.OAuth.RegistrationURL, input.OAuth.Scopes)
		} else {
			_, err = tx.Exec(ctx, `INSERT INTO oauth_configs(connection_id,authorization_url,token_url,registration_url,client_id,scopes,token_auth_method,redirect_uri)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
				ON CONFLICT (connection_id) DO UPDATE SET authorization_url=excluded.authorization_url,token_url=excluded.token_url,
				registration_url=excluded.registration_url,client_id=excluded.client_id,scopes=excluded.scopes,
				token_auth_method=excluded.token_auth_method,redirect_uri=excluded.redirect_uri`, connectionID, input.OAuth.AuthorizationURL, input.OAuth.TokenURL,
				input.OAuth.RegistrationURL, nullable(input.OAuth.ClientID), input.OAuth.Scopes, input.OAuth.TokenAuthMethod, input.OAuth.RedirectURI)
		}
		if err != nil {
			return "", false, err
		}
	}

	if input.Stdio != nil {
		_, err = tx.Exec(ctx, `INSERT INTO mcp_stdio_specs(connection_id,executable,args,working_directory) VALUES ($1,$2,$3,$4)
			ON CONFLICT (connection_id) DO UPDATE SET executable=excluded.executable,args=excluded.args,working_directory=excluded.working_directory`,
			connectionID, input.Stdio.Executable, input.Stdio.Args, input.Stdio.WorkingDirectory)
		if err != nil {
			return "", false, err
		}
	}

	if input.AgentID != "" {
		if _, err := tx.Exec(ctx, `INSERT INTO agent_connections(agent_id,connection_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, input.AgentID, connectionID); err != nil {
			return "", false, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO grants(agent_id,tool_id) SELECT $1,id FROM tools WHERE connection_id=$2 AND enabled ON CONFLICT DO NOTHING`, input.AgentID, connectionID); err != nil {
			return "", false, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return connectionID, created, nil
}

// resolveConnectorSlug keeps the requested slug unless a connector with that
// slug already describes a different kind or endpoint, in which case the new
// connector gets a derived slug instead of rewriting the existing one.
func resolveConnectorSlug(ctx context.Context, q querier, input ImportedConnection) (string, error) {
	var kind, endpoint string
	err := q.QueryRow(ctx, `SELECT kind, coalesce(endpoint_url,'') FROM connectors WHERE slug=$1`, input.ConnectorSlug).Scan(&kind, &endpoint)
	if errors.Is(err, pgx.ErrNoRows) {
		return input.ConnectorSlug, nil
	}
	if err != nil {
		return "", err
	}
	if kind == input.Kind && endpoint == input.EndpointURL {
		return input.ConnectorSlug, nil
	}
	sum := sha256.Sum256([]byte(input.Kind + "\x00" + input.EndpointURL))
	return input.ConnectorSlug + "-" + hex.EncodeToString(sum[:3]), nil
}

func (s *Store) upsertCredential(ctx context.Context, tx pgx.Tx, connectionID string, credential Credential) error {
	ciphertext, nonce, err := s.sealCredential(credential)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO connection_credentials(connection_id,ciphertext,nonce) VALUES ($1,$2,$3)
		ON CONFLICT (connection_id) DO UPDATE SET ciphertext=excluded.ciphertext,nonce=excluded.nonce,key_version=1,updated_at=now()`, connectionID, ciphertext, nonce)
	return err
}

// SaveCredential replaces the stored credential for a connection.
func (s *Store) SaveCredential(ctx context.Context, connectionID string, credential Credential) error {
	ciphertext, nonce, err := s.sealCredential(credential)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO connection_credentials(connection_id,ciphertext,nonce) VALUES ($1,$2,$3) ON CONFLICT (connection_id) DO UPDATE SET ciphertext=excluded.ciphertext,nonce=excluded.nonce,key_version=1,updated_at=now()`, connectionID, ciphertext, nonce)
	return err
}

// DeleteConnection removes a connection, its tools, grants, credentials, and
// check history. The connector is removed too once nothing references it.
func (s *Store) DeleteConnection(ctx context.Context, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var connectorID string
	err = tx.QueryRow(ctx, `DELETE FROM connections WHERE id=$1 RETURNING connector_id`, id).Scan(&connectorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM connectors WHERE id=$1 AND NOT EXISTS (SELECT 1 FROM connections WHERE connector_id=$1)`, connectorID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SetConnectionEnabled pauses or resumes a connection. A disabled connection
// keeps its tools and grants but agents cannot list or call them, and the
// scheduled health check skips it. Resuming leaves it unchecked until the next
// check runs.
func (s *Store) SetConnectionEnabled(ctx context.Context, id string, enabled bool) error {
	status := "disabled"
	if enabled {
		status = "unchecked"
	}
	command, err := s.pool.Exec(ctx, `UPDATE connections SET status=$2, last_error=NULL WHERE id=$1`, id, status)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetOAuthConfig(ctx context.Context, connectionID string) (OAuthConfig, error) {
	var config OAuthConfig
	err := s.pool.QueryRow(ctx, `SELECT authorization_url,token_url,registration_url,coalesce(client_id,''),scopes,token_auth_method,redirect_uri FROM oauth_configs WHERE connection_id=$1`, connectionID).Scan(
		&config.AuthorizationURL, &config.TokenURL, &config.RegistrationURL, &config.ClientID, &config.Scopes, &config.TokenAuthMethod, &config.RedirectURI)
	if errors.Is(err, pgx.ErrNoRows) {
		return OAuthConfig{}, ErrNotFound
	}
	return config, err
}

// SaveOAuthClient records a client Toolmux registered dynamically, merging the
// client secret into the existing credential payload.
func (s *Store) SaveOAuthClient(ctx context.Context, connectionID, clientID, clientSecret, tokenAuthMethod, redirectURI string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE oauth_configs SET client_id=$2,token_auth_method=$3,redirect_uri=$4 WHERE connection_id=$1`, connectionID, clientID, tokenAuthMethod, redirectURI); err != nil {
		return err
	}
	credential, err := s.credential(ctx, tx, connectionID)
	if err != nil {
		return err
	}
	credential.ClientSecret = clientSecret
	if err := s.upsertCredential(ctx, tx, connectionID, credential); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) CreateOAuthState(ctx context.Context, connectionID, state, verifier string, expiresAt time.Time) error {
	hash := sha256.Sum256([]byte(state))
	ciphertext, nonce, err := s.box.Seal([]byte(verifier))
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO oauth_states(state_hash,connection_id,verifier_ciphertext,verifier_nonce,expires_at) VALUES ($1,$2,$3,$4,$5)`, hash[:], connectionID, ciphertext, nonce, expiresAt)
	return err
}

// ConsumeOAuthState atomically retires a pending state and returns its
// connection and PKCE verifier.
func (s *Store) ConsumeOAuthState(ctx context.Context, state string) (string, string, error) {
	hash := sha256.Sum256([]byte(state))
	var connectionID string
	var ciphertext, nonce []byte
	err := s.pool.QueryRow(ctx, `DELETE FROM oauth_states WHERE state_hash=$1 AND expires_at>now() RETURNING connection_id,verifier_ciphertext,verifier_nonce`, hash[:]).Scan(&connectionID, &ciphertext, &nonce)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	verifier, err := s.box.Open(ciphertext, nonce)
	if err != nil {
		return "", "", err
	}
	return connectionID, string(verifier), nil
}

const checksKept = 50

// SaveCheck records a health check result, updates the connection status, and
// trims the history to the most recent entries.
func (s *Store) SaveCheck(ctx context.Context, connectionID string, check Check) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO connection_checks(connection_id,status,reachable,protocol_ok,authorized,capability_ok,tool_count,detail) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		connectionID, check.Status, check.Reachable, check.ProtocolOK, check.Authorized, check.CapabilityOK, check.ToolCount, nullable(check.Detail)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE connections SET status=$2,last_checked_at=now(),last_error=$3 WHERE id=$1`, connectionID, check.Status, nullable(check.Detail)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM connection_checks WHERE connection_id=$1 AND id NOT IN (SELECT id FROM connection_checks WHERE connection_id=$1 ORDER BY checked_at DESC, id DESC LIMIT $2)`, connectionID, checksKept); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ListChecks(ctx context.Context, connectionID string, limit int) ([]CheckRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT status,reachable,protocol_ok,authorized,capability_ok,coalesce(tool_count,0),coalesce(detail,''),checked_at
		FROM connection_checks WHERE connection_id=$1 ORDER BY checked_at DESC, id DESC LIMIT $2`, connectionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []CheckRecord
	for rows.Next() {
		var record CheckRecord
		if err := rows.Scan(&record.Status, &record.Reachable, &record.ProtocolOK, &record.Authorized, &record.CapabilityOK, &record.ToolCount, &record.Detail, &record.CheckedAt); err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

// AgentsForConnection lists agents that are assigned the connection or hold a
// grant on any of its tools.
func (s *Store) AgentsForConnection(ctx context.Context, connectionID string) ([]AgentAccess, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.slug, a.name, a.status, coalesce(i.runtime,''),
		       EXISTS (SELECT 1 FROM agent_connections ac WHERE ac.agent_id=a.id AND ac.connection_id=$1),
		       (SELECT count(*) FROM grants g JOIN tools t ON t.id=g.tool_id WHERE g.agent_id=a.id AND t.connection_id=$1 AND t.enabled)
		FROM agents a LEFT JOIN agent_installations i ON i.agent_id=a.id
		WHERE EXISTS (SELECT 1 FROM agent_connections ac WHERE ac.agent_id=a.id AND ac.connection_id=$1)
		   OR EXISTS (SELECT 1 FROM grants g JOIN tools t ON t.id=g.tool_id WHERE g.agent_id=a.id AND t.connection_id=$1 AND t.enabled)
		ORDER BY a.name`, connectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []AgentAccess
	for rows.Next() {
		var access AgentAccess
		if err := rows.Scan(&access.ID, &access.Slug, &access.Name, &access.Status, &access.Runtime, &access.Assigned, &access.GrantedCount); err != nil {
			return nil, err
		}
		result = append(result, access)
	}
	return result, rows.Err()
}
