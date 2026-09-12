package store

import (
	"context"
	"time"
)

const auditColumns = `SELECT coalesce(e.agent_id::text,''),coalesce(a.name,'Unknown'),coalesce(e.connection_id::text,''),coalesce(c.name,e.model_provider,''),coalesce(e.tool_id::text,''),coalesce(t.exposed_name,e.model_alias,''),
	e.method,e.decision,coalesce(e.reason,''),e.duration_ms,e.created_at,coalesce(e.model_name,''),e.input_tokens,e.output_tokens`

const auditFrom = ` FROM audit_events e LEFT JOIN agents a ON a.id=e.agent_id LEFT JOIN connections c ON c.id=e.connection_id LEFT JOIN tools t ON t.id=e.tool_id`

// RecordAudit appends one authorization or invocation outcome. Payloads and
// credentials are never stored.
func (s *Store) RecordAudit(ctx context.Context, agentID, connectionID, toolID, method, decision, reason string, duration time.Duration) error {
	ms := int(duration.Milliseconds())
	_, err := s.pool.Exec(ctx, `INSERT INTO audit_events(agent_id,connection_id,tool_id,method,decision,reason,duration_ms) VALUES ($1,$2,$3,$4,$5,$6,$7)`, nullable(agentID), nullable(connectionID), nullable(toolID), method, decision, nullable(reason), ms)
	return err
}

func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditEvent, error) {
	events, _, err := s.SearchAudit(ctx, "", "", "", limit, 0)
	return events, err
}

func (s *Store) SearchAudit(ctx context.Context, query, decision, agentID string, limit, offset int) ([]AuditEvent, int, error) {
	filter := ` WHERE ($1='' OR coalesce(a.name,'') ILIKE '%'||$1||'%' OR coalesce(c.name,e.model_provider,'') ILIKE '%'||$1||'%' OR coalesce(t.exposed_name,e.model_alias,'') ILIKE '%'||$1||'%' OR coalesce(e.reason,'') ILIKE '%'||$1||'%')
		AND ($2='' OR e.decision=$2) AND ($3='' OR e.agent_id::text=$3)`
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*)`+auditFrom+filter, query, decision, agentID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx, auditColumns+auditFrom+filter+` ORDER BY e.created_at DESC, e.id DESC LIMIT $4 OFFSET $5`, query, decision, agentID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var result []AuditEvent
	for rows.Next() {
		var e AuditEvent
		if err := rows.Scan(&e.AgentID, &e.AgentName, &e.ConnectionID, &e.ConnectionName, &e.ToolID, &e.ToolName, &e.Method, &e.Decision, &e.Reason, &e.DurationMS, &e.CreatedAt, &e.ModelName, &e.InputTokens, &e.OutputTokens); err != nil {
			return nil, 0, err
		}
		result = append(result, e)
	}
	return result, total, rows.Err()
}

// AuditStatsSince counts outcomes recorded at or after the given time.
func (s *Store) AuditStatsSince(ctx context.Context, since time.Time) (AuditStats, error) {
	var stats AuditStats
	err := s.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE decision='allowed'), count(*) FILTER (WHERE decision='denied'), count(*) FILTER (WHERE decision='error')
		FROM audit_events WHERE created_at >= $1`, since).Scan(&stats.Allowed, &stats.Denied, &stats.Errors)
	return stats, err
}
