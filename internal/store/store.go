package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shero4/toolmux/internal/secretbox"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	pool *pgxpool.Pool
	box  *secretbox.Box
}

type Agent struct {
	ID, Slug, Name, Status                               string
	SourceKey, Runtime, Profile, Environment, ConfigPath string
	ToolCount                                            int
	LastUsedAt                                           *time.Time
}

type AgentToken struct {
	ID, Label, Prefix string
	CreatedAt         time.Time
	LastUsedAt        *time.Time
}

type Connection struct {
	ID, Slug, ConnectorSlug, ConnectorName, Name, Kind, EndpointURL, HealthPath, AuthMethod, AuthName, Status string
	LastCheckedAt                                                                                             *time.Time
	LastError                                                                                                 string
	ToolCount, GrantedCount                                                                                   int
	Assigned                                                                                                  bool
}

type Tool struct {
	ID, ConnectionID, ConnectionName, Kind, UpstreamName, ExposedName string
	Title, Description                                                string
	InputSchema                                                       json.RawMessage
	OutputSchema, Annotations, Icons                                  json.RawMessage
	Enabled, Granted                                                  bool
}

type ToolCall struct {
	Arguments      json.RawMessage
	InputResponses map[string]json.RawMessage
	RequestState   json.RawMessage
}

type HTTPToolSpec struct {
	Method, URLTemplate                          string
	QueryTemplate, HeadersTemplate, BodyTemplate json.RawMessage
	TimeoutMS, MaxResponseBytes                  int
}

type CommandToolSpec struct {
	Executable, WorkingDirectory, StdinMode, CredentialEnv string
	ArgsTemplate                                           json.RawMessage
	TimeoutMS, MaxOutputBytes                              int
}

type Credential struct {
	BearerToken  string            `json:"bearer_token,omitempty"`
	ClientSecret string            `json:"client_secret,omitempty"`
	AccessToken  string            `json:"access_token,omitempty"`
	RefreshToken string            `json:"refresh_token,omitempty"`
	TokenType    string            `json:"token_type,omitempty"`
	ExpiresAt    *time.Time        `json:"expires_at,omitempty"`
	Environment  map[string]string `json:"environment,omitempty"`
}

func (c Credential) Bearer() string {
	if c.AccessToken != "" {
		return c.AccessToken
	}
	return c.BearerToken
}

type OAuthConfig struct {
	AuthorizationURL string
	TokenURL         string
	RegistrationURL  string
	ClientID         string
	Scopes           string
	TokenAuthMethod  string
	RedirectURI      string
}

type MCPStdioSpec struct {
	Executable, WorkingDirectory string
	Args                         json.RawMessage
}

type ImportedConnection struct {
	SourceKey, Kind, ConnectorName, ConnectorSlug string
	EndpointURL, ConnectionName, ConnectionSlug   string
	AuthMethod, AuthName, AgentID                 string
	Credential                                    Credential
	OAuth                                         *OAuthConfig
	Stdio                                         *MCPStdioSpec
}

type AuditEvent struct {
	AgentName, ConnectionName, ToolName, Method, Decision, Reason string
	DurationMS                                                    *int
	CreatedAt                                                     time.Time
}

type Check struct {
	Status                                          string
	Reachable, ProtocolOK, Authorized, CapabilityOK bool
	ToolCount                                       int
	Detail                                          string
}

func Open(ctx context.Context, databaseURL string, box *secretbox.Box) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{pool: pool, box: box}, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Migrate(ctx context.Context, migrations map[int]string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(722834681)`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	versions := make([]int, 0, len(migrations))
	for version := range migrations {
		versions = append(versions, version)
	}
	sort.Ints(versions)
	for _, version := range versions {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := tx.Exec(ctx, migrations[version]); err != nil {
			return fmt.Errorf("apply migration %d: %w", version, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ($1)`, version); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) Summary(ctx context.Context) (agents, connections, tools int, err error) {
	err = s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM agents), (SELECT count(*) FROM connections), (SELECT count(*) FROM tools WHERE enabled)`).Scan(&agents, &connections, &tools)
	return
}

func (s *Store) ListAgents(ctx context.Context) ([]Agent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.slug, a.name, a.status,
		       coalesce(i.source_key,''), coalesce(i.runtime,''), coalesce(i.profile,''), coalesce(i.environment,''), coalesce(i.config_path,''),
		       count(DISTINCT g.tool_id), max(t.last_used_at)
		FROM agents a
		LEFT JOIN agent_installations i ON i.agent_id = a.id
		LEFT JOIN grants g ON g.agent_id = a.id
		LEFT JOIN agent_tokens t ON t.agent_id = a.id
		GROUP BY a.id, i.agent_id ORDER BY a.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Agent
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.ID, &a.Slug, &a.Name, &a.Status, &a.SourceKey, &a.Runtime, &a.Profile, &a.Environment, &a.ConfigPath, &a.ToolCount, &a.LastUsedAt); err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

func (s *Store) CreateAgent(ctx context.Context, name, slug string) (Agent, string, error) {
	return s.createAgent(ctx, name, slug, "", "", "", "", "")
}

func (s *Store) CreateDiscoveredAgent(ctx context.Context, name, slug, sourceKey, runtime, profile, environment, configPath string) (Agent, string, error) {
	return s.createAgent(ctx, name, slug, sourceKey, runtime, profile, environment, configPath)
}

func (s *Store) IssueAgentToken(ctx context.Context, agentID, label string) (string, error) {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", err
	}
	token := "tmx_" + base64.RawURLEncoding.EncodeToString(tokenBytes)
	hash := sha256.Sum256([]byte(token))
	if label == "" {
		label = "default"
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO agent_tokens(agent_id,label,token_prefix,token_hash) VALUES ($1,$2,$3,$4)`, agentID, label, token[:12], hash[:])
	if err != nil {
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

func (s *Store) EnsureDiscoveredAgent(ctx context.Context, name, slug, sourceKey, runtime, profile, environment, configPath string) (Agent, string, bool, error) {
	var agent Agent
	err := s.pool.QueryRow(ctx, `
		SELECT a.id,a.slug,a.name,a.status,i.source_key,i.runtime,i.profile,i.environment,i.config_path
		FROM agent_installations i JOIN agents a ON a.id=i.agent_id WHERE i.source_key=$1`, sourceKey).Scan(
		&agent.ID, &agent.Slug, &agent.Name, &agent.Status, &agent.SourceKey, &agent.Runtime, &agent.Profile, &agent.Environment, &agent.ConfigPath)
	if err == nil {
		return agent, "", false, nil
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
	err = tx.QueryRow(ctx, `INSERT INTO agents(name, slug) VALUES ($1,$2) RETURNING id, slug, name, status`, name, slug).Scan(&a.ID, &a.Slug, &a.Name, &a.Status)
	if err != nil {
		return Agent{}, "", err
	}
	if sourceKey != "" {
		_, err = tx.Exec(ctx, `INSERT INTO agent_installations(agent_id, source_key, runtime, profile, environment, config_path) VALUES ($1,$2,$3,$4,$5,$6)`, a.ID, sourceKey, runtime, profile, environment, configPath)
		if err != nil {
			return Agent{}, "", err
		}
		a.SourceKey, a.Runtime, a.Profile, a.Environment, a.ConfigPath = sourceKey, runtime, profile, environment, configPath
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return Agent{}, "", err
	}
	token := "tmx_" + base64.RawURLEncoding.EncodeToString(tokenBytes)
	hash := sha256.Sum256([]byte(token))
	_, err = tx.Exec(ctx, `INSERT INTO agent_tokens(agent_id, token_prefix, token_hash) VALUES ($1,$2,$3)`, a.ID, token[:12], hash[:])
	if err != nil {
		return Agent{}, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return Agent{}, "", err
	}
	return a, token, nil
}

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

func (s *Store) ListConnections(ctx context.Context) ([]Connection, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.id, c.slug, x.slug, x.name, c.name, x.kind, coalesce(x.endpoint_url,''), x.health_path, c.auth_method, coalesce(c.auth_name,''), c.status,
		       c.last_checked_at, coalesce(c.last_error,''), count(DISTINCT t.id), count(DISTINCT g.tool_id)
		FROM connections c JOIN connectors x ON x.id=c.connector_id
		LEFT JOIN tools t ON t.connection_id=c.id AND t.enabled
		LEFT JOIN grants g ON g.tool_id=t.id
		GROUP BY c.id,x.id
		ORDER BY CASE c.status WHEN 'reauthorization_required' THEN 0 WHEN 'connected' THEN 2 ELSE 1 END, c.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Connection
	for rows.Next() {
		var c Connection
		if err := rows.Scan(&c.ID, &c.Slug, &c.ConnectorSlug, &c.ConnectorName, &c.Name, &c.Kind, &c.EndpointURL, &c.HealthPath, &c.AuthMethod, &c.AuthName, &c.Status, &c.LastCheckedAt, &c.LastError, &c.ToolCount, &c.GrantedCount); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

func (s *Store) ConnectionsForAgent(ctx context.Context, agentID string) ([]Connection, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.id,c.slug,x.slug,x.name,c.name,x.kind,coalesce(x.endpoint_url,''),x.health_path,c.auth_method,coalesce(c.auth_name,''),c.status,
		       c.last_checked_at,coalesce(c.last_error,''),count(DISTINCT t.id),count(DISTINCT g.tool_id),(ac.agent_id IS NOT NULL)
		FROM connections c JOIN connectors x ON x.id=c.connector_id
		LEFT JOIN tools t ON t.connection_id=c.id AND t.enabled
		LEFT JOIN grants g ON g.tool_id=t.id AND g.agent_id=$1
		LEFT JOIN agent_connections ac ON ac.connection_id=c.id AND ac.agent_id=$1
		GROUP BY c.id,x.id,ac.agent_id ORDER BY c.name`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Connection
	for rows.Next() {
		var c Connection
		if err := rows.Scan(&c.ID, &c.Slug, &c.ConnectorSlug, &c.ConnectorName, &c.Name, &c.Kind, &c.EndpointURL, &c.HealthPath, &c.AuthMethod, &c.AuthName, &c.Status, &c.LastCheckedAt, &c.LastError, &c.ToolCount, &c.GrantedCount, &c.Assigned); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

func (s *Store) SetAgentConnection(ctx context.Context, agentID, connectionID string, enabled bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
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

func (s *Store) CreateConnection(ctx context.Context, kind, connectorName, connectorSlug, endpointURL, healthPath, connectionName, connectionSlug, authMethod, authName, secret string, oauth *OAuthConfig) (string, error) {
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
		payload, _ := json.Marshal(credential)
		ciphertext, nonce, err := s.box.Seal(payload)
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO connection_credentials(connection_id,ciphertext,nonce) VALUES ($1,$2,$3)`, connectionID, ciphertext, nonce); err != nil {
			return "", err
		}
	}
	if authMethod == "oauth2" {
		if oauth == nil {
			return "", errors.New("OAuth configuration is required")
		}
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
	var connectorID string
	err = tx.QueryRow(ctx, `
		INSERT INTO connectors(name,slug,kind,endpoint_url,transport) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (slug) DO UPDATE SET name=excluded.name,kind=excluded.kind,endpoint_url=excluded.endpoint_url,transport=excluded.transport
		RETURNING id`, input.ConnectorName, input.ConnectorSlug, input.Kind, nullable(input.EndpointURL), transport).Scan(&connectorID)
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

	if hasCredential(input.Credential) {
		payload, err := json.Marshal(input.Credential)
		if err != nil {
			return "", false, err
		}
		ciphertext, nonce, err := s.box.Seal(payload)
		if err != nil {
			return "", false, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO connection_credentials(connection_id,ciphertext,nonce) VALUES ($1,$2,$3)
			ON CONFLICT (connection_id) DO UPDATE SET ciphertext=excluded.ciphertext,nonce=excluded.nonce,key_version=1,updated_at=now()`, connectionID, ciphertext, nonce); err != nil {
			return "", false, err
		}
	}

	if input.OAuth != nil {
		_, err = tx.Exec(ctx, `INSERT INTO oauth_configs(connection_id,authorization_url,token_url,registration_url,client_id,scopes,token_auth_method,redirect_uri)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT (connection_id) DO UPDATE SET authorization_url=excluded.authorization_url,token_url=excluded.token_url,
			registration_url=excluded.registration_url,client_id=excluded.client_id,scopes=excluded.scopes,
			token_auth_method=excluded.token_auth_method,redirect_uri=excluded.redirect_uri`, connectionID, input.OAuth.AuthorizationURL, input.OAuth.TokenURL,
			input.OAuth.RegistrationURL, nullable(input.OAuth.ClientID), input.OAuth.Scopes, input.OAuth.TokenAuthMethod, input.OAuth.RedirectURI)
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

func (s *Store) GetOAuthConfig(ctx context.Context, connectionID string) (OAuthConfig, error) {
	var config OAuthConfig
	err := s.pool.QueryRow(ctx, `SELECT authorization_url,token_url,registration_url,coalesce(client_id,''),scopes,token_auth_method,redirect_uri FROM oauth_configs WHERE connection_id=$1`, connectionID).Scan(
		&config.AuthorizationURL, &config.TokenURL, &config.RegistrationURL, &config.ClientID, &config.Scopes, &config.TokenAuthMethod, &config.RedirectURI)
	if errors.Is(err, pgx.ErrNoRows) {
		return OAuthConfig{}, ErrNotFound
	}
	return config, err
}

func (s *Store) SaveOAuthClient(ctx context.Context, connectionID, clientID, clientSecret, tokenAuthMethod, redirectURI string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE oauth_configs SET client_id=$2,token_auth_method=$3,redirect_uri=$4 WHERE connection_id=$1`, connectionID, clientID, tokenAuthMethod, redirectURI); err != nil {
		return err
	}
	var credential Credential
	var ciphertext, nonce []byte
	err = tx.QueryRow(ctx, `SELECT ciphertext,nonce FROM connection_credentials WHERE connection_id=$1`, connectionID).Scan(&ciphertext, &nonce)
	if err == nil {
		plaintext, err := s.box.Open(ciphertext, nonce)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(plaintext, &credential); err != nil {
			return err
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	credential.ClientSecret = clientSecret
	payload, err := json.Marshal(credential)
	if err != nil {
		return err
	}
	ciphertext, nonce, err = s.box.Seal(payload)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO connection_credentials(connection_id,ciphertext,nonce) VALUES ($1,$2,$3)
		ON CONFLICT (connection_id) DO UPDATE SET ciphertext=excluded.ciphertext,nonce=excluded.nonce,key_version=1,updated_at=now()`, connectionID, ciphertext, nonce); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) SaveCredential(ctx context.Context, connectionID string, credential Credential) error {
	payload, err := json.Marshal(credential)
	if err != nil {
		return err
	}
	ciphertext, nonce, err := s.box.Seal(payload)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO connection_credentials(connection_id,ciphertext,nonce) VALUES ($1,$2,$3) ON CONFLICT (connection_id) DO UPDATE SET ciphertext=excluded.ciphertext,nonce=excluded.nonce,key_version=1,updated_at=now()`, connectionID, ciphertext, nonce)
	return err
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

func (s *Store) ConsumeOAuthState(ctx context.Context, state string) (string, string, error) {
	hash := sha256.Sum256([]byte(state))
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx)
	var connectionID string
	var ciphertext, nonce []byte
	err = tx.QueryRow(ctx, `DELETE FROM oauth_states WHERE state_hash=$1 AND expires_at>now() RETURNING connection_id,verifier_ciphertext,verifier_nonce`, hash[:]).Scan(&connectionID, &ciphertext, &nonce)
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
	if err := tx.Commit(ctx); err != nil {
		return "", "", err
	}
	return connectionID, string(verifier), nil
}

func (s *Store) GetConnection(ctx context.Context, id string) (Connection, Credential, error) {
	var c Connection
	var ciphertext, nonce []byte
	err := s.pool.QueryRow(ctx, `
		SELECT c.id,c.slug,x.slug,x.name,c.name,x.kind,coalesce(x.endpoint_url,''),x.health_path,c.auth_method,coalesce(c.auth_name,''),c.status,c.last_checked_at,coalesce(c.last_error,''),
		       coalesce(k.ciphertext,decode('','hex')),coalesce(k.nonce,decode('','hex'))
		FROM connections c JOIN connectors x ON x.id=c.connector_id
		LEFT JOIN connection_credentials k ON k.connection_id=c.id WHERE c.id=$1`, id).Scan(
		&c.ID, &c.Slug, &c.ConnectorSlug, &c.ConnectorName, &c.Name, &c.Kind, &c.EndpointURL, &c.HealthPath, &c.AuthMethod, &c.AuthName, &c.Status, &c.LastCheckedAt, &c.LastError, &ciphertext, &nonce)
	if errors.Is(err, pgx.ErrNoRows) {
		return Connection{}, Credential{}, ErrNotFound
	}
	if err != nil {
		return Connection{}, Credential{}, err
	}
	var credential Credential
	if len(ciphertext) > 0 {
		plaintext, err := s.box.Open(ciphertext, nonce)
		if err != nil {
			return Connection{}, Credential{}, err
		}
		if err := json.Unmarshal(plaintext, &credential); err != nil {
			return Connection{}, Credential{}, err
		}
	}
	return c, credential, nil
}

func (s *Store) SaveCheck(ctx context.Context, connectionID string, check Check) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `INSERT INTO connection_checks(connection_id,status,reachable,protocol_ok,authorized,capability_ok,tool_count,detail) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, connectionID, check.Status, check.Reachable, check.ProtocolOK, check.Authorized, check.CapabilityOK, check.ToolCount, nullable(check.Detail))
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE connections SET status=$2,last_checked_at=now(),last_error=$3 WHERE id=$1`, connectionID, check.Status, nullable(check.Detail))
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ReconcileTools(ctx context.Context, connection Connection, tools []Tool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE tools SET enabled=false WHERE connection_id=$1`, connection.ID); err != nil {
		return err
	}
	for _, tool := range tools {
		schemaHash := sha256.Sum256([]byte(string(tool.InputSchema) + string(tool.OutputSchema) + string(tool.Annotations) + string(tool.Icons)))
		exposed := exposedToolName(connection.Slug, tool.UpstreamName)
		_, err := tx.Exec(ctx, `
			INSERT INTO tools(connection_id,upstream_name,exposed_name,title,description,input_schema,output_schema,annotations,icons,schema_hash,enabled,last_seen_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,true,now())
			ON CONFLICT (connection_id,upstream_name) DO UPDATE SET exposed_name=excluded.exposed_name,title=excluded.title,
			 description=excluded.description,input_schema=excluded.input_schema,output_schema=excluded.output_schema,
			 annotations=excluded.annotations,icons=excluded.icons,schema_hash=excluded.schema_hash,enabled=true,last_seen_at=now()`,
			connection.ID, tool.UpstreamName, exposed, nullable(tool.Title), tool.Description, tool.InputSchema, nullableJSON(tool.OutputSchema), nullableJSON(tool.Annotations), nullableJSON(tool.Icons), schemaHash[:])
		if err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO grants(agent_id,tool_id)
		SELECT a.agent_id,t.id FROM agent_connections a JOIN tools t ON t.connection_id=a.connection_id
		WHERE a.connection_id=$1 AND t.enabled
		ON CONFLICT DO NOTHING`, connection.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ToolsForAgent(ctx context.Context, agentID string) ([]Tool, error) {
	return s.queryTools(ctx, `
		SELECT t.id,t.connection_id,c.name,t.kind,t.upstream_name,t.exposed_name,coalesce(t.title,''),t.description,t.input_schema,
		       coalesce(t.output_schema,'null'::jsonb),coalesce(t.annotations,'null'::jsonb),coalesce(t.icons,'null'::jsonb),t.enabled,true
		FROM grants g JOIN tools t ON t.id=g.tool_id JOIN connections c ON c.id=t.connection_id
		WHERE g.agent_id=$1 AND t.enabled AND c.status <> 'disabled' ORDER BY t.exposed_name`, agentID)
}

func (s *Store) ToolsForGrantPage(ctx context.Context, agentID string) ([]Tool, error) {
	return s.queryTools(ctx, `
		SELECT t.id,t.connection_id,c.name,t.kind,t.upstream_name,t.exposed_name,coalesce(t.title,''),t.description,t.input_schema,
		       coalesce(t.output_schema,'null'::jsonb),coalesce(t.annotations,'null'::jsonb),coalesce(t.icons,'null'::jsonb),t.enabled,(g.agent_id IS NOT NULL)
		FROM tools t JOIN connections c ON c.id=t.connection_id LEFT JOIN grants g ON g.tool_id=t.id AND g.agent_id=$1
		WHERE t.enabled ORDER BY c.name,t.exposed_name`, agentID)
}

func (s *Store) SearchTools(ctx context.Context, query, kind, connectionID string, limit, offset int) ([]Tool, int, error) {
	filter := `t.enabled AND ($1='' OR t.exposed_name ILIKE '%'||$1||'%' OR c.name ILIKE '%'||$1||'%' OR t.description ILIKE '%'||$1||'%')
		AND ($2='' OR t.kind=$2) AND ($3='' OR t.connection_id::text=$3)`
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM tools t JOIN connections c ON c.id=t.connection_id WHERE `+filter, query, kind, connectionID).Scan(&total); err != nil {
		return nil, 0, err
	}
	tools, err := s.queryTools(ctx, `
		SELECT t.id,t.connection_id,c.name,t.kind,t.upstream_name,t.exposed_name,coalesce(t.title,''),t.description,t.input_schema,
		       coalesce(t.output_schema,'null'::jsonb),coalesce(t.annotations,'null'::jsonb),coalesce(t.icons,'null'::jsonb),t.enabled,false
		FROM tools t JOIN connections c ON c.id=t.connection_id WHERE `+filter+`
		ORDER BY c.name,t.exposed_name LIMIT $4 OFFSET $5`, query, kind, connectionID, limit, offset)
	return tools, total, err
}

func (s *Store) SearchToolsForAgentGrant(ctx context.Context, agentID, query, kind, connectionID string, limit, offset int) ([]Tool, int, error) {
	countFilter := `t.enabled AND ($1='' OR t.exposed_name ILIKE '%'||$1||'%' OR c.name ILIKE '%'||$1||'%' OR t.description ILIKE '%'||$1||'%')
		AND ($2='' OR t.kind=$2) AND ($3='' OR t.connection_id::text=$3)`
	filter := `t.enabled AND ($2='' OR t.exposed_name ILIKE '%'||$2||'%' OR c.name ILIKE '%'||$2||'%' OR t.description ILIKE '%'||$2||'%')
		AND ($3='' OR t.kind=$3) AND ($4='' OR t.connection_id::text=$4)`
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM tools t JOIN connections c ON c.id=t.connection_id WHERE `+countFilter, query, kind, connectionID).Scan(&total); err != nil {
		return nil, 0, err
	}
	tools, err := s.queryTools(ctx, `
		SELECT t.id,t.connection_id,c.name,t.kind,t.upstream_name,t.exposed_name,coalesce(t.title,''),t.description,t.input_schema,
		       coalesce(t.output_schema,'null'::jsonb),coalesce(t.annotations,'null'::jsonb),coalesce(t.icons,'null'::jsonb),t.enabled,(g.agent_id IS NOT NULL)
		FROM tools t JOIN connections c ON c.id=t.connection_id LEFT JOIN grants g ON g.tool_id=t.id AND g.agent_id=$1
		WHERE `+filter+` ORDER BY c.name,t.exposed_name LIMIT $5 OFFSET $6`, agentID, query, kind, connectionID, limit, offset)
	return tools, total, err
}

func (s *Store) queryTools(ctx context.Context, query string, args ...any) ([]Tool, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Tool
	for rows.Next() {
		var t Tool
		if err := rows.Scan(&t.ID, &t.ConnectionID, &t.ConnectionName, &t.Kind, &t.UpstreamName, &t.ExposedName, &t.Title, &t.Description, &t.InputSchema, &t.OutputSchema, &t.Annotations, &t.Icons, &t.Enabled, &t.Granted); err != nil {
			return nil, err
		}
		result = append(result, t)
	}
	return result, rows.Err()
}

func (s *Store) ResolveGrantedTool(ctx context.Context, agentID, exposedName string) (Tool, Connection, Credential, error) {
	var t Tool
	var c Connection
	var ciphertext, nonce []byte
	err := s.pool.QueryRow(ctx, `
		SELECT t.id,t.connection_id,c.name,t.kind,t.upstream_name,t.exposed_name,coalesce(t.title,''),t.description,t.input_schema,
		       coalesce(t.output_schema,'null'::jsonb),coalesce(t.annotations,'null'::jsonb),coalesce(t.icons,'null'::jsonb),t.enabled,true,
		       c.id,c.slug,x.slug,x.name,c.name,x.kind,coalesce(x.endpoint_url,''),x.health_path,c.auth_method,coalesce(c.auth_name,''),c.status,c.last_checked_at,coalesce(c.last_error,''),
		       coalesce(k.ciphertext,decode('','hex')),coalesce(k.nonce,decode('','hex'))
		FROM grants g JOIN tools t ON t.id=g.tool_id JOIN connections c ON c.id=t.connection_id
		JOIN connectors x ON x.id=c.connector_id LEFT JOIN connection_credentials k ON k.connection_id=c.id
		WHERE g.agent_id=$1 AND t.exposed_name=$2 AND t.enabled AND c.status <> 'disabled'`, agentID, exposedName).Scan(
		&t.ID, &t.ConnectionID, &t.ConnectionName, &t.Kind, &t.UpstreamName, &t.ExposedName, &t.Title, &t.Description, &t.InputSchema, &t.OutputSchema, &t.Annotations, &t.Icons, &t.Enabled, &t.Granted,
		&c.ID, &c.Slug, &c.ConnectorSlug, &c.ConnectorName, &c.Name, &c.Kind, &c.EndpointURL, &c.HealthPath, &c.AuthMethod, &c.AuthName, &c.Status, &c.LastCheckedAt, &c.LastError, &ciphertext, &nonce)
	if errors.Is(err, pgx.ErrNoRows) {
		return Tool{}, Connection{}, Credential{}, ErrNotFound
	}
	if err != nil {
		return Tool{}, Connection{}, Credential{}, err
	}
	var credential Credential
	if len(ciphertext) > 0 {
		plaintext, err := s.box.Open(ciphertext, nonce)
		if err != nil {
			return Tool{}, Connection{}, Credential{}, err
		}
		if err := json.Unmarshal(plaintext, &credential); err != nil {
			return Tool{}, Connection{}, Credential{}, err
		}
	}
	return t, c, credential, nil
}

func (s *Store) ListTools(ctx context.Context) ([]Tool, error) {
	return s.queryTools(ctx, `
		SELECT t.id,t.connection_id,c.name,t.kind,t.upstream_name,t.exposed_name,coalesce(t.title,''),t.description,t.input_schema,
		       coalesce(t.output_schema,'null'::jsonb),coalesce(t.annotations,'null'::jsonb),coalesce(t.icons,'null'::jsonb),t.enabled,false
		FROM tools t JOIN connections c ON c.id=t.connection_id
		WHERE t.enabled ORDER BY c.name,t.exposed_name`)
}

func (s *Store) CreateHTTPTool(ctx context.Context, connectionID, name, title, description string, inputSchema json.RawMessage, spec HTTPToolSpec) error {
	return s.createDeclaredTool(ctx, connectionID, "http", name, title, description, inputSchema, func(tx pgx.Tx, toolID string) error {
		_, err := tx.Exec(ctx, `INSERT INTO http_tool_specs(tool_id,method,url_template,query_template,headers_template,body_template,timeout_ms,max_response_bytes)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, toolID, spec.Method, spec.URLTemplate, spec.QueryTemplate, spec.HeadersTemplate, nullableJSON(spec.BodyTemplate), spec.TimeoutMS, spec.MaxResponseBytes)
		return err
	})
}

func (s *Store) CreateCommandTool(ctx context.Context, connectionID, name, title, description string, inputSchema json.RawMessage, spec CommandToolSpec) error {
	return s.createDeclaredTool(ctx, connectionID, "command", name, title, description, inputSchema, func(tx pgx.Tx, toolID string) error {
		_, err := tx.Exec(ctx, `INSERT INTO command_tool_specs(tool_id,executable,working_directory,args_template,stdin_mode,credential_env,timeout_ms,max_output_bytes)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, toolID, spec.Executable, nullable(spec.WorkingDirectory), spec.ArgsTemplate, spec.StdinMode, nullable(spec.CredentialEnv), spec.TimeoutMS, spec.MaxOutputBytes)
		return err
	})
}

func (s *Store) EnsureCommandTool(ctx context.Context, connectionID, name, title, description string, inputSchema json.RawMessage, spec CommandToolSpec) error {
	connection, _, err := s.GetConnection(ctx, connectionID)
	if err != nil {
		return err
	}
	if connection.Kind != "command" {
		return errors.New("tool kind does not match connection kind")
	}
	hash := sha256.Sum256(inputSchema)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var toolID string
	err = tx.QueryRow(ctx, `INSERT INTO tools(connection_id,kind,upstream_name,exposed_name,title,description,input_schema,schema_hash)
		VALUES ($1,'command',$2,$3,$4,$5,$6,$7)
		ON CONFLICT (connection_id,upstream_name) DO UPDATE SET exposed_name=excluded.exposed_name,title=excluded.title,
		description=excluded.description,input_schema=excluded.input_schema,schema_hash=excluded.schema_hash,enabled=true,last_seen_at=now()
		RETURNING id`, connectionID, name, exposedToolName(connection.Slug, name), nullable(title), description, inputSchema, hash[:]).Scan(&toolID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO command_tool_specs(tool_id,executable,working_directory,args_template,stdin_mode,credential_env,timeout_ms,max_output_bytes)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (tool_id) DO UPDATE SET executable=excluded.executable,working_directory=excluded.working_directory,
		args_template=excluded.args_template,stdin_mode=excluded.stdin_mode,credential_env=excluded.credential_env,
		timeout_ms=excluded.timeout_ms,max_output_bytes=excluded.max_output_bytes`, toolID, spec.Executable, nullable(spec.WorkingDirectory), spec.ArgsTemplate, spec.StdinMode, nullable(spec.CredentialEnv), spec.TimeoutMS, spec.MaxOutputBytes)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO grants(agent_id,tool_id) SELECT agent_id,$2 FROM agent_connections WHERE connection_id=$1 ON CONFLICT DO NOTHING`, connectionID, toolID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) createDeclaredTool(ctx context.Context, connectionID, kind, name, title, description string, inputSchema json.RawMessage, addSpec func(pgx.Tx, string) error) error {
	connection, _, err := s.GetConnection(ctx, connectionID)
	if err != nil {
		return err
	}
	if (kind == "http" && connection.Kind != "http_api") || (kind == "command" && connection.Kind != "command") {
		return errors.New("tool kind does not match connection kind")
	}
	hash := sha256.Sum256(inputSchema)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var toolID string
	err = tx.QueryRow(ctx, `INSERT INTO tools(connection_id,kind,upstream_name,exposed_name,title,description,input_schema,schema_hash)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`, connectionID, kind, name, exposedToolName(connection.Slug, name), nullable(title), description, inputSchema, hash[:]).Scan(&toolID)
	if err != nil {
		return err
	}
	if err := addSpec(tx, toolID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) GetHTTPToolSpec(ctx context.Context, toolID string) (HTTPToolSpec, error) {
	var spec HTTPToolSpec
	err := s.pool.QueryRow(ctx, `SELECT method,url_template,query_template,headers_template,coalesce(body_template,'null'::jsonb),timeout_ms,max_response_bytes FROM http_tool_specs WHERE tool_id=$1`, toolID).Scan(
		&spec.Method, &spec.URLTemplate, &spec.QueryTemplate, &spec.HeadersTemplate, &spec.BodyTemplate, &spec.TimeoutMS, &spec.MaxResponseBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return HTTPToolSpec{}, ErrNotFound
	}
	return spec, err
}

func (s *Store) GetCommandToolSpec(ctx context.Context, toolID string) (CommandToolSpec, error) {
	var spec CommandToolSpec
	err := s.pool.QueryRow(ctx, `SELECT executable,coalesce(working_directory,''),args_template,stdin_mode,coalesce(credential_env,''),timeout_ms,max_output_bytes FROM command_tool_specs WHERE tool_id=$1`, toolID).Scan(
		&spec.Executable, &spec.WorkingDirectory, &spec.ArgsTemplate, &spec.StdinMode, &spec.CredentialEnv, &spec.TimeoutMS, &spec.MaxOutputBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return CommandToolSpec{}, ErrNotFound
	}
	return spec, err
}

func (s *Store) GetMCPStdioSpec(ctx context.Context, connectionID string) (MCPStdioSpec, error) {
	var spec MCPStdioSpec
	err := s.pool.QueryRow(ctx, `SELECT executable,args,working_directory FROM mcp_stdio_specs WHERE connection_id=$1`, connectionID).Scan(
		&spec.Executable, &spec.Args, &spec.WorkingDirectory)
	if errors.Is(err, pgx.ErrNoRows) {
		return MCPStdioSpec{}, ErrNotFound
	}
	return spec, err
}

func (s *Store) CountToolsForConnection(ctx context.Context, connectionID string) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM tools WHERE connection_id=$1 AND enabled`, connectionID).Scan(&count)
	return count, err
}

func (s *Store) CommandSpecsForConnection(ctx context.Context, connectionID string) ([]CommandToolSpec, error) {
	rows, err := s.pool.Query(ctx, `SELECT s.executable,coalesce(s.working_directory,''),s.args_template,s.stdin_mode,coalesce(s.credential_env,''),s.timeout_ms,s.max_output_bytes
		FROM command_tool_specs s JOIN tools t ON t.id=s.tool_id WHERE t.connection_id=$1 AND t.enabled ORDER BY t.exposed_name`, connectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var specs []CommandToolSpec
	for rows.Next() {
		var spec CommandToolSpec
		if err := rows.Scan(&spec.Executable, &spec.WorkingDirectory, &spec.ArgsTemplate, &spec.StdinMode, &spec.CredentialEnv, &spec.TimeoutMS, &spec.MaxOutputBytes); err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	return specs, rows.Err()
}

func (s *Store) SetGrants(ctx context.Context, agentID string, toolIDs []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM grants WHERE agent_id=$1`, agentID); err != nil {
		return err
	}
	for _, id := range toolIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO grants(agent_id,tool_id) SELECT $1,id FROM tools WHERE id=$2 AND enabled ON CONFLICT DO NOTHING`, agentID, id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) SetVisibleGrants(ctx context.Context, agentID string, visibleToolIDs, grantedToolIDs []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if len(visibleToolIDs) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM grants WHERE agent_id=$1 AND tool_id=ANY($2::uuid[])`, agentID, visibleToolIDs); err != nil {
			return err
		}
	}
	visible := make(map[string]struct{}, len(visibleToolIDs))
	for _, id := range visibleToolIDs {
		visible[id] = struct{}{}
	}
	for _, id := range grantedToolIDs {
		if _, ok := visible[id]; !ok {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO grants(agent_id,tool_id) SELECT $1,id FROM tools WHERE id=$2 AND enabled ON CONFLICT DO NOTHING`, agentID, id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) GetAgent(ctx context.Context, id string) (Agent, error) {
	var a Agent
	err := s.pool.QueryRow(ctx, `
		SELECT a.id,a.slug,a.name,a.status,coalesce(i.source_key,''),coalesce(i.runtime,''),coalesce(i.profile,''),coalesce(i.environment,''),coalesce(i.config_path,'')
		FROM agents a LEFT JOIN agent_installations i ON i.agent_id=a.id WHERE a.id=$1`, id).
		Scan(&a.ID, &a.Slug, &a.Name, &a.Status, &a.SourceKey, &a.Runtime, &a.Profile, &a.Environment, &a.ConfigPath)
	if errors.Is(err, pgx.ErrNoRows) {
		return Agent{}, ErrNotFound
	}
	return a, err
}

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

func (s *Store) RecordAudit(ctx context.Context, agentID, connectionID, toolID, method, decision, reason string, duration time.Duration) error {
	ms := int(duration.Milliseconds())
	_, err := s.pool.Exec(ctx, `INSERT INTO audit_events(agent_id,connection_id,tool_id,method,decision,reason,duration_ms) VALUES ($1,$2,$3,$4,$5,$6,$7)`, nullID(agentID), nullID(connectionID), nullID(toolID), method, decision, nullable(reason), ms)
	return err
}

func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditEvent, error) {
	rows, err := s.pool.Query(ctx, `
	SELECT coalesce(a.name,'Unknown'),coalesce(c.name,'—'),coalesce(t.exposed_name,'—'),e.method,e.decision,coalesce(e.reason,''),e.duration_ms,e.created_at
	FROM audit_events e LEFT JOIN agents a ON a.id=e.agent_id LEFT JOIN connections c ON c.id=e.connection_id LEFT JOIN tools t ON t.id=e.tool_id ORDER BY e.created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []AuditEvent
	for rows.Next() {
		var e AuditEvent
		if err := rows.Scan(&e.AgentName, &e.ConnectionName, &e.ToolName, &e.Method, &e.Decision, &e.Reason, &e.DurationMS, &e.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

func (s *Store) SearchAudit(ctx context.Context, query, decision, agentID string, limit, offset int) ([]AuditEvent, int, error) {
	filter := `($1='' OR coalesce(a.name,'') ILIKE '%'||$1||'%' OR coalesce(c.name,'') ILIKE '%'||$1||'%' OR coalesce(t.exposed_name,'') ILIKE '%'||$1||'%')
		AND ($2='' OR e.decision=$2) AND ($3='' OR e.agent_id::text=$3)`
	joins := ` FROM audit_events e LEFT JOIN agents a ON a.id=e.agent_id LEFT JOIN connections c ON c.id=e.connection_id LEFT JOIN tools t ON t.id=e.tool_id `
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*)`+joins+`WHERE `+filter, query, decision, agentID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx, `SELECT coalesce(a.name,'Unknown'),coalesce(c.name,'—'),coalesce(t.exposed_name,'—'),e.method,e.decision,coalesce(e.reason,''),e.duration_ms,e.created_at`+joins+`WHERE `+filter+` ORDER BY e.created_at DESC LIMIT $4 OFFSET $5`, query, decision, agentID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var result []AuditEvent
	for rows.Next() {
		var event AuditEvent
		if err := rows.Scan(&event.AgentName, &event.ConnectionName, &event.ToolName, &event.Method, &event.Decision, &event.Reason, &event.DurationMS, &event.CreatedAt); err != nil {
			return nil, 0, err
		}
		result = append(result, event)
	}
	return result, total, rows.Err()
}

func Slug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	dash := false
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if b.Len() > 0 && !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
func sanitizeTool(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}
func exposedToolName(connectionSlug, upstream string) string {
	name := connectionSlug + "__" + sanitizeTool(upstream)
	if len(name) <= 128 {
		return name
	}
	hash := sha256.Sum256([]byte(name))
	suffix := "_" + hex.EncodeToString(hash[:4])
	return name[:128-len(suffix)] + suffix
}
func HashForDisplay(value []byte) string { return hex.EncodeToString(value)[:12] }
func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}
func nullableJSON(v json.RawMessage) any {
	if len(v) == 0 || string(v) == "null" {
		return nil
	}
	return v
}
func hasCredential(credential Credential) bool {
	return credential.BearerToken != "" || credential.ClientSecret != "" || credential.AccessToken != "" || credential.RefreshToken != "" || len(credential.Environment) > 0
}
func nullID(v string) any {
	if v == "" {
		return nil
	}
	return v
}
