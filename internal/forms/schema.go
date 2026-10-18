package forms

import (
	"context"
	"database/sql"
)

// schema creates the oper-data-index tables in the shared oumma database.
// Table names are prefixed with oper_ to avoid collisions with engine tables.
const schema = `
CREATE TABLE IF NOT EXISTS oper_process_forms (
    id         BIGSERIAL PRIMARY KEY,
    process_id TEXT NOT NULL,
    created_at BIGINT
);

CREATE TABLE IF NOT EXISTS oper_steps (
    id                  BIGSERIAL PRIMARY KEY,
    process_form_id     BIGINT NOT NULL REFERENCES oper_process_forms(id),
    form_id             TEXT,
    step_id             TEXT,
    version             TEXT,
    process_instance_id TEXT,
    step_name           TEXT,
    step_description    TEXT,
    launchable          BOOLEAN NOT NULL DEFAULT FALSE,
    form_input          TEXT,
    created_at          BIGINT
);

CREATE TABLE IF NOT EXISTS oper_form_fields (
    id                        BIGSERIAL PRIMARY KEY,
    steps_id                  BIGINT NOT NULL REFERENCES oper_steps(id),
    field_row                 INT NOT NULL DEFAULT 0,
    field_tab                 TEXT,
    field_type                TEXT,
    field_label               TEXT,
    field_name                TEXT,
    field_value               TEXT,
    field_required            BOOLEAN NOT NULL DEFAULT FALSE,
    field_unique              BOOLEAN NOT NULL DEFAULT FALSE,
    field_min_length          INT,
    field_header_display      BOOLEAN,
    field_edit                BOOLEAN NOT NULL DEFAULT TRUE,
    field_template            TEXT,
    field_pattern             TEXT,
    field_source              TEXT,
    field_source_type         TEXT,
    field_table_name          TEXT,
    user_can_add              BOOLEAN NOT NULL DEFAULT FALSE,
    field_multple_choice      BOOLEAN NOT NULL DEFAULT FALSE,
    field_db_query            TEXT,
    field_database_user       TEXT,
    field_database_password   TEXT,
    api_key                   TEXT
);

-- Element collections stored as JSON text arrays (internal representation).
CREATE TABLE IF NOT EXISTS oper_form_field_options (
    step_field_id BIGINT NOT NULL REFERENCES oper_form_fields(id),
    ordinal       INT NOT NULL,
    value         TEXT NOT NULL,
    PRIMARY KEY (step_field_id, ordinal)
);

CREATE TABLE IF NOT EXISTS oper_form_field_table_field_names (
    step_field_id BIGINT NOT NULL REFERENCES oper_form_fields(id),
    ordinal       INT NOT NULL,
    value         TEXT NOT NULL,
    PRIMARY KEY (step_field_id, ordinal)
);

CREATE TABLE IF NOT EXISTS oper_process_form_deleted (
    id                  BIGSERIAL PRIMARY KEY,
    process_id          TEXT,
    form_id             TEXT,
    step_id             TEXT,
    version             TEXT,
    process_instance_id TEXT,
    step_name           TEXT,
    step_description    TEXT,
    launchable          BOOLEAN NOT NULL DEFAULT FALSE,
    form_input          TEXT,
    form_fields         TEXT,
    deleted             BOOLEAN NOT NULL DEFAULT FALSE,
    deleted_reason      TEXT,
    deleted_at          BIGINT NOT NULL DEFAULT 0,
    deleted_by          TEXT
);
`

// Migrate creates the oper-data-index tables (idempotent).
func Migrate(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, schema)
	return err
}
