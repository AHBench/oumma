package documents

import (
	"context"
	"database/sql"
)

// schema creates the DMS tables in the shared oumma database. Table names
// are prefixed with dms_ to avoid collisions with engine tables.
const schema = `
CREATE TABLE IF NOT EXISTS dms_documents (
    id                  BIGSERIAL PRIMARY KEY,
    name                TEXT NOT NULL,
    type                TEXT NOT NULL,
    size                BIGINT NOT NULL,
    upload_date         TIMESTAMPTZ NOT NULL,
    status              TEXT NOT NULL,
    user_id             TEXT NOT NULL DEFAULT '',
    organization_id     TEXT,
    process_id          TEXT,
    process_instance_id TEXT,
    description         TEXT,
    file_path           TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS dms_document_versions (
    id              BIGSERIAL PRIMARY KEY,
    document_id     BIGINT NOT NULL REFERENCES dms_documents(id),
    version_number  INT NOT NULL,
    upload_date     TIMESTAMPTZ NOT NULL,
    size            BIGINT NOT NULL,
    type            TEXT NOT NULL,
    file_storage_id TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS dms_tags (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL,
    color      TEXT NOT NULL,
    process_id TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS dms_document_tags (
    document_id BIGINT NOT NULL REFERENCES dms_documents(id),
    tag_id      BIGINT NOT NULL REFERENCES dms_tags(id),
    PRIMARY KEY (document_id, tag_id)
);
`

// Migrate creates the DMS tables (idempotent).
func Migrate(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, schema)
	return err
}
