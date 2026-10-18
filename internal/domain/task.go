package domain

import "encoding/json"

// TaskType defines the category of a workflow task.
type TaskType string

const (
	TaskTypeStart  TaskType = "START"
	TaskTypeEnd    TaskType = "END"
	TaskTypeHuman  TaskType = "HUMAN"
	TaskTypeSystem TaskType = "SYSTEM"
)

// Task represents a single node in the workflow graph.
type Task struct {
	ID           string          `json:"id" yaml:"task_id"`
	Type         TaskType        `json:"type" yaml:"type"`
	Description  string          `json:"description" yaml:"description"`
	Group        string          `json:"group,omitempty" yaml:"group,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty" yaml:"input_schema,omitempty"`
	OutputSchema json.RawMessage `json:"output_schema,omitempty" yaml:"output_schema,omitempty"`
	Config       *TaskConfig     `json:"config,omitempty" yaml:"config,omitempty"`
	Metadata     map[string]any  `json:"metadata,omitempty" yaml:"metadata,omitempty"`
}

// TaskConfig holds execution-specific configuration.
type TaskConfig struct {
	URL     string            `json:"url,omitempty" yaml:"url,omitempty"`
	Method  string            `json:"method,omitempty" yaml:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`

	// MCP-like tool execution (system tasks): when Tool is set the engine
	// runs the catalog tool with ToolConfig (credentials/addresses) and
	// resolves ToolInputs (tool parameter <- context field key) from the
	// process context — deterministically, with no LLM call.
	Tool       string            `json:"tool,omitempty" yaml:"tool,omitempty"`
	ToolConfig map[string]string `json:"tool_config,omitempty" yaml:"tool_config,omitempty"`
	ToolInputs map[string]string `json:"tool_inputs,omitempty" yaml:"tool_inputs,omitempty"`
}
