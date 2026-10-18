package domain

import "time"

// LLMLog represents a single LLM API call with full request/response capture.
type LLMLog struct {
	ID           string    `json:"id"`
	ProcessID    string    `json:"process_id"`
	TaskID       string    `json:"task_id"`
	Operation    string    `json:"operation"` // "prepare_input" or "route"
	Model        string    `json:"model"`
	SystemPrompt string    `json:"system_prompt"`
	UserPrompt   string    `json:"user_prompt"`
	Response     string    `json:"response"`
	Error            string    `json:"error,omitempty"`
	DurationMS       int64     `json:"duration_ms"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	// Usage attribution (cost rollup dimension: department/user/process).
	Username     string `json:"username,omitempty"`
	Department   string `json:"department,omitempty"`
	DefinitionID string `json:"definition_id,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}
