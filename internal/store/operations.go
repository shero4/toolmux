package store

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"time"
)

type OperationSettings struct {
	IntervalSeconds            int
	WebhookEnabled, HasWebhook bool
}
type OperationEvent struct {
	ID                                      int64
	CreatedAt                               time.Time
	Kind, ResourceID, Name, Status, Message string
}
type OperationState struct {
	Kind, ResourceID, Name, Status string
	CheckedAt                      time.Time
}
type Delivery struct {
	ID       int64
	Payload  json.RawMessage
	Attempts int
}

func (s *Store) OperationSettings(ctx context.Context) (OperationSettings, error) {
	var v OperationSettings
	err := s.pool.QueryRow(ctx, `SELECT interval_seconds,webhook_enabled,ciphertext IS NOT NULL FROM operation_settings WHERE id`).Scan(&v.IntervalSeconds, &v.WebhookEnabled, &v.HasWebhook)
	return v, err
}
func (s *Store) SaveOperationSettings(ctx context.Context, v OperationSettings, endpoint string, clear bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT id FROM operation_settings WHERE id FOR UPDATE`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE operation_settings SET interval_seconds=$1,webhook_enabled=$2 WHERE id`, v.IntervalSeconds, v.WebhookEnabled); err != nil {
		return err
	}
	if clear {
		if _, err = tx.Exec(ctx, `UPDATE operation_settings SET ciphertext=NULL,nonce=NULL,webhook_enabled=false WHERE id`); err != nil {
			return err
		}
	}
	if endpoint != "" {
		c, n, e := s.sealCredential(Credential{BearerToken: endpoint})
		if e != nil {
			return e
		}
		if _, err = tx.Exec(ctx, `UPDATE operation_settings SET ciphertext=$1,nonce=$2 WHERE id`, c, n); err != nil {
			return err
		}
	}
	// Do not send old queued messages to a newly configured destination.
	if _, err = tx.Exec(ctx, `UPDATE operation_states SET authorization_alerted=false`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM webhook_deliveries`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) WebhookURL(ctx context.Context) (string, error) {
	var c, n []byte
	err := s.pool.QueryRow(ctx, `SELECT ciphertext,nonce FROM operation_settings WHERE id AND webhook_enabled`).Scan(&c, &n)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	if err != nil || len(c) == 0 {
		return "", err
	}
	v, err := s.openCredential(c, n)
	return v.BearerToken, err
}
func (s *Store) LogOperation(ctx context.Context, kind, id, name, status, message string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO operation_events(kind,resource_id,name,status,message) VALUES($1,$2,$3,$4,$5)`, kind, id, name, status, message)
	return err
}

// Observe records transitions, not successful heartbeats. Alerts are queued in
// the same transaction, once per transition, and remain durable across restarts.
func (s *Store) Observe(ctx context.Context, kind, id, name, status string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, kind+":"+id); err != nil {
		return err
	}
	var previous string
	var alerted bool
	err = tx.QueryRow(ctx, `SELECT status,authorization_alerted FROM operation_states WHERE kind=$1 AND resource_id=$2`, kind, id).Scan(&previous, &alerted)
	if err != nil && err != pgx.ErrNoRows {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO operation_states(kind,resource_id,name,status) VALUES($1,$2,$3,$4) ON CONFLICT(kind,resource_id) DO UPDATE SET name=excluded.name,status=excluded.status,checked_at=now()`, kind, id, name, status); err != nil {
		return err
	}
	if previous != status {
		message := "Authorization check: " + status
		if previous != "" {
			message = previous + " → " + status
		}
		if _, err = tx.Exec(ctx, `INSERT INTO operation_events(kind,resource_id,name,status,message) VALUES($1,$2,$3,$4,$5)`, kind, id, name, status, message); err != nil {
			return err
		}
	}
	// A transient network failure does not reset an unresolved auth alert.
	if status == "reauthorization_required" && !alerted || status == "connected" && alerted {
		text := fmt.Sprintf("Toolmux: %s (%s) — %s", name, kind, status)
		// Slack incoming webhooks accept text; generic endpoints get the same small payload.
		payload, _ := json.Marshal(map[string]string{"text": text})
		queued, e := tx.Exec(ctx, `INSERT INTO webhook_deliveries(payload) SELECT $1 WHERE EXISTS(SELECT 1 FROM operation_settings WHERE id AND webhook_enabled AND ciphertext IS NOT NULL)`, payload)
		if e != nil {
			return e
		}
		if queued.RowsAffected() > 0 {
			alerted = status == "reauthorization_required"
		}
	}
	if status == "connected" || status == "disabled" {
		alerted = false
	}
	if _, err = tx.Exec(ctx, `UPDATE operation_states SET authorization_alerted=$3 WHERE kind=$1 AND resource_id=$2`, kind, id, alerted); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) OperationEvents(ctx context.Context) ([]OperationEvent, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,created_at,kind,resource_id,name,status,message FROM operation_events ORDER BY id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	v := []OperationEvent{}
	for rows.Next() {
		var e OperationEvent
		if err = rows.Scan(&e.ID, &e.CreatedAt, &e.Kind, &e.ResourceID, &e.Name, &e.Status, &e.Message); err != nil {
			return nil, err
		}
		v = append(v, e)
	}
	return v, rows.Err()
}
func (s *Store) OperationStates(ctx context.Context) ([]OperationState, error) {
	rows, err := s.pool.Query(ctx, `SELECT kind,resource_id,name,status,checked_at FROM operation_states ORDER BY kind,name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	v := []OperationState{}
	for rows.Next() {
		var e OperationState
		if err = rows.Scan(&e.Kind, &e.ResourceID, &e.Name, &e.Status, &e.CheckedAt); err != nil {
			return nil, err
		}
		v = append(v, e)
	}
	return v, rows.Err()
}
func (s *Store) NextDelivery(ctx context.Context) (Delivery, error) {
	var d Delivery
	// A lease prevents duplicate delivery by concurrent application workers.
	err := s.pool.QueryRow(ctx, `UPDATE webhook_deliveries SET next_attempt=now()+interval '60 seconds' WHERE id=(SELECT id FROM webhook_deliveries WHERE next_attempt<=now() ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1) RETURNING id,payload,attempts`).Scan(&d.ID, &d.Payload, &d.Attempts)
	return d, err
}
func (s *Store) FinishDelivery(ctx context.Context, d Delivery, success bool) error {
	if success || d.Attempts >= 2 {
		_, err := s.pool.Exec(ctx, `DELETE FROM webhook_deliveries WHERE id=$1`, d.ID)
		return err
	}
	_, err := s.pool.Exec(ctx, `UPDATE webhook_deliveries SET attempts=attempts+1,next_attempt=now()+($2 * interval '1 minute') WHERE id=$1`, d.ID, (d.Attempts+1)*5)
	return err
}
func (s *Store) PruneOperations(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM operation_events WHERE created_at<now()-interval '14 days' OR id NOT IN (SELECT id FROM operation_events ORDER BY id DESC LIMIT 2000); DELETE FROM webhook_deliveries WHERE created_at<now()-interval '1 day'; DELETE FROM operation_states WHERE (kind='connection' AND NOT EXISTS(SELECT 1 FROM connections WHERE id::text=resource_id)) OR (kind='provider' AND NOT EXISTS(SELECT 1 FROM model_providers WHERE id::text=resource_id))`)
	return err
}
