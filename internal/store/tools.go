package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// toolColumns selects the shared Tool fields. Callers append two more columns:
// whether the tool is granted (bool) and how many agents hold it (int).
const toolColumns = `
	SELECT t.id,t.connection_id,c.name,t.kind,t.upstream_name,t.exposed_name,coalesce(t.title,''),t.description,t.input_schema,
	       coalesce(t.output_schema,'null'::jsonb),coalesce(t.annotations,'null'::jsonb),coalesce(t.icons,'null'::jsonb),t.enabled`

func scanTool(row pgx.Row) (Tool, error) {
	var t Tool
	err := row.Scan(&t.ID, &t.ConnectionID, &t.ConnectionName, &t.Kind, &t.UpstreamName, &t.ExposedName, &t.Title, &t.Description, &t.InputSchema, &t.OutputSchema, &t.Annotations, &t.Icons, &t.Enabled, &t.Granted, &t.AgentCount)
	return t, err
}

func (s *Store) queryTools(ctx context.Context, query string, args ...any) ([]Tool, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Tool
	for rows.Next() {
		t, err := scanTool(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, t)
	}
	return result, rows.Err()
}

// ReconcileTools replaces a connection's discovered catalog. Tools that
// disappeared are disabled rather than deleted so grants and audit history
// survive, and agents assigned the whole connection receive new tools.
func (s *Store) ReconcileTools(ctx context.Context, connection Connection, tools []Tool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT id FROM connections WHERE id=$1 FOR UPDATE`, connection.ID); err != nil {
		return err
	}
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

// ToolsForAgent is the agent-facing catalog: granted, enabled tools on
// connections that are not disabled, in a stable order.
func (s *Store) ToolsForAgent(ctx context.Context, agentID string) ([]Tool, error) {
	return s.queryTools(ctx, toolColumns+`, true, 0
		FROM grants g JOIN tools t ON t.id=g.tool_id JOIN connections c ON c.id=t.connection_id
		WHERE g.agent_id=$1 AND t.enabled AND c.status <> 'disabled' ORDER BY t.exposed_name`, agentID)
}

// ToolFilter narrows a catalog search. AgentID makes Granted reflect that
// agent's grants; Granted then accepts "yes" or "no" to filter on it.
type ToolFilter struct {
	Query, Kind, ConnectionID, AgentID, Granted string
	Limit, Offset                               int
}

// SearchTools returns one page of enabled tools plus the total matching count.
func (s *Store) SearchTools(ctx context.Context, filter ToolFilter) ([]Tool, int, error) {
	const from = ` FROM tools t JOIN connections c ON c.id=t.connection_id LEFT JOIN grants g ON g.tool_id=t.id AND g.agent_id=$4::uuid
		WHERE t.enabled AND ($1='' OR t.exposed_name ILIKE '%'||$1||'%' OR c.name ILIKE '%'||$1||'%' OR t.description ILIKE '%'||$1||'%')
		AND ($2='' OR t.kind=$2) AND ($3='' OR t.connection_id::text=$3)
		AND ($5='' OR ($5='yes' AND g.agent_id IS NOT NULL) OR ($5='no' AND g.agent_id IS NULL))`
	args := []any{filter.Query, filter.Kind, filter.ConnectionID, nullable(filter.AgentID), filter.Granted}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*)`+from, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	tools, err := s.queryTools(ctx, toolColumns+`, (g.agent_id IS NOT NULL), (SELECT count(*) FROM grants x WHERE x.tool_id=t.id)`+from+
		` ORDER BY c.name,t.exposed_name LIMIT $6 OFFSET $7`, append(args, filter.Limit, filter.Offset)...)
	return tools, total, err
}

// ResolveGrantedTool authorizes one tool call: the tool must be granted to the
// agent, enabled, and on a connection that is not disabled. The connection's
// credential is decrypted only on success.
func (s *Store) ResolveGrantedTool(ctx context.Context, agentID, exposedName string) (Tool, Connection, Credential, error) {
	t, err := scanTool(s.pool.QueryRow(ctx, toolColumns+`, true, 0
		FROM grants g JOIN tools t ON t.id=g.tool_id JOIN connections c ON c.id=t.connection_id
		WHERE g.agent_id=$1 AND t.exposed_name=$2 AND t.enabled AND c.status <> 'disabled'`, agentID, exposedName))
	if errors.Is(err, pgx.ErrNoRows) {
		return Tool{}, Connection{}, Credential{}, ErrNotFound
	}
	if err != nil {
		return Tool{}, Connection{}, Credential{}, err
	}
	c, credential, err := s.GetConnection(ctx, t.ConnectionID)
	if err != nil {
		return Tool{}, Connection{}, Credential{}, err
	}
	return t, c, credential, nil
}

// GetTool returns one tool with its connection, typed specification, and the
// agents that hold it.
func (s *Store) GetTool(ctx context.Context, id string) (ToolDetail, error) {
	var detail ToolDetail
	row := s.pool.QueryRow(ctx, toolColumns+`, false, (SELECT count(*) FROM grants x WHERE x.tool_id=t.id), t.last_seen_at, t.created_at
		FROM tools t JOIN connections c ON c.id=t.connection_id WHERE t.id=$1`, id)
	t := &detail.Tool
	err := row.Scan(&t.ID, &t.ConnectionID, &t.ConnectionName, &t.Kind, &t.UpstreamName, &t.ExposedName, &t.Title, &t.Description, &t.InputSchema, &t.OutputSchema, &t.Annotations, &t.Icons, &t.Enabled, &t.Granted, &t.AgentCount, &detail.LastSeenAt, &detail.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ToolDetail{}, ErrNotFound
	}
	if err != nil {
		return ToolDetail{}, err
	}
	if detail.Connection, err = s.GetConnectionSummary(ctx, t.ConnectionID); err != nil {
		return ToolDetail{}, err
	}
	switch t.Kind {
	case "http":
		spec, err := s.GetHTTPToolSpec(ctx, id)
		if err != nil {
			return ToolDetail{}, err
		}
		detail.HTTP = &spec
	case "command":
		spec, err := s.GetCommandToolSpec(ctx, id)
		if err != nil {
			return ToolDetail{}, err
		}
		detail.Command = &spec
	}
	rows, err := s.pool.Query(ctx, `SELECT a.id,a.slug,a.name,a.status FROM grants g JOIN agents a ON a.id=g.agent_id WHERE g.tool_id=$1 ORDER BY a.name`, id)
	if err != nil {
		return ToolDetail{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.ID, &a.Slug, &a.Name, &a.Status); err != nil {
			return ToolDetail{}, err
		}
		detail.Agents = append(detail.Agents, a)
	}
	return detail, rows.Err()
}

// DeleteTool removes a declared HTTP or command tool. Discovered MCP tools are
// owned by their connection's catalog and cannot be deleted individually.
func (s *Store) DeleteTool(ctx context.Context, id string) error {
	command, err := s.pool.Exec(ctx, `DELETE FROM tools WHERE id=$1 AND kind <> 'mcp'`, id)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CreateHTTPTool(ctx context.Context, connectionID, name, title, description string, inputSchema json.RawMessage, spec HTTPToolSpec) (string, error) {
	return s.createDeclaredTool(ctx, connectionID, "http", name, title, description, inputSchema, func(tx pgx.Tx, toolID string) error {
		_, err := tx.Exec(ctx, `INSERT INTO http_tool_specs(tool_id,method,url_template,query_template,headers_template,body_template,timeout_ms,max_response_bytes)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, toolID, spec.Method, spec.URLTemplate, spec.QueryTemplate, spec.HeadersTemplate, nullableJSON(spec.BodyTemplate), spec.TimeoutMS, spec.MaxResponseBytes)
		return err
	})
}

func (s *Store) CreateCommandTool(ctx context.Context, connectionID, name, title, description string, inputSchema json.RawMessage, spec CommandToolSpec) (string, error) {
	return s.createDeclaredTool(ctx, connectionID, "command", name, title, description, inputSchema, func(tx pgx.Tx, toolID string) error {
		_, err := tx.Exec(ctx, `INSERT INTO command_tool_specs(tool_id,executable,working_directory,args_template,stdin_mode,credential_env,timeout_ms,max_output_bytes)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, toolID, spec.Executable, nullable(spec.WorkingDirectory), spec.ArgsTemplate, spec.StdinMode, nullable(spec.CredentialEnv), spec.TimeoutMS, spec.MaxOutputBytes)
		return err
	})
}

func (s *Store) createDeclaredTool(ctx context.Context, connectionID, kind, name, title, description string, inputSchema json.RawMessage, addSpec func(pgx.Tx, string) error) (string, error) {
	connection, err := s.GetConnectionSummary(ctx, connectionID)
	if err != nil {
		return "", err
	}
	if (kind == "http" && connection.Kind != "http_api") || (kind == "command" && connection.Kind != "command") {
		return "", errors.New("tool kind does not match connection kind")
	}
	hash := sha256.Sum256(inputSchema)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var toolID string
	err = tx.QueryRow(ctx, `INSERT INTO tools(connection_id,kind,upstream_name,exposed_name,title,description,input_schema,schema_hash)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`, connectionID, kind, name, exposedToolName(connection.Slug, name), nullable(title), description, inputSchema, hash[:]).Scan(&toolID)
	if err != nil {
		return "", err
	}
	if err := addSpec(tx, toolID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO grants(agent_id,tool_id) SELECT agent_id,$2 FROM agent_connections WHERE connection_id=$1 ON CONFLICT DO NOTHING`, connectionID, toolID); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return toolID, nil
}

// EnsureCommandTool creates or updates a command tool by name. It is used by
// importers that must be safe to run repeatedly.
func (s *Store) EnsureCommandTool(ctx context.Context, connectionID, name, title, description string, inputSchema json.RawMessage, spec CommandToolSpec) error {
	connection, err := s.GetConnectionSummary(ctx, connectionID)
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
