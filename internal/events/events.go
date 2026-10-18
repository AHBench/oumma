package events

import "time"

// Topic names for the two Kafka streams.
const (
	TopicProcessStream = "flowiq-process-stream"
	TopicTaskStream    = "flowiq-task-stream"
)

// OperationType identifies the mutation that triggered the event.
type OperationType string

const (
	OpAddComment    OperationType = "ADD_COMMENT"
	OpAddAttachment OperationType = "ADD_ATTACHMENT"
	OpTransition    OperationType = "PROCESS_TRANSITION"
)

// TaskEvent is published to flowiq-task-stream.
// It captures user-driven mutations on a task instance (comments, attachments).
type TaskEvent struct {
	EventID    string        `json:"event_id"`
	OccurredAt time.Time     `json:"occurred_at"`
	Operation  OperationType `json:"operation"`
	ProcessID  string        `json:"process_id"`
	TaskID     string        `json:"task_id"`
	// Fields contains a diff / snapshot of all modified fields for the operation.
	Fields map[string]any `json:"fields"`
}

// ProcessEvent is published to flowiq-process-stream.
// It captures every LLM-driven transition between workflow steps.
type ProcessEvent struct {
	EventID           string       `json:"event_id"`
	OccurredAt        time.Time    `json:"occurred_at"`
	ProcessInstanceID string       `json:"process_instance_id"`
	StatusChange      StatusChange `json:"status_change"`
	// LLMOutput is the raw structured response the LLM produced to decide the transition.
	LLMOutput any `json:"llm_output"`
}

// StatusChange describes the from/to step in a workflow transition.
type StatusChange struct {
	FromTask  string `json:"from_task"`
	ToTask    string `json:"to_task"`
	Reasoning string `json:"reasoning,omitempty"`
}
