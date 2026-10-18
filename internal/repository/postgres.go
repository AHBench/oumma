package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	_ "github.com/lib/pq"

	"oumma/internal/domain"
)

const schema = `
CREATE TABLE IF NOT EXISTS execution_states (
    process_id TEXT PRIMARY KEY,
    data       JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_states_definition_status
    ON execution_states ((data->>'definition_id'), (data->>'status'));

CREATE TABLE IF NOT EXISTS process_definitions (
    id         TEXT PRIMARY KEY,
    data       JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS llm_logs (
    id            TEXT PRIMARY KEY,
    process_id    TEXT NOT NULL,
    task_id       TEXT NOT NULL,
    operation     TEXT NOT NULL,
    model         TEXT NOT NULL,
    system_prompt TEXT NOT NULL,
    user_prompt   TEXT NOT NULL,
    response      TEXT NOT NULL,
    error         TEXT NOT NULL DEFAULT '',
    duration_ms   BIGINT NOT NULL,
    prompt_tokens     BIGINT NOT NULL DEFAULT 0,
    completion_tokens BIGINT NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_llm_logs_process
    ON llm_logs (process_id, created_at);

ALTER TABLE llm_logs ADD COLUMN IF NOT EXISTS prompt_tokens BIGINT NOT NULL DEFAULT 0;
ALTER TABLE llm_logs ADD COLUMN IF NOT EXISTS completion_tokens BIGINT NOT NULL DEFAULT 0;
ALTER TABLE llm_logs ADD COLUMN IF NOT EXISTS username TEXT NOT NULL DEFAULT '';
ALTER TABLE llm_logs ADD COLUMN IF NOT EXISTS department TEXT NOT NULL DEFAULT '';
ALTER TABLE llm_logs ADD COLUMN IF NOT EXISTS definition_id TEXT NOT NULL DEFAULT '';

-- Usage counters: running month (reset at rollover) and the monthly archive
-- (department/user/process cost ledger).
CREATE TABLE IF NOT EXISTS usage_running (
    username          TEXT NOT NULL DEFAULT '',
    department        TEXT NOT NULL DEFAULT '',
    process_id        TEXT NOT NULL DEFAULT '',
    calls             BIGINT NOT NULL DEFAULT 0,
    prompt_tokens     BIGINT NOT NULL DEFAULT 0,
    completion_tokens BIGINT NOT NULL DEFAULT 0,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (username, department, process_id)
);

CREATE TABLE IF NOT EXISTS usage_monthly (
    month             TEXT NOT NULL,
    username          TEXT NOT NULL DEFAULT '',
    department        TEXT NOT NULL DEFAULT '',
    process_id        TEXT NOT NULL DEFAULT '',
    calls             BIGINT NOT NULL DEFAULT 0,
    prompt_tokens     BIGINT NOT NULL DEFAULT 0,
    completion_tokens BIGINT NOT NULL DEFAULT 0,
    cost_usd          NUMERIC(14,6) NOT NULL DEFAULT 0,
    PRIMARY KEY (month, username, department, process_id)
);

CREATE TABLE IF NOT EXISTS usage_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS process_checkpoints (
    process_id     TEXT PRIMARY KEY,
    task_id        TEXT NOT NULL,
    phase          TEXT NOT NULL,
    prepared_input JSONB,
    task_output    JSONB,
    next_task_id   TEXT NOT NULL DEFAULT '',
    override       TEXT NOT NULL DEFAULT '',
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS task_comments (
    id               TEXT PRIMARY KEY,
    process_id       TEXT NOT NULL,
    task_id          TEXT NOT NULL,
    task_instance_id TEXT NOT NULL,
    content          TEXT NOT NULL,
	group_name       TEXT NOT NULL DEFAULT '',
    created_by       TEXT NOT NULL DEFAULT '',
    updated_by       TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_task_comments_instance
    ON task_comments (process_id, task_instance_id, created_at);

CREATE TABLE IF NOT EXISTS task_attachments (
    id               TEXT PRIMARY KEY,
    process_id       TEXT NOT NULL,
    task_id          TEXT NOT NULL,
    task_instance_id TEXT NOT NULL,
    name             TEXT NOT NULL,
    uri              TEXT NOT NULL,
	group_name       TEXT NOT NULL DEFAULT '',
    created_by       TEXT NOT NULL DEFAULT '',
    updated_by       TEXT NOT NULL DEFAULT '',
    deleted          BOOLEAN NOT NULL DEFAULT FALSE,
    deleted_at       TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_task_attachments_instance
    ON task_attachments (process_id, task_instance_id, deleted);

-- Idempotent column additions for existing comment/attachment deployments.
ALTER TABLE task_comments ADD COLUMN IF NOT EXISTS created_by TEXT NOT NULL DEFAULT '';
ALTER TABLE task_comments ADD COLUMN IF NOT EXISTS updated_by TEXT NOT NULL DEFAULT '';
ALTER TABLE task_comments ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW();
ALTER TABLE task_comments ADD COLUMN IF NOT EXISTS group_name TEXT NOT NULL DEFAULT '';
ALTER TABLE task_attachments ADD COLUMN IF NOT EXISTS created_by TEXT NOT NULL DEFAULT '';
ALTER TABLE task_attachments ADD COLUMN IF NOT EXISTS updated_by TEXT NOT NULL DEFAULT '';
ALTER TABLE task_attachments ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW();
ALTER TABLE task_attachments ADD COLUMN IF NOT EXISTS group_name TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS process_track_steps (
    id           TEXT PRIMARY KEY,
    process_id   TEXT NOT NULL,
    step_order   INT  NOT NULL,
    from_task_id TEXT NOT NULL,
    to_task_id   TEXT NOT NULL DEFAULT '',
    task_type    TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'ROUTED',
    reasoning    TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_track_steps_process
    ON process_track_steps (process_id, step_order ASC);

-- Idempotent column additions for existing deployments.
ALTER TABLE process_track_steps ADD COLUMN IF NOT EXISTS task_type TEXT NOT NULL DEFAULT '';
ALTER TABLE process_track_steps ADD COLUMN IF NOT EXISTS status    TEXT NOT NULL DEFAULT 'ROUTED';

CREATE TABLE IF NOT EXISTS task_histories (
	id                 TEXT PRIMARY KEY,
	process_id         TEXT NOT NULL,
	chronological_order INT NOT NULL,
	task_id            TEXT NOT NULL,
	task_instance_id   TEXT NOT NULL DEFAULT '',
	task_type          TEXT NOT NULL DEFAULT '',
	owner_id           TEXT NOT NULL DEFAULT '',
	groups             JSONB,
	prepared_input     JSONB,
	task_output        JSONB,
	state              TEXT NOT NULL DEFAULT 'running',
	created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	completed_at       TIMESTAMPTZ,
	size_bytes         INT,
	updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_task_histories_process_order
	ON task_histories (process_id, chronological_order ASC);

CREATE TABLE IF NOT EXISTS email_activations (
    token       TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL,
    username    TEXT NOT NULL,
    email       TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at  TIMESTAMPTZ NOT NULL,
    used_at     TIMESTAMPTZ
);
`

// PostgresStore implements Repository and LLMLogStore using PostgreSQL with JSONB.
type PostgresStore struct {
	db *sql.DB
}

// NewPostgresStore opens a connection and runs the schema migration.
func NewPostgresStore(databaseURL string) (*PostgresStore, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("run schema migration: %w", err)
	}
	if err := (&PostgresStore{db: db}).initDesignSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("run design schema migration: %w", err)
	}

	return &PostgresStore{db: db}, nil
}

// Close closes the underlying database connection.
func (s *PostgresStore) Close() error {
	return s.db.Close()
}

// DB exposes the underlying handle so the in-process sub-services
// (documents, forms) can share the same PostgreSQL connection.
func (s *PostgresStore) DB() *sql.DB { return s.db }

// --- Repository implementation ---

func (s *PostgresStore) SaveState(ctx context.Context, state *domain.ExecutionState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO execution_states (process_id, data, created_at, updated_at)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (process_id) DO UPDATE SET data = $2, updated_at = $4`,
		state.ProcessID, data, state.CreatedAt, state.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetState(ctx context.Context, processID string) (*domain.ExecutionState, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT data FROM execution_states WHERE process_id = $1`, processID,
	).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("process %s not found", processID)
	}
	if err != nil {
		return nil, fmt.Errorf("get state: %w", err)
	}
	var state domain.ExecutionState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("unmarshal state: %w", err)
	}
	return &state, nil
}

func (s *PostgresStore) ListStates(ctx context.Context) ([]*domain.ExecutionState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM execution_states`)
	if err != nil {
		return nil, fmt.Errorf("list states: %w", err)
	}
	defer rows.Close()

	var result []*domain.ExecutionState
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("scan state: %w", err)
		}
		var st domain.ExecutionState
		if err := json.Unmarshal(data, &st); err != nil {
			continue
		}
		result = append(result, &st)
	}
	return result, rows.Err()
}

func (s *PostgresStore) RegisterDefinition(ctx context.Context, def *domain.ProcessDefinition) error {
	data, err := json.Marshal(def)
	if err != nil {
		return fmt.Errorf("marshal definition: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO process_definitions (id, data)
		 VALUES ($1, $2)
		 ON CONFLICT (id) DO UPDATE SET data = $2`,
		def.ID, data,
	)
	if err != nil {
		return fmt.Errorf("register definition: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetDefinition(ctx context.Context, defID string) (*domain.ProcessDefinition, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT data FROM process_definitions WHERE id = $1`, defID,
	).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("definition %s not found", defID)
	}
	if err != nil {
		return nil, fmt.Errorf("get definition: %w", err)
	}
	var def domain.ProcessDefinition
	if err := json.Unmarshal(data, &def); err != nil {
		return nil, fmt.Errorf("unmarshal definition: %w", err)
	}
	def.RebuildTaskList()
	return &def, nil
}

func (s *PostgresStore) ListDefinitions(ctx context.Context) ([]*domain.ProcessDefinition, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM process_definitions ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list definitions: %w", err)
	}
	defer rows.Close()

	var result []*domain.ProcessDefinition
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("scan definition: %w", err)
		}
		var def domain.ProcessDefinition
		if err := json.Unmarshal(data, &def); err != nil {
			continue
		}
		def.RebuildTaskList()
		result = append(result, &def)
	}
	return result, rows.Err()
}

// --- LLMLogStore implementation ---

func (s *PostgresStore) SaveLLMLog(ctx context.Context, log *domain.LLMLog) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO llm_logs (id, process_id, task_id, operation, model, system_prompt, user_prompt, response, error, duration_ms, prompt_tokens, completion_tokens, username, department, definition_id, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		log.ID, log.ProcessID, log.TaskID, log.Operation, log.Model,
		log.SystemPrompt, log.UserPrompt, log.Response, log.Error,
		log.DurationMS, log.PromptTokens, log.CompletionTokens,
		log.Username, log.Department, log.DefinitionID, log.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("save llm log: %w", err)
	}
	// Every attributed call increments the running usage counters
	// (rolled up into usage_monthly at month end).
	if log.DefinitionID != "" {
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO usage_running (username, department, process_id, calls, prompt_tokens, completion_tokens, updated_at)
			 VALUES ($1, $2, $3, 1, $4, $5, now())
			 ON CONFLICT (username, department, process_id) DO UPDATE
			 SET calls = usage_running.calls + EXCLUDED.calls,
			     prompt_tokens = usage_running.prompt_tokens + EXCLUDED.prompt_tokens,
			     completion_tokens = usage_running.completion_tokens + EXCLUDED.completion_tokens,
			     updated_at = now()`,
			log.Username, log.Department, log.DefinitionID, log.PromptTokens, log.CompletionTokens,
		)
		if err != nil {
			return fmt.Errorf("save usage counters: %w", err)
		}
	}
	return nil
}

func (s *PostgresStore) ListLLMLogs(ctx context.Context, processID string) ([]*domain.LLMLog, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, process_id, task_id, operation, model, system_prompt, user_prompt, response, error, duration_ms, prompt_tokens, completion_tokens, username, department, definition_id, created_at
		 FROM llm_logs WHERE process_id = $1 ORDER BY created_at ASC`, processID,
	)
	if err != nil {
		return nil, fmt.Errorf("list llm logs: %w", err)
	}
	defer rows.Close()

	var result []*domain.LLMLog
	for rows.Next() {
		var l domain.LLMLog
		if err := rows.Scan(&l.ID, &l.ProcessID, &l.TaskID, &l.Operation, &l.Model,
			&l.SystemPrompt, &l.UserPrompt, &l.Response, &l.Error, &l.DurationMS, &l.PromptTokens, &l.CompletionTokens, &l.Username, &l.Department, &l.DefinitionID, &l.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan llm log: %w", err)
		}
		result = append(result, &l)
	}
	return result, rows.Err()
}

func (s *PostgresStore) SumTokenUsage(ctx context.Context, processID string) (int64, int64, int64, error) {
	var p, c, n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COUNT(*)
			 FROM llm_logs WHERE process_id = $1`, processID,
	).Scan(&p, &c, &n)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("sum token usage: %w", err)
	}
	return p, c, n, nil
}

func (s *PostgresStore) SumTokenUsageByDefinition(ctx context.Context, definitionID string) (int64, int64, int64, error) {
	var p, c, n int64
	// A log belongs to the definition when EITHER its process_id is one of
	// the definition's instance ids (engine calls, instance-attributed chat)
	// OR it was recorded against the definition itself (agent chat / ask
	// with no active dossier — process_id = definition id). The OR (not a
	// UNION) keeps every log counted exactly once.
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(l.prompt_tokens),0), COALESCE(SUM(l.completion_tokens),0), COUNT(l.id)
			 FROM llm_logs l
			 WHERE l.process_id = $1
			    OR EXISTS (SELECT 1 FROM execution_states es
			               WHERE es.process_id = l.process_id
			                 AND es.data->>'definition_id' = $1)`, definitionID,
	).Scan(&p, &c, &n)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("sum token usage by definition: %w", err)
	}
	return p, c, n, nil
}

// SumTokenUsageByUsernameMonth totals a user's LLM tokens recorded since
// monthStart (the normal-user monthly quota).
func (s *PostgresStore) SumTokenUsageByUsernameMonth(ctx context.Context, username string, monthStart time.Time) (int64, int64, int64, error) {
	var p, c, n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COUNT(id)
			 FROM llm_logs
			 WHERE username = $1 AND created_at >= $2`, username, monthStart,
	).Scan(&p, &c, &n)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("sum token usage by user month: %w", err)
	}
	return p, c, n, nil
}

// --- TaskHistoryStore implementation ---

func (s *PostgresStore) SaveTaskHistory(ctx context.Context, h *domain.TaskHistory) error {
	if h.ID == "" {
		h.ID = domain.NewUUID()
	}
	groups, err := json.Marshal(h.Groups)
	if err != nil {
		return fmt.Errorf("marshal groups: %w", err)
	}
	prepared, err := json.Marshal(h.PreparedInput)
	if err != nil {
		return fmt.Errorf("marshal prepared_input: %w", err)
	}
	output, err := json.Marshal(h.TaskOutput)
	if err != nil {
		return fmt.Errorf("marshal task_output: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO task_histories (id, process_id, chronological_order, task_id, task_instance_id, task_type, owner_id, groups, prepared_input, task_output, state, created_at, completed_at, size_bytes, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		h.ID, h.ProcessID, h.ChronologicalOrder, h.TaskID, h.TaskInstanceID, h.TaskType, h.OwnerID, groups, prepared, output, h.State, h.CreatedAt, h.CompletedAt, h.SizeBytes, h.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("save task history: %w", err)
	}
	return nil
}

func (s *PostgresStore) UpdateTaskHistory(ctx context.Context, id string, h *domain.TaskHistory) error {
	prepared, err := json.Marshal(h.PreparedInput)
	if err != nil {
		return fmt.Errorf("marshal prepared_input: %w", err)
	}
	output, err := json.Marshal(h.TaskOutput)
	if err != nil {
		return fmt.Errorf("marshal task_output: %w", err)
	}
	groups, err := json.Marshal(h.Groups)
	if err != nil {
		return fmt.Errorf("marshal groups: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE task_histories SET task_instance_id = $2, task_output = $3, state = $4, completed_at = $5, updated_at = $6, prepared_input = $7, groups = $8, owner_id = $9, size_bytes = $10
		 WHERE id = $1`,
		id, h.TaskInstanceID, output, h.State, h.CompletedAt, h.UpdatedAt, prepared, groups, h.OwnerID, h.SizeBytes,
	)
	if err != nil {
		return fmt.Errorf("update task history: %w", err)
	}
	return nil
}

func (s *PostgresStore) ListTaskHistories(ctx context.Context, processID string) ([]*domain.TaskHistory, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, process_id, chronological_order, task_id, task_instance_id, task_type, owner_id, groups, prepared_input, task_output, state, created_at, completed_at, size_bytes, updated_at
		 FROM task_histories WHERE process_id = $1 ORDER BY chronological_order ASC`, processID,
	)
	if err != nil {
		return nil, fmt.Errorf("list task histories: %w", err)
	}
	defer rows.Close()

	var result []*domain.TaskHistory
	for rows.Next() {
		var h domain.TaskHistory
		var groups []byte
		var prepared []byte
		var output []byte
		var completed sql.NullTime
		if err := rows.Scan(&h.ID, &h.ProcessID, &h.ChronologicalOrder, &h.TaskID, &h.TaskInstanceID, &h.TaskType, &h.OwnerID, &groups, &prepared, &output, &h.State, &h.CreatedAt, &completed, &h.SizeBytes, &h.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan task history: %w", err)
		}
		if len(groups) > 0 && string(groups) != "null" {
			_ = json.Unmarshal(groups, &h.Groups)
		}
		if len(prepared) > 0 && string(prepared) != "null" {
			_ = json.Unmarshal(prepared, &h.PreparedInput)
		}
		if len(output) > 0 && string(output) != "null" {
			_ = json.Unmarshal(output, &h.TaskOutput)
		}
		if completed.Valid {
			t := completed.Time
			h.CompletedAt = &t
		}
		result = append(result, &h)
	}
	return result, rows.Err()
}

// --- CheckpointStore implementation ---

func (s *PostgresStore) SaveCheckpoint(ctx context.Context, cp *domain.ProcessCheckpoint) error {
	preparedInput, err := json.Marshal(cp.PreparedInput)
	if err != nil {
		return fmt.Errorf("marshal prepared_input: %w", err)
	}
	taskOutput, err := json.Marshal(cp.TaskOutput)
	if err != nil {
		return fmt.Errorf("marshal task_output: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO process_checkpoints (process_id, task_id, phase, prepared_input, task_output, next_task_id, override, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 ON CONFLICT (process_id) DO UPDATE SET
		   task_id = $2, phase = $3, prepared_input = $4, task_output = $5,
		   next_task_id = $6, override = $7, updated_at = $8`,
		cp.ProcessID, cp.TaskID, string(cp.Phase),
		preparedInput, taskOutput,
		cp.NextTaskID, cp.Override, cp.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("save checkpoint: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetCheckpoint(ctx context.Context, processID string) (*domain.ProcessCheckpoint, error) {
	var (
		cp            domain.ProcessCheckpoint
		phase         string
		preparedInput []byte
		taskOutput    []byte
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT process_id, task_id, phase, prepared_input, task_output, next_task_id, override, updated_at
		 FROM process_checkpoints WHERE process_id = $1`, processID,
	).Scan(&cp.ProcessID, &cp.TaskID, &phase, &preparedInput, &taskOutput,
		&cp.NextTaskID, &cp.Override, &cp.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get checkpoint: %w", err)
	}
	cp.Phase = domain.ExecutionPhase(phase)
	if len(preparedInput) > 0 && string(preparedInput) != "null" {
		if err := json.Unmarshal(preparedInput, &cp.PreparedInput); err != nil {
			return nil, fmt.Errorf("unmarshal prepared_input: %w", err)
		}
	}
	if len(taskOutput) > 0 && string(taskOutput) != "null" {
		if err := json.Unmarshal(taskOutput, &cp.TaskOutput); err != nil {
			return nil, fmt.Errorf("unmarshal task_output: %w", err)
		}
	}
	return &cp, nil
}

func (s *PostgresStore) DeleteCheckpoint(ctx context.Context, processID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM process_checkpoints WHERE process_id = $1`, processID,
	)
	if err != nil {
		return fmt.Errorf("delete checkpoint: %w", err)
	}
	return nil
}

// --- TaskCommentStore implementation ---

func (s *PostgresStore) SaveComment(ctx context.Context, c *domain.TaskComment) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO task_comments (id, process_id, task_id, task_instance_id, content, group_name, created_by, updated_by, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		c.ID, c.ProcessID, c.TaskID, c.TaskInstanceID, c.Content, c.Group,
		c.CreatedBy, c.UpdatedBy, c.CreatedAt, c.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("save comment: %w", err)
	}
	return nil
}

func (s *PostgresStore) ListComments(ctx context.Context, processID, taskInstanceID string) ([]*domain.TaskComment, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, process_id, task_id, task_instance_id, content, group_name, created_by, updated_by, created_at, updated_at
		 FROM task_comments
		 WHERE process_id = $1 AND task_instance_id = $2
		 ORDER BY created_at ASC`,
		processID, taskInstanceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list comments: %w", err)
	}
	defer rows.Close()

	var result []*domain.TaskComment
	for rows.Next() {
		var c domain.TaskComment
		if err := rows.Scan(&c.ID, &c.ProcessID, &c.TaskID, &c.TaskInstanceID, &c.Content, &c.Group,
			&c.CreatedBy, &c.UpdatedBy, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan comment: %w", err)
		}
		result = append(result, &c)
	}
	return result, rows.Err()
}

func (s *PostgresStore) ListCommentsByProcess(ctx context.Context, processID string) ([]*domain.TaskComment, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, process_id, task_id, task_instance_id, content, group_name, created_by, updated_by, created_at, updated_at
		 FROM task_comments
		 WHERE process_id = $1
		 ORDER BY created_at ASC`,
		processID,
	)
	if err != nil {
		return nil, fmt.Errorf("list comments by process: %w", err)
	}
	defer rows.Close()

	var result []*domain.TaskComment
	for rows.Next() {
		var c domain.TaskComment
		if err := rows.Scan(&c.ID, &c.ProcessID, &c.TaskID, &c.TaskInstanceID, &c.Content, &c.Group,
			&c.CreatedBy, &c.UpdatedBy, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan comment: %w", err)
		}
		result = append(result, &c)
	}
	return result, rows.Err()
}

func (s *PostgresStore) UpdateComment(ctx context.Context, commentID, content, updatedBy string, updatedAt time.Time) (*domain.TaskComment, error) {
	var c domain.TaskComment
	err := s.db.QueryRowContext(ctx,
		`UPDATE task_comments
		 SET content = $2, updated_by = $3, updated_at = $4
		 WHERE id = $1
		 RETURNING id, process_id, task_id, task_instance_id, content, group_name, created_by, updated_by, created_at, updated_at`,
		commentID, content, updatedBy, updatedAt,
	).Scan(&c.ID, &c.ProcessID, &c.TaskID, &c.TaskInstanceID, &c.Content,
		&c.Group, &c.CreatedBy, &c.UpdatedBy, &c.CreatedAt, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("comment %s not found", commentID)
	}
	if err != nil {
		return nil, fmt.Errorf("update comment: %w", err)
	}
	return &c, nil
}

// --- TaskAttachmentStore implementation ---

func (s *PostgresStore) SaveAttachment(ctx context.Context, a *domain.TaskAttachment) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO task_attachments (id, process_id, task_id, task_instance_id, name, uri, group_name, created_by, updated_by, deleted, deleted_at, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		a.ID, a.ProcessID, a.TaskID, a.TaskInstanceID, a.Name, a.URI, a.Group,
		a.CreatedBy, a.UpdatedBy, a.Deleted, a.DeletedAt, a.CreatedAt, a.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("save attachment: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetAttachment(ctx context.Context, attachmentID string) (*domain.TaskAttachment, error) {
	var a domain.TaskAttachment
	err := s.db.QueryRowContext(ctx,
		`SELECT id, process_id, task_id, task_instance_id, name, uri, group_name, created_by, updated_by, deleted, deleted_at, created_at, updated_at
		 FROM task_attachments WHERE id = $1`, attachmentID,
	).Scan(&a.ID, &a.ProcessID, &a.TaskID, &a.TaskInstanceID, &a.Name, &a.URI, &a.Group,
		&a.CreatedBy, &a.UpdatedBy, &a.Deleted, &a.DeletedAt, &a.CreatedAt, &a.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("attachment %s not found", attachmentID)
	}
	if err != nil {
		return nil, fmt.Errorf("get attachment: %w", err)
	}
	return &a, nil
}

func (s *PostgresStore) ListAttachments(ctx context.Context, processID, taskInstanceID string) ([]*domain.TaskAttachment, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, process_id, task_id, task_instance_id, name, uri, group_name, created_by, updated_by, deleted, deleted_at, created_at, updated_at
		 FROM task_attachments
		 WHERE process_id = $1 AND task_instance_id = $2 AND deleted = FALSE
		 ORDER BY created_at ASC`,
		processID, taskInstanceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list attachments: %w", err)
	}
	defer rows.Close()

	var result []*domain.TaskAttachment
	for rows.Next() {
		var a domain.TaskAttachment
		if err := rows.Scan(&a.ID, &a.ProcessID, &a.TaskID, &a.TaskInstanceID, &a.Name, &a.URI, &a.Group,
			&a.CreatedBy, &a.UpdatedBy, &a.Deleted, &a.DeletedAt, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan attachment: %w", err)
		}
		result = append(result, &a)
	}
	return result, rows.Err()
}

func (s *PostgresStore) ListAttachmentsByProcess(ctx context.Context, processID string) ([]*domain.TaskAttachment, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, process_id, task_id, task_instance_id, name, uri, group_name, created_by, updated_by, deleted, deleted_at, created_at, updated_at
		 FROM task_attachments
		 WHERE process_id = $1 AND deleted = FALSE
		 ORDER BY created_at ASC`,
		processID,
	)
	if err != nil {
		return nil, fmt.Errorf("list attachments by process: %w", err)
	}
	defer rows.Close()

	var result []*domain.TaskAttachment
	for rows.Next() {
		var a domain.TaskAttachment
		if err := rows.Scan(&a.ID, &a.ProcessID, &a.TaskID, &a.TaskInstanceID, &a.Name, &a.URI, &a.Group,
			&a.CreatedBy, &a.UpdatedBy, &a.Deleted, &a.DeletedAt, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan attachment: %w", err)
		}
		result = append(result, &a)
	}
	return result, rows.Err()
}

func (s *PostgresStore) SoftDeleteAttachment(ctx context.Context, attachmentID string) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE task_attachments
		 SET deleted = TRUE, deleted_at = NOW()
		 WHERE id = $1 AND deleted = FALSE`,
		attachmentID,
	)
	if err != nil {
		return fmt.Errorf("soft delete attachment: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("attachment %s not found or already deleted", attachmentID)
	}
	return nil
}

// --- TrackStore implementation ---

func (s *PostgresStore) AppendTrackStep(ctx context.Context, step *domain.ProcessTrackStep) error {
	// step_order is assigned as MAX(existing)+1 atomically; works even with 0 existing rows
	// because MAX() over an empty set returns NULL and COALESCE gives -1, so first order = 0.
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO process_track_steps (id, process_id, step_order, from_task_id, to_task_id, task_type, status, reasoning, created_at)
		 SELECT $1, $2, COALESCE(MAX(step_order), -1) + 1, $3, $4, $5, $6, $7, $8
		 FROM process_track_steps WHERE process_id = $2`,
		step.ID, step.ProcessID, step.FromTaskID, step.ToTaskID,
		step.TaskType, string(step.Status), step.Reasoning, step.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("append track step: %w", err)
	}
	return nil
}

func (s *PostgresStore) ListTrackSteps(ctx context.Context, processID string) ([]*domain.ProcessTrackStep, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, process_id, step_order, from_task_id, to_task_id, task_type, status, reasoning, created_at
		 FROM process_track_steps
		 WHERE process_id = $1
		 ORDER BY step_order ASC`,
		processID,
	)
	if err != nil {
		return nil, fmt.Errorf("list track steps: %w", err)
	}
	defer rows.Close()

	var result []*domain.ProcessTrackStep
	for rows.Next() {
		var ts domain.ProcessTrackStep
		var status string
		if err := rows.Scan(&ts.ID, &ts.ProcessID, &ts.StepOrder, &ts.FromTaskID, &ts.ToTaskID,
			&ts.TaskType, &status, &ts.Reasoning, &ts.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan track step: %w", err)
		}
		ts.Status = domain.TrackStatus(status)
		result = append(result, &ts)
	}
	return result, rows.Err()
}

func (s *PostgresStore) UpdateWaitingHumanStep(ctx context.Context, processID, fromTaskID, toTaskID, reasoning string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE process_track_steps
		 SET to_task_id = $3, status = 'ROUTED', reasoning = $4
		 WHERE id = (
		   SELECT id FROM process_track_steps
		   WHERE process_id = $1 AND from_task_id = $2 AND status = 'WAITING_HUMAN'
		   ORDER BY step_order DESC
		   LIMIT 1
		 )`,
		processID, fromTaskID, toTaskID, reasoning,
	)
	if err != nil {
		return fmt.Errorf("update waiting human step: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Monthly usage rollup
//
// usage_running accumulates the current month's LLM calls (department/user/
// process). At month end the totals are archived into usage_monthly with the
// USD cost (cost per LLM call) and the running counters are reset.
// ---------------------------------------------------------------------------

// CostPerCallUSD is the flat USD cost of one LLM call.
const CostPerCallUSD = 0.00049

func (s *PostgresStore) StartUsageRollover(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		// Catch up on boot (covers a month boundary the service missed).
		s.rollUsageMonth(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.rollUsageMonth(ctx)
			}
		}
	}()
}

// rollUsageMonth archives the running counters into usage_monthly when the
// calendar month has changed (or on first run, for the previous month).
func (s *PostgresStore) rollUsageMonth(ctx context.Context) {
	now := time.Now().UTC()
	cur := now.Format("2006-01")

	var last string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM usage_meta WHERE key = 'last_rolled_month'`).Scan(&last)
	if err == nil && last == cur {
		return // already rolled for this month
	}
	if err != nil && err != sql.ErrNoRows {
		return
	}

	// Target month: the month being closed (previous month on first run).
	target := last
	if target == "" {
		target = now.AddDate(0, -1, 0).Format("2006-01")
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO usage_monthly (month, username, department, process_id, calls, prompt_tokens, completion_tokens, cost_usd)
		 SELECT $1, username, department, process_id, calls, prompt_tokens, completion_tokens, calls * `+costLiteral()+`
		 FROM usage_running
		 ON CONFLICT (month, username, department, process_id) DO UPDATE
		 SET calls = usage_monthly.calls + EXCLUDED.calls,
		     prompt_tokens = usage_monthly.prompt_tokens + EXCLUDED.prompt_tokens,
		     completion_tokens = usage_monthly.completion_tokens + EXCLUDED.completion_tokens,
		     cost_usd = usage_monthly.cost_usd + EXCLUDED.cost_usd`, target)
	if err != nil {
		return
	}
	if _, err := s.db.ExecContext(ctx, `TRUNCATE usage_running`); err != nil {
		return
	}
	_, _ = s.db.ExecContext(ctx,
		`INSERT INTO usage_meta (key, value) VALUES ('last_rolled_month', $1)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, cur)
}

func costLiteral() string {
	return strconv.FormatFloat(CostPerCallUSD, 'f', -1, 64)
}

// RunningUsage returns the current month's counters per (user, department,
// process) for a definition, plus the archived months.
func (s *PostgresStore) RunningUsage(ctx context.Context, definitionID string) (map[string]int64, error) {
	var calls, p, c int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(calls),0), COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0)
		 FROM usage_running WHERE process_id = $1`, definitionID,
	).Scan(&calls, &p, &c)
	if err != nil {
		return nil, fmt.Errorf("running usage: %w", err)
	}
	return map[string]int64{"calls": calls, "prompt_tokens": p, "completion_tokens": c}, nil
}
