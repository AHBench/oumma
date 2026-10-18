-- Migration 002: Task comments and attachments for collaborative user tasks.

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
