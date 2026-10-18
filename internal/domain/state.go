package domain

import (
	"crypto/rand"
	"fmt"
	"time"
)

// ProcessStatus represents the lifecycle status of a running process.
type ProcessStatus string

const (
	StatusRunning   ProcessStatus = "RUNNING"
	StatusPaused    ProcessStatus = "PAUSED"
	StatusCompleted ProcessStatus = "COMPLETED"
	StatusFailed    ProcessStatus = "FAILED"
	StatusAborted   ProcessStatus = "ABORTED"
)

// ParallelBranchStatus tracks the runtime state of a single fork branch.
type ParallelBranchStatus string

const (
	BranchRunning   ParallelBranchStatus = "running"
	BranchCompleted ParallelBranchStatus = "completed"
)

// ParallelBranch holds the state of one task running as part of a parallel fork.
// Keyed by task ID inside ExecutionState.ParallelBranches.
type ParallelBranch struct {
	TaskID      string               `json:"task_id"`
	InstanceID  string               `json:"instance_id"`
	Status      ParallelBranchStatus `json:"status"`
	Output      map[string]any       `json:"output,omitempty"`
	ActorID     string               `json:"actor_id,omitempty"`
	ActorGroups any                  `json:"actor_groups,omitempty"`
}

// ExecutionState holds the full runtime state of a process instance.
type ExecutionState struct {
	ProcessID           string         `json:"process_id"`
	DefinitionID        string         `json:"definition_id"`
	// DefinitionVersion pins the design version this instance started from
	// (0 = file-registered definition, no versioned design).
	DefinitionVersion   int            `json:"definition_version,omitempty"`
	CurrentTaskID       string         `json:"current_task_id"`
	LastCompletedTaskID string         `json:"last_completed_task_id,omitempty"`
	Status              ProcessStatus  `json:"status"`
	GlobalContext       map[string]any `json:"global_context"`
	// TaskOutputs is an ordered list of per-task outputs. Each entry contains
	// at minimum `task_id` and `task_instance_id` plus any output keys produced
	// by the task.
	TaskOutputs          []map[string]any `json:"task_outputs"`
	ActiveTaskInstanceID string           `json:"active_task_instance_id,omitempty"`
	CreatedAt            time.Time        `json:"created_at"`
	UpdatedAt            time.Time        `json:"updated_at"`
	Error                string           `json:"error,omitempty"`

	// ChronologicalCounter is an internal monotonic counter used to assign a
	// numeric chronological order to task starts. Tasks started in the same
	// engine cycle (parallel starts) will receive the same counter value.
	ChronologicalCounter int `json:"chronological_counter,omitempty"`

	// RestartedOnBoot indicates the engine already attempted an automatic
	// restart of this FAILED process during a previous node startup. Used to
	// ensure we only auto-restart recoverable FAILED processes once per
	// container start to avoid restart loops.
	RestartedOnBoot bool `json:"restarted_on_boot,omitempty"`

	// SYSTEM task error-recovery fields.
	// LastHumanTaskID is the ID of the most recent HUMAN task that was active.
	// When a SYSTEM task fails, the engine routes back to this task.
	LastHumanTaskID string `json:"last_human_task_id,omitempty"`
	// FailedSystemTaskID is the SYSTEM task that failed and must be retried.
	// Set when routing back to a HUMAN task after a SYSTEM error; cleared once
	// the human re-completes the task and the engine issues the retry.
	FailedSystemTaskID string `json:"failed_system_task_id,omitempty"`
	// SystemError is the error message from the most recent SYSTEM task failure.
	SystemError string `json:"system_error,omitempty"`

	// ParallelBranches holds the runtime state of all branches currently running
	// as part of a parallel fork. Non-nil only while a fork is in progress.
	// Keyed by task ID.
	ParallelBranches map[string]*ParallelBranch `json:"parallel_branches,omitempty"`
}

// NewUUID generates a version-4 UUID using crypto/rand.
func NewUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
