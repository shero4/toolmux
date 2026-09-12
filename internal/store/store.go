// Package store is the PostgreSQL persistence layer. It owns the relational
// model, credential encryption at rest, and every query the service runs.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shero4/toolmux/internal/secretbox"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	pool *pgxpool.Pool
	box  *secretbox.Box
}

// Agent is a machine identity. Runtime fields are populated when the agent was
// discovered from a Hermes or OpenClaw installation.
type Agent struct {
	ID, Slug, Name, Status                               string
	SourceKey, Runtime, Profile, Environment, ConfigPath string
	ToolCount, TokenCount                                int
	LastUsedAt                                           *time.Time
	CreatedAt                                            time.Time
}

type AgentToken struct {
	ID, Label, Prefix string
	CreatedAt         time.Time
	LastUsedAt        *time.Time
}

// Connection is an authorized account or environment on a connector.
//
// ToolCount is always the number of enabled tools. AgentCount is the number of
// agents holding at least one grant (list views). GrantedCount and Assigned are
// only populated by ConnectionsForAgent and describe one agent's access.
type Connection struct {
	ID, Slug, ConnectorSlug, ConnectorName, Name, Kind string
	EndpointURL, HealthPath, AuthMethod, AuthName      string
	Status, SourceKey, LastError                       string
	LastCheckedAt                                      *time.Time
	CreatedAt                                          time.Time
	ToolCount, GrantedCount, AgentCount                int
	Assigned                                           bool
}

// AgentAccess describes one agent's relationship to a connection.
type AgentAccess struct {
	Agent
	Assigned     bool
	GrantedCount int
}

type Tool struct {
	ID, ConnectionID, ConnectionName, Kind, UpstreamName, ExposedName string
	Title, Description                                                string
	InputSchema                                                       json.RawMessage
	OutputSchema, Annotations, Icons                                  json.RawMessage
	Enabled, Granted                                                  bool
	AgentCount                                                        int
}

// ToolDetail is one tool with its connection, typed specification, and grants.
type ToolDetail struct {
	Tool
	Connection Connection
	HTTP       *HTTPToolSpec
	Command    *CommandToolSpec
	Agents     []Agent
	LastSeenAt time.Time
	CreatedAt  time.Time
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
	Headers      map[string]string `json:"headers,omitempty"`
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

// Empty reports whether the credential carries no secret material.
func (c Credential) Empty() bool {
	return c.BearerToken == "" && c.ClientSecret == "" && c.AccessToken == "" && c.RefreshToken == "" && len(c.Environment) == 0 && len(c.Headers) == 0
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
	AgentID, AgentName, ConnectionID, ConnectionName, ToolID, ToolName string
	Method, Decision, Reason                                           string
	DurationMS                                                         *int
	CreatedAt                                                          time.Time
	ModelName                                                          string
	InputTokens, OutputTokens                                          *int64
}

type AuditStats struct {
	Allowed, Denied, Errors int
}

type Check struct {
	Status                                          string
	Reachable, ProtocolOK, Authorized, CapabilityOK bool
	ToolCount                                       int
	Detail                                          string
}

type CheckRecord struct {
	Check
	CheckedAt time.Time
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

// sealCredential encrypts a credential payload for storage.
func (s *Store) sealCredential(credential Credential) (ciphertext, nonce []byte, err error) {
	payload, err := json.Marshal(credential)
	if err != nil {
		return nil, nil, err
	}
	return s.box.Seal(payload)
}

// openCredential decrypts a stored credential. An empty ciphertext yields an
// empty credential.
func (s *Store) openCredential(ciphertext, nonce []byte) (Credential, error) {
	var credential Credential
	if len(ciphertext) == 0 {
		return credential, nil
	}
	plaintext, err := s.box.Open(ciphertext, nonce)
	if err != nil {
		return Credential{}, err
	}
	if err := json.Unmarshal(plaintext, &credential); err != nil {
		return Credential{}, err
	}
	return credential, nil
}

// Slug lowercases a name and keeps only [a-z0-9-], collapsing runs of other
// characters into single dashes.
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

// exposedToolName is the stable name an agent sees: <connection>__<tool>,
// truncated with a hash suffix when it would exceed 128 characters.
func exposedToolName(connectionSlug, upstream string) string {
	name := connectionSlug + "__" + sanitizeTool(upstream)
	if len(name) <= 128 {
		return name
	}
	hash := sha256.Sum256([]byte(name))
	suffix := "_" + hex.EncodeToString(hash[:4])
	return name[:128-len(suffix)] + suffix
}

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
