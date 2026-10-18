-- Oumma workflow engine — initial schema.
-- This migration runs automatically on startup when DATABASE_URL is set.
-- Tables use JSONB for flexible, schema-less storage of workflow state and definitions.

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
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_llm_logs_process
    ON llm_logs (process_id, created_at);

-- Checkpoint table: one row per active process, tracks execution phase within
-- a single task iteration so the engine can resume exactly where it left off
-- after a crash/restart, avoiding duplicate LLM calls and HTTP side-effects.
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
