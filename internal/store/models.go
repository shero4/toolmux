package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type ModelProvider struct {
	Options ProviderOptions
	// Headers are decrypted only for outbound requests, never for page rendering.
	Headers                          map[string]string `json:"-"`
	ID, Name, Slug, BaseURL, Adapter string
	Enabled                          bool
	TimeoutSeconds                   int
	LastCheckedAt                    *time.Time
	LastError                        string
	ModelCount                       int
}

// ProviderOptions contains no credential values. Protocol and authentication
// are independent: an Anthropic-compatible relay can use bearer or custom auth.
type ProviderOptions struct {
	AuthType    string `json:"auth_type,omitempty"`
	AuthHeader  string `json:"auth_header,omitempty"`
	Username    string `json:"username,omitempty"`
	TokenURL    string `json:"token_url,omitempty"`
	ClientID    string `json:"client_id,omitempty"`
	Scope       string `json:"scope,omitempty"`
	Audience    string `json:"audience,omitempty"`
	TokenAuth   string `json:"token_auth,omitempty"`
	APIVersion  string `json:"api_version,omitempty"`
	CatalogMode string `json:"catalog_mode,omitempty"`
}

type ProviderModel struct {
	Model        string
	Source       string
	Enabled      bool
	LastSeenAt   *time.Time
	ReferencedBy int
}

const providerColumns = `SELECT p.id,p.name,p.slug,p.base_url,p.adapter,p.enabled,p.timeout_seconds,p.last_checked_at,p.last_error,(SELECT count(*) FROM provider_models m WHERE m.provider_id=p.id AND m.enabled AND position('*' in m.model)=0),p.options FROM model_providers p`

func scanProvider(row pgx.Row) (ModelProvider, error) {
	var p ModelProvider
	err := row.Scan(&p.ID, &p.Name, &p.Slug, &p.BaseURL, &p.Adapter, &p.Enabled, &p.TimeoutSeconds, &p.LastCheckedAt, &p.LastError, &p.ModelCount, &p.Options)
	return p, err
}

func (s *Store) ListModelProviders(ctx context.Context) ([]ModelProvider, error) {
	rows, err := s.pool.Query(ctx, providerColumns+` ORDER BY lower(p.name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ModelProvider{}
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

func (s *Store) GetModelProvider(ctx context.Context, id string) (ModelProvider, error) {
	p, err := scanProvider(s.pool.QueryRow(ctx, providerColumns+` WHERE p.id::text=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return p, err
}

func (s *Store) ModelProviderKey(ctx context.Context, id string) (string, error) {
	c, err := s.ModelProviderCredential(ctx, id)
	return c.BearerToken, err
}

func (s *Store) ModelProviderCredential(ctx context.Context, id string) (Credential, error) {
	var ciphertext, nonce []byte
	if err := s.pool.QueryRow(ctx, `SELECT ciphertext,nonce FROM model_providers WHERE id=$1`, id).Scan(&ciphertext, &nonce); err != nil {
		return Credential{}, err
	}
	return s.openCredential(ciphertext, nonce)
}

// Saving a provider and any manually entered models is atomic. An empty key
// on edit preserves the stored key; it is never returned in forms.
func (s *Store) SaveModelProvider(ctx context.Context, p ModelProvider, key string, models []string) (string, error) {
	return s.SaveModelProviderConfig(ctx, p, key, nil, false, models)
}

func (s *Store) SaveModelProviderConfig(ctx context.Context, p ModelProvider, key string, headers map[string]string, clear bool, models []string) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	credential := Credential{}
	if p.ID != "" {
		var ciphertext, nonce []byte
		if err = tx.QueryRow(ctx, `SELECT ciphertext,nonce FROM model_providers WHERE id::text=$1 FOR UPDATE`, p.ID).Scan(&ciphertext, &nonce); err != nil {
			return "", err
		}
		if credential, err = s.openCredential(ciphertext, nonce); err != nil {
			return "", err
		}
	}
	if clear {
		credential = Credential{}
	}
	if key != "" {
		credential.BearerToken = key
	}
	if headers != nil {
		credential.Headers = headers
	}
	ciphertext, nonce, err := s.sealCredential(credential)
	if err != nil {
		return "", err
	}
	if p.ID == "" {
		err = tx.QueryRow(ctx, `INSERT INTO model_providers(name,slug,base_url,adapter,ciphertext,nonce,enabled,timeout_seconds,options) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`, p.Name, p.Slug, p.BaseURL, p.Adapter, ciphertext, nonce, p.Enabled, p.TimeoutSeconds, p.Options).Scan(&p.ID)
	} else {
		result, e := tx.Exec(ctx, `UPDATE model_providers SET name=$2,base_url=$3,adapter=$4,enabled=$5,timeout_seconds=$6,
            ciphertext=$7,nonce=$8,options=$9 WHERE id::text=$1`, p.ID, p.Name, p.BaseURL, p.Adapter, p.Enabled, p.TimeoutSeconds, ciphertext, nonce, p.Options)
		err = e
		if err == nil && result.RowsAffected() == 0 {
			err = ErrNotFound
		}
	}
	if err != nil {
		return "", err
	}
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if strings.Contains(model, "*") {
			return "", errors.New("wildcard model IDs cannot be published")
		}
		if _, err = tx.Exec(ctx, `INSERT INTO provider_models(provider_id,model,source,enabled) VALUES($1,$2,'manual',true)
			ON CONFLICT(provider_id,model) DO UPDATE SET source='manual',enabled=true`, p.ID, model); err != nil {
			return "", err
		}
	}
	return p.ID, tx.Commit(ctx)
}

func (s *Store) ProviderModels(ctx context.Context, id string) ([]ProviderModel, error) {
	rows, err := s.pool.Query(ctx, `SELECT m.model,m.source,m.enabled,m.last_seen_at,
		(SELECT count(*) FROM agents a JOIN model_providers p ON p.id=m.provider_id
		 WHERE a.primary_model=p.slug||'/'||m.model OR a.fallback_model=p.slug||'/'||m.model)
		FROM provider_models m WHERE m.provider_id::text=$1 ORDER BY m.model`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ProviderModel{}
	for rows.Next() {
		var m ProviderModel
		if err := rows.Scan(&m.Model, &m.Source, &m.Enabled, &m.LastSeenAt, &m.ReferencedBy); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

func (s *Store) SetProviderModelEnabled(ctx context.Context, providerID, model string, enabled bool) error {
	command, err := s.pool.Exec(ctx, `UPDATE provider_models SET enabled=$3 WHERE provider_id::text=$1 AND model=$2`, providerID, model, enabled)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) AgentsUsingModel(ctx context.Context, selected string) ([]Agent, error) {
	rows, err := s.pool.Query(ctx, agentColumns+` WHERE a.primary_model=$1 OR a.fallback_model=$1 ORDER BY a.name`, selected)
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

func (s *Store) SaveDiscoveredModels(ctx context.Context, id string, models []string, detail string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// A failed discovery never changes the last known-good catalog.
	if detail != "" {
		_, err = tx.Exec(ctx, `UPDATE model_providers SET last_checked_at=now(),last_error=$2 WHERE id=$1`, id, detail)
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	seen := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" || strings.Contains(model, "*") || len(model) > 256 {
			continue
		}
		seen = append(seen, model)
		if _, err = tx.Exec(ctx, `INSERT INTO provider_models(provider_id,model,source,enabled,last_seen_at) VALUES($1,$2,'discovered',true,now())
			ON CONFLICT(provider_id,model) DO UPDATE SET enabled=true,last_seen_at=now()`, id, model); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE provider_models SET enabled=false
		WHERE provider_id=$1 AND source='discovered' AND NOT(model=ANY($2::text[]))`, id, seen); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE model_providers SET last_checked_at=now(),last_error='' WHERE id=$1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AvailableModels is shared by every authenticated agent. Clients choose the
// provider/model ID; Toolmux does not choose a model or rewrite agent settings.
func (s *Store) AvailableModels(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT p.slug||'/'||m.model FROM provider_models m JOIN model_providers p ON p.id=m.provider_id WHERE p.enabled AND m.enabled AND position('*' in m.model)=0 ORDER BY p.slug,m.model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			return nil, err
		}
		result = append(result, model)
	}
	return result, rows.Err()
}

func (s *Store) ResolveModel(ctx context.Context, selected string) (ModelProvider, string, string, error) {
	slug, model, ok := strings.Cut(selected, "/")
	if !ok {
		return ModelProvider{}, "", "", ErrNotFound
	}
	var id string
	err := s.pool.QueryRow(ctx, `SELECT p.id FROM model_providers p JOIN provider_models m ON m.provider_id=p.id WHERE p.slug=$1 AND m.model=$2 AND p.enabled AND m.enabled AND position('*' in m.model)=0`, slug, model).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ModelProvider{}, "", "", ErrNotFound
	}
	if err != nil {
		return ModelProvider{}, "", "", err
	}
	p, err := s.GetModelProvider(ctx, id)
	if err != nil {
		return p, "", "", err
	}
	credential, err := s.ModelProviderCredential(ctx, id)
	p.Headers = credential.Headers
	return p, model, credential.BearerToken, err
}
func (s *Store) RecordModelCall(ctx context.Context, agentID, provider, alias, model, decision, reason string, duration time.Duration, input, output *int64) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO audit_events(agent_id,method,decision,reason,duration_ms,model_provider,model_alias,model_name,input_tokens,output_tokens) VALUES($1,'models/call',$2,$3,$4,$5,$6,$7,$8,$9)`, nullable(agentID), decision, nullable(reason), duration.Milliseconds(), provider, alias, model, input, output)
	return err
}
