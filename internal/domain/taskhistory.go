package domain

import "time"

// TaskHistory represents a persisted record of a task execution or skeleton.
type TaskHistory struct {
	ID                 string         `json:"id"`
	ProcessID          string         `json:"process_id"`
	ChronologicalOrder int            `json:"chronological_order"`
	TaskID             string         `json:"task_id"`
	TaskInstanceID     string         `json:"task_instance_id,omitempty"`
	TaskType           string         `json:"task_type,omitempty"`
	OwnerID            string         `json:"owner_id,omitempty"`
	Groups             []string       `json:"groups,omitempty"`
	PreparedInput      map[string]any `json:"prepared_input,omitempty"`
	TaskOutput         map[string]any `json:"task_output,omitempty"`
	State              string         `json:"state,omitempty"` // running|completed|failed
	CreatedAt          time.Time      `json:"created_at,omitempty"`
	CompletedAt        *time.Time     `json:"completed_at,omitempty"`
	SizeBytes          int            `json:"size_bytes,omitempty"`
	UpdatedAt          time.Time      `json:"updated_at,omitempty"`
}
