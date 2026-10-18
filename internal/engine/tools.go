package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ServiceTool is a read-only tool the LLM can call while answering /ask
// questions. Tools call the in-process data services (DMS doc service and
// operational data index) using the caller's auth claims from the request
// context, so authorization stays on the service side.
type ServiceTool struct {
	Name        string
	Description string
	Args        string // human-readable argument description for the prompt
	exec        func(ctx context.Context, token string, args map[string]any) (string, error)
}

// ServiceTools is the LLM tool catalog.
type ServiceTools []ServiceTool

// InProcessDocuments is implemented by the in-process DMS document service
// (internal/documents). Methods run with the caller's auth claims in ctx and
// return JSON text for the LLM.
type InProcessDocuments interface {
	LLMListDocuments(ctx context.Context, processID, processInstanceID, keyword string) (string, error)
	LLMGetDocument(ctx context.Context, id int64) (string, error)
	LLMListTags(ctx context.Context, processID string) (string, error)
}

// InProcessForms is implemented by the in-process operational data index
// (internal/forms).
type InProcessForms interface {
	LLMListForms(ctx context.Context, active bool) (string, error)
	LLMSearchForms(ctx context.Context, query string) (string, error)
}

// NewServiceTools builds the LLM tool catalog from the in-process data
// services (Go ports of the former Java sidecars). Returns nil when neither
// service is available.
func NewServiceTools(docs InProcessDocuments, forms InProcessForms) ServiceTools {
	var tools ServiceTools
	if docs != nil {
		tools = append(tools,
			ServiceTool{
				Name:        "list_documents",
				Description: "List documents stored in the document management service (needs at least one of processId / processInstanceId / keyword)",
				Args:        `{"processId": "..."? , "processInstanceId": "..."?, "keyword": "..."?}`,
				exec: func(ctx context.Context, _ string, args map[string]any) (string, error) {
					pid, _ := args["processId"].(string)
					pinst, _ := args["processInstanceId"].(string)
					kw, _ := args["keyword"].(string)
					return docs.LLMListDocuments(ctx, pid, pinst, kw)
				},
			},
			ServiceTool{
				Name:        "get_document",
				Description: "Fetch one document by id from the document management service",
				Args:        `{"documentId": 123}`,
				exec: func(ctx context.Context, _ string, args map[string]any) (string, error) {
					id, _ := args["documentId"].(float64)
					return docs.LLMGetDocument(ctx, int64(id))
				},
			},
			ServiceTool{
				Name:        "list_document_tags",
				Description: "List document tags for a process",
				Args:        `{"processId": "onboarding"}`,
				exec: func(ctx context.Context, _ string, args map[string]any) (string, error) {
					pid, _ := args["processId"].(string)
					return docs.LLMListTags(ctx, pid)
				},
			},
		)
	}
	if forms != nil {
		tools = append(tools,
			ServiceTool{
				Name:        "list_forms",
				Description: "List operational process forms (saved or launched/active)",
				Args:        `{"active": true|false}`,
				exec: func(ctx context.Context, _ string, args map[string]any) (string, error) {
					active, _ := args["active"].(bool)
					return forms.LLMListForms(ctx, active)
				},
			},
			ServiceTool{
				Name:        "search_forms",
				Description: "Search saved operational process forms by keyword",
				Args:        `{"query": "Karim"}`,
				exec: func(ctx context.Context, _ string, args map[string]any) (string, error) {
					query, _ := args["query"].(string)
					return forms.LLMSearchForms(ctx, query)
				},
			},
		)
	}
	return tools
}

// ToolPromptSection returns the prompt text describing the available tools.
func (ts ServiceTools) ToolPromptSection() string {
	if len(ts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nYou have access to data-service tools. To call one, output a fenced block:\n```tool\n{\"name\": \"<tool>\", \"args\": {...}}\n```\n")
	b.WriteString("You may call at most 3 tools total. After receiving tool results, give the final answer.\nAvailable tools:\n")
	for _, t := range ts {
		b.WriteString(fmt.Sprintf("- %s — %s. Args: %s\n", t.Name, t.Description, t.Args))
	}
	return b.String()
}

// Execute runs the named tool and returns its result (truncated).
func (ts ServiceTools) Execute(ctx context.Context, name, token string, args map[string]any) (string, error) {
	for _, t := range ts {
		if t.Name == name {
			out, err := t.exec(ctx, token, args)
			if err != nil {
				return "", err
			}
			if len(out) > 4000 {
				out = out[:4000] + "…(truncated)"
			}
			return out, nil
		}
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

// toolCall is a parsed ```tool block from the LLM.
type toolCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

// ExtractToolCall returns the first tool call found in a ```tool fenced block.
func ExtractToolCall(text string) (*toolCall, bool) {
	const open = "```tool"
	const close = "```"
	i := strings.Index(text, open)
	if i < 0 {
		return nil, false
	}
	rest := text[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		return nil, false
	}
	block := strings.TrimSpace(rest[:j])
	var tc toolCall
	if err := json.Unmarshal([]byte(block), &tc); err != nil || tc.Name == "" {
		return nil, false
	}
	return &tc, true
}
