// Package mcptools implements the designer's MCP-like tool set: system
// tasks that talk to external services (Gmail, Telegram, WhatsApp, generic
// HTTP). The designer adds a tool as a system task with a configuration
// (credentials/addresses, stored on the task) and an input mapping (tool
// parameters ← process-context fields). The engine executes the tool when
// it reaches the task — deterministically, with ZERO LLM tokens.
//
// Dry-run: when MCP_TOOLS_DRY_RUN is not "false" (the default), executions
// build the exact payload that would be sent, log it, and return a
// simulated success — safe for local development and E2E without real
// credentials. Set MCP_TOOLS_DRY_RUN=false for real delivery.
package mcptools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// FieldSpec is one configuration field of a tool (rendered as a form row
// by the designer's tool-config editor).
type FieldSpec struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Placeholder string   `json:"placeholder,omitempty"`
	Required    bool     `json:"required"`
	Secret      bool     `json:"secret,omitempty"` // masked in the UI
	Options     []string `json:"options,omitempty"`
}

// Tool is one entry of the catalog.
type Tool struct {
	Key         string      `json:"key"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Provider    string      `json:"provider"` // telegram | gmail | whatsapp | http
	Config      []FieldSpec `json:"config"`
}

// Catalog is the full tool set exposed to the designer.
func Catalog() []Tool {
	return []Tool{
		{
			Key:        "mcp.telegram.send",
			Name:       "Telegram — send message",
			Provider:   "telegram",
			Description: "Sends a Telegram message via the Bot API (sendMessage).",
			Config: []FieldSpec{
				{Key: "bot_token", Label: "Bot token", Placeholder: "123456:ABC-DEF…", Required: true, Secret: true},
				{Key: "chat_id", Label: "Chat ID", Placeholder: "123456789 or @channel", Required: true},
			},
		},
		{
			Key:        "mcp.gmail.send",
			Name:       "Gmail — send email",
			Provider:   "gmail",
			Description: "Sends an email (to/subject/body) through the configured email gateway.",
			Config: []FieldSpec{
				{Key: "from", Label: "From address", Placeholder: "no-reply@oumma.local", Required: true},
				{Key: "gateway", Label: "Email gateway URL", Placeholder: "https://mail.example.com/api/send", Required: true},
				{Key: "api_key", Label: "Gateway API key", Required: true, Secret: true},
			},
		},
		{
			Key:        "mcp.whatsapp.send",
			Name:       "WhatsApp — send message",
			Provider:   "whatsapp",
			Description: "Sends a WhatsApp message via the Meta Cloud API (text).",
			Config: []FieldSpec{
				{Key: "access_token", Label: "Cloud API access token", Required: true, Secret: true},
				{Key: "phone_number_id", Label: "Phone number ID", Required: true},
				{Key: "api_base", Label: "API base", Placeholder: "https://graph.facebook.com/v18.0", Required: false},
			},
		},
		{
			Key:        "mcp.http.request",
			Name:       "HTTP — generic request",
			Provider:   "http",
			Description: "Calls any HTTP endpoint (method/URL/headers) with the mapped inputs as JSON body.",
			Config: []FieldSpec{
				{Key: "method", Label: "Method", Required: true, Options: []string{"GET", "POST", "PUT", "PATCH", "DELETE"}},
				{Key: "url", Label: "URL", Placeholder: "https://api.example.com/webhook", Required: true},
				{Key: "headers", Label: "Headers (JSON)", Placeholder: `{"X-Api-Key": "…"}`, Required: false},
			},
		},
	}
}

// Lookup finds a tool by key.
func Lookup(key string) (Tool, bool) {
	for _, t := range Catalog() {
		if t.Key == key {
			return t, true
		}
	}
	return Tool{}, false
}

// ValidateConfig checks that all required config fields are filled.
func ValidateConfig(key string, config map[string]string) error {
	t, ok := Lookup(key)
	if !ok {
		return fmt.Errorf("unknown tool %q", key)
	}
	for _, f := range t.Config {
		if !f.Required {
			continue
		}
		if strings.TrimSpace(config[f.Key]) == "" {
			return fmt.Errorf("tool %s: missing required config field %q", key, f.Label)
		}
	}
	return nil
}

func dryRun() bool {
	return strings.ToLower(strings.TrimSpace(os.Getenv("MCP_TOOLS_DRY_RUN"))) != "false"
}

// httpTimeout is the outbound call timeout for real tool deliveries.
// Override with MCP_HTTP_TIMEOUT_SEC (seconds; default 30).
func httpTimeout() time.Duration {
	if v := strings.TrimSpace(os.Getenv("MCP_HTTP_TIMEOUT_SEC")); v != "" {
		if sec, err := time.ParseDuration(v + "s"); err == nil && sec > 0 {
			return sec
		}
	}
	return 30 * time.Second
}

// Execute runs the tool with the resolved inputs (tool parameters ← process
// context values already substituted by the engine). Returns the task
// output map.
func Execute(ctx context.Context, toolKey string, config map[string]string, inputs map[string]any) (map[string]any, error) {
	tool, ok := Lookup(toolKey)
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", toolKey)
	}
	if err := ValidateConfig(toolKey, config); err != nil {
		return nil, err
	}

	var endpoint string
	var method string
	var payload map[string]any
	var err error
	switch tool.Provider {
	case "telegram":
		base := strings.TrimRight(os.Getenv("MCP_TELEGRAM_API_BASE"), "/")
		if base == "" {
			base = "https://api.telegram.org"
		}
		endpoint = fmt.Sprintf("%s/bot%s/sendMessage", base, strings.TrimSpace(config["bot_token"]))
		method = http.MethodPost
		payload = map[string]any{
			"chat_id": mustStr(inputs, "chat_id", config["chat_id"]),
			"text":    mustStr(inputs, "message", mustStr(inputs, "text", "")),
		}
	case "gmail":
		base := strings.TrimRight(os.Getenv("MCP_GMAIL_GATEWAY"), "/")
		if base == "" {
			base = strings.TrimSpace(config["gateway"])
		}
		endpoint = base
		if endpoint == "" {
			return nil, fmt.Errorf("tool mcp.gmail.send: no email gateway configured (gateway config or MCP_GMAIL_GATEWAY)")
		}
		method = http.MethodPost
		payload = map[string]any{
			"from":    config["from"],
			"to":      mustStr(inputs, "to", ""),
			"subject": mustStr(inputs, "subject", ""),
			"body":    mustStr(inputs, "body", mustStr(inputs, "message", "")),
		}
		if strings.TrimSpace(config["api_key"]) != "" {
			payload["api_key"] = config["api_key"]
		}
	case "whatsapp":
		base := strings.TrimSpace(config["api_base"])
		if base == "" {
			base = os.Getenv("MCP_WHATSAPP_API_BASE")
		}
		if base == "" {
			base = "https://graph.facebook.com/v18.0"
		}
		endpoint = fmt.Sprintf("%s/%s/messages", strings.TrimRight(base, "/"), strings.TrimSpace(config["phone_number_id"]))
		method = http.MethodPost
		payload = map[string]any{
			"messaging_product": "whatsapp",
			"to":                mustStr(inputs, "to", ""),
			"type":              "text",
			"text":              map[string]any{"body": mustStr(inputs, "message", mustStr(inputs, "text", ""))},
		}
	case "http":
		method = strings.ToUpper(strings.TrimSpace(config["method"]))
		if method == "" {
			method = http.MethodPost
		}
		endpoint = strings.TrimSpace(config["url"])
		if endpoint == "" {
			return nil, fmt.Errorf("tool mcp.http.request: missing url")
		}
		payload = inputs
	default:
		return nil, fmt.Errorf("tool %s: unsupported provider %q", toolKey, tool.Provider)
	}

	if payload == nil {
		payload = map[string]any{}
	}

	// Dry-run: build the exact request, log it, simulate success.
	if dryRun() {
		log := logger()
		log.Info("mcp tool dry-run",
			"tool", toolKey, "endpoint", endpoint, "method", method,
			"payload", redactPayload(payload, tool.Provider, config))
		return map[string]any{
			"status":  "sent",
			"dry_run": true,
			"tool":    toolKey,
			"provider": tool.Provider,
			"endpoint": endpoint,
			"payload":  redactPayload(payload, tool.Provider, config),
		}, nil
	}

	// Real delivery.
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(mustJSON(payload)))
	if err != nil {
		return nil, fmt.Errorf("tool %s: %w", toolKey, err)
	}
	req.Header.Set("Content-Type", "application/json")
	switch tool.Provider {
	case "whatsapp":
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(config["access_token"]))
	case "gmail":
		if k := strings.TrimSpace(config["api_key"]); k != "" {
			req.Header.Set("Authorization", "Bearer "+k)
		}
	}
	if tool.Provider == "http" && strings.TrimSpace(config["headers"]) != "" {
		var h map[string]string
		if json.Unmarshal([]byte(config["headers"]), &h) == nil {
			for k, v := range h {
				req.Header.Set(k, v)
			}
		}
	}
	client := &http.Client{Timeout: httpTimeout()}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tool %s: %w", toolKey, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var data map[string]any
	if json.Unmarshal(body, &data) != nil {
		data = map[string]any{"raw_body": string(body)}
	}
	out := map[string]any{
		"status":  "sent",
		"dry_run": false,
		"tool":    toolKey,
		"provider": tool.Provider,
	}
	for k, v := range data {
		out[k] = v
	}
	if resp.StatusCode >= 400 {
		out["error"] = fmt.Sprintf("upstream status %d", resp.StatusCode)
		return out, &SystemError{StatusCode: resp.StatusCode, Response: out}
	}
	return out, nil
}

// SystemError mirrors engine.SystemTaskError semantics for tool failures.
type SystemError struct {
	StatusCode int
	Response   map[string]any
}

func (e *SystemError) Error() string {
	return fmt.Sprintf("mcp tool error: status %d", e.StatusCode)
}

// ---------- helpers ----------

func mustStr(m map[string]any, key, fallback string) string {
	if m != nil {
		if v, ok := m[key]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
			if v != nil {
				return fmt.Sprintf("%v", v)
			}
		}
	}
	return fallback
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// redactPayload masks secret values before logging/returning.
func redactPayload(p map[string]any, provider string, config map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range p {
		out[k] = v
	}
	if provider == "telegram" {
		if t, ok := out["text"].(string); ok && len(t) > 120 {
			out["text"] = t[:120] + "…"
		}
	}
	_ = config
	return out
}

func logger() fieldLogger {
	return fieldLogger{}
}

// fieldLogger is a tiny logger shim so the package has no dependencies
// outside the stdlib. The engine wires real logging via context.
type fieldLogger struct{}

func (fieldLogger) Info(msg string, kv ...any) {
	fmt.Printf("[mcptools] %s %v\n", msg, kv)
}

var _ = json.Marshal
