// Package forms is the Go port of the brief-oper-data-index (Quarkus/Java)
// microservice. It is an integral part of the oumma binary: same process,
// same HTTP mux (under /oper-api), same Postgres database.
package forms

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"time"
)

// ClaimsFunc returns the authenticated caller's username and token groups.
// It is wired to the oumma auth middleware claims in cmd/main.go and
// replaces the Java AuthService.validateToken calls.
type ClaimsFunc func(ctx context.Context) (username string, groups []string, ok bool)

// Service is the operational data index store.
type Service struct {
	db     *sql.DB
	claims ClaimsFunc
	log    *slog.Logger
}

// New creates the oper-data-index service bound to the shared database.
func New(db *sql.DB, claims ClaimsFunc, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	if claims == nil {
		claims = func(context.Context) (string, []string, bool) { return "", nil, false }
	}
	return &Service{db: db, claims: claims, log: log}
}

// InstantMillis is epoch milliseconds, serialized as an ISO-8601 instant to
// match the Java Instant getters on ProcessForm/Steps.
type InstantMillis int64

func (m InstantMillis) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.UnixMilli(int64(m)).UTC().Format(time.RFC3339Nano))
}

// ProcessForm mirrors the Java ProcessForm entity's JSON shape.
type ProcessForm struct {
	ID        int64          `json:"id"`
	ProcessID string         `json:"process_id"`
	CreatedAt *InstantMillis `json:"created_at"`
	Steps     []*Step        `json:"steps"`
}

// Step mirrors the Java Steps entity's JSON shape.
type Step struct {
	ID                 int64          `json:"id"`
	FormID             *string        `json:"form_id"`
	StepID             *string        `json:"step_id"`
	Version            *string        `json:"version"`
	ProcessInstanceIDs []string       `json:"process_instance_id"`
	StepName           *string        `json:"step_name"`
	StepDescription    *string        `json:"step_description"`
	Launchable         bool           `json:"launchable"`
	FormFields         []*FormField   `json:"form_fields"`
	FormInput          string         `json:"form_input"`
	CreatedAt          *InstantMillis `json:"created_at"`
	// ProcessInstanceRaw is the stored JSON text (not serialized).
	ProcessInstanceRaw string `json:"-"`
	// ProcessFormID is the parent row id (not serialized).
	ProcessFormID int64 `json:"-"`
}

// HasProcessInstanceIDs mirrors Steps.hasProcessInstanceIds().
func (st *Step) HasProcessInstanceIDs() bool { return len(st.ProcessInstanceIDs) > 0 }

// FormField mirrors the Java FormField entity's JSON shape.
type FormField struct {
	ID                     int64    `json:"id"`
	FieldRow               int      `json:"field_row"`
	FieldTab               *string  `json:"field_tab"`
	FieldType              *string  `json:"field_type"`
	FieldLabel             *string  `json:"field_label"`
	FieldName              *string  `json:"field_name"`
	FieldValue             any      `json:"field_value"`
	FieldRequired          bool     `json:"field_required"`
	FieldUnique            bool     `json:"field_unique"`
	FieldMinLength         *int     `json:"field_minLength"`
	FieldHeaderDisplay     *bool    `json:"field_header_display"`
	FieldEdit              *bool    `json:"field_edit"`
	FieldTemplate          any      `json:"field_template"`
	FieldOptions           []string `json:"field_options"`
	FieldPattern           *string  `json:"field_pattern"`
	FieldSource            *string  `json:"field_source"`
	FieldSourceType        *string  `json:"field_source_type"`
	FieldTableName         *string  `json:"field_table_name"`
	FieldTableFieldNames   []string `json:"field_table_field_names"`
	UserCanAdd             bool     `json:"user_can_add"`
	FieldMultpleChoice     bool     `json:"field_multple_choice"`
	FieldDBQuery           *string  `json:"field_db_query"`
	FieldDatabaseUser      *string  `json:"field_database_user"`
	FieldDatabasePassword  *string  `json:"field_database_password"`
	APIKey                 *string  `json:"api_key"`
}

// --- Request DTOs (mirror ProcessFormRequestDto / UpdateProcessFormRequestDto) ---

// FormFieldRequest mirrors StepDto.FormFieldDto.
type FormFieldRequest struct {
	FieldRow             int      `json:"field_row"`
	FieldTab             *string  `json:"field_tab"`
	FieldType            *string  `json:"field_type"`
	FieldLabel           *string  `json:"field_label"`
	FieldName            *string  `json:"field_name"`
	FieldValue           any      `json:"field_value"`
	FieldRequired        bool     `json:"field_required"`
	FieldUnique          bool     `json:"field_unique"`
	FieldMinLength       *int     `json:"field_minLength"`
	FieldHeaderDisplay   *bool    `json:"field_header_display"`
	FieldEdit            *bool    `json:"field_edit"`
	FieldTemplate        any      `json:"field_template"`
	FieldOptions         []string `json:"field_options"`
	FieldPattern         *string  `json:"field_pattern"`
	FieldSource          *string  `json:"field_source"`
	FieldSourceType      *string  `json:"field_source_type"`
	FieldTableName       *string  `json:"field_table_name"`
	FieldTableFieldNames []string `json:"field_table_field_names"`
	UserCanAdd           bool     `json:"user_can_add"`
	FieldMultpleChoice   bool     `json:"field_multple_choice"`
	FieldDBQuery         *string  `json:"field_db_query"`
	FieldDatabaseUser    *string  `json:"field_database_user"`
	FieldDatabasePassword *string `json:"field_database_password"`
	APIKey               *string  `json:"api_key"`
}

// StepRequest mirrors StepDto.
type StepRequest struct {
	StepID            string              `json:"step_id"`
	FormID            *string             `json:"form_id"`
	StepName          *string             `json:"step_name"`
	StepDescription   *string             `json:"step_description"`
	ProcessInstanceID []string            `json:"process_instance_id"`
	Launchable        bool                `json:"launchable"`
	FormFields        []*FormFieldRequest `json:"form_fields"`
	FormInput         any                 `json:"form_input"`
	DeletedReason     *string             `json:"deleted_reason"`
}

// ProcessFormRequest mirrors ProcessFormRequestDto (POST/DELETE body).
type ProcessFormRequest struct {
	ProcessID         string         `json:"process_id"`
	ProcessInstanceID []string       `json:"process_instance_id"`
	Steps             []*StepRequest `json:"steps"`
}

// UpdateProcessFormRequest mirrors UpdateProcessFormRequestDto (PUT body).
type UpdateProcessFormRequest struct {
	ProcessID string       `json:"process_id"`
	FormID    string       `json:"form_id"`
	Step      *StepRequest `json:"step"`
	Source    *string      `json:"source"`
}

// apiErr is a status-code + body error, matching the Java Response errors.
type apiErr struct {
	status int
	msg    string
}

func textErr(status int, msg string) *apiErr { return &apiErr{status: status, msg: msg} }

func (e *apiErr) Error() string { return e.msg }

func strPtr(s string) *string { v := s; return &v }

func strv(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func parseVersion(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// nowMillis returns the current time as epoch milliseconds (Java
// System.currentTimeMillis equivalent).
func nowMillis() int64 { return time.Now().UnixMilli() }
