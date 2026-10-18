package domain

import "time"

// TaskComment represents a human comment left on a user task instance.
type TaskComment struct {
	ID             string `json:"id"`
	ProcessID      string `json:"process_id"`
	TaskID         string `json:"task_id"`
	TaskInstanceID string `json:"task_instance_id"`
	Content        string `json:"content"`
	// Group is the owning group for the comment (e.g. "Managers").
	// Empty means no group associated.
	Group     string    `json:"group,omitempty"`
	CreatedBy string    `json:"created_by"`
	UpdatedBy string    `json:"updated_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TaskAttachment represents a file or link attached to a user task instance.
// Soft-deleted attachments are retained in the database but excluded from listings.
type TaskAttachment struct {
	ID             string `json:"id"`
	ProcessID      string `json:"process_id"`
	TaskID         string `json:"task_id"`
	TaskInstanceID string `json:"task_instance_id"`
	Name           string `json:"name"`
	URI            string `json:"uri"`
	// Group is the owning group for the attachment (e.g. "CustRelations").
	// Empty means no group associated.
	Group     string     `json:"group,omitempty"`
	CreatedBy string     `json:"created_by"`
	UpdatedBy string     `json:"updated_by"`
	Deleted   bool       `json:"deleted"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}
