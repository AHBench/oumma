package domain

import "time"

// TrackStatus represents the execution status of a step in the process history.
type TrackStatus string

const (
	TrackStatusRouted       TrackStatus = "ROUTED"        // LLM made a routing decision; FromTaskID completed
	TrackStatusWaitingHuman TrackStatus = "WAITING_HUMAN" // process paused at a HUMAN task awaiting input
)

// ProcessTrackStep records one event in the execution history of a process.
// Two kinds of events are stored:
//
//   - ROUTED: the LLM routed away from FromTaskID to ToTaskID after the task completed.
//   - WAITING_HUMAN: the process entered a HUMAN task and is now paused (ToTaskID is empty).
type ProcessTrackStep struct {
	ID         string      `json:"id"`
	ProcessID  string      `json:"process_id"`
	StepOrder  int         `json:"step_order"`           // 0-based, assigned by the store
	FromTaskID string      `json:"from_task_id"`         // task that triggered this event
	ToTaskID   string      `json:"to_task_id,omitempty"` // next task (empty for WAITING_HUMAN)
	TaskType   string      `json:"task_type"`            // domain.TaskType of FromTaskID
	Status     TrackStatus `json:"status"`
	Reasoning  string      `json:"reasoning,omitempty"` // LLM explanation (ROUTED only)
	CreatedAt  time.Time   `json:"created_at"`
}

// ExecutionPhase represents where in the task execution cycle a process is.
type ExecutionPhase string

const (
	PhaseInputReady   ExecutionPhase = "INPUT_READY"   // LLM prepared input, ready to execute
	PhaseTaskDone     ExecutionPhase = "TASK_DONE"     // Task executed, output available, ready to route
	PhaseWaitingHuman ExecutionPhase = "WAITING_HUMAN" // Paused at HUMAN task, waiting for resume
	PhaseRouted       ExecutionPhase = "ROUTED"        // LLM routed to next task, ready to advance state
)

// ProcessCheckpoint captures the execution progress within a single task iteration.
// It allows the engine to resume exactly where it left off after a crash or restart,
// avoiding duplicate LLM calls and duplicate side-effecting HTTP calls.
type ProcessCheckpoint struct {
	ProcessID     string         `json:"process_id"`
	TaskID        string         `json:"task_id"`
	Phase         ExecutionPhase `json:"phase"`
	PreparedInput map[string]any `json:"prepared_input,omitempty"`
	TaskOutput    map[string]any `json:"task_output,omitempty"`
	NextTaskID    string         `json:"next_task_id,omitempty"`
	Override      string         `json:"override,omitempty"`
	UpdatedAt     time.Time      `json:"updated_at"`
}
