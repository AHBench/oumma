package ai

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

	"oumma/internal/domain"
)

// ctxKey is the type for context keys in this package.
type ctxKey string

const processIDKey ctxKey = "process_id"
const usageKey ctxKey = "usage"

// UsageInfo attributes an LLM call to a user/department/process for the
// running usage counters (and the monthly cost rollup).
type UsageInfo struct {
	Username   string
	Department string
	Definition string // process definition id (usage dimension "processid")
}

// ContextWithUsage returns a context carrying usage attribution. It replaces
// the process id in the log entry as well, so callers should pass it instead
// of ContextWithProcessID.
func ContextWithUsage(ctx context.Context, processID string, info UsageInfo) context.Context {
	ctx = context.WithValue(ctx, processIDKey, processID)
	ctx = context.WithValue(ctx, usageKey, info)
	return ctx
}

func usageFromContext(ctx context.Context) (UsageInfo, bool) {
	v, ok := ctx.Value(usageKey).(UsageInfo)
	return v, ok
}

// HasUsage reports whether the context carries usage attribution.
func HasUsage(ctx context.Context) bool {
	_, ok := ctx.Value(usageKey).(UsageInfo)
	return ok
}

// ContextWithProcessID returns a context carrying the process ID.
func ContextWithProcessID(ctx context.Context, pid string) context.Context {
	return context.WithValue(ctx, processIDKey, pid)
}

func processIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(processIDKey).(string); ok {
		return v
	}
	return ""
}

// LLMLogSaver is the interface the AI client uses to persist call logs.
type LLMLogSaver interface {
	SaveLLMLog(ctx context.Context, log *domain.LLMLog) error
}

// RouteResponse represents the LLM's structured routing decision.
type RouteResponse struct {
	NextTaskID      string `json:"next_task_id"`
	Reasoning       string `json:"reasoning"`
	OverrideApplied bool   `json:"override_applied"`
}

// PrepareInputRequest bundles all inputs needed for an LLM input-preparation call.
type PrepareInputRequest struct {
	Task           *domain.Task
	GlobalCtx      map[string]any
	PrevOutput     any
	TaskOutputs    []map[string]any // full execution history: ordered list of task outputs
	DefinitionID   string
	DefinitionDesc string
	Lang           string // BCP-47 language code for LLM reasoning output (e.g. "fr", "ar")
}

// LLMClient defines the interface for the AI orchestration layer.
type LLMClient interface {
	PrepareInput(ctx context.Context, req *PrepareInputRequest) (map[string]any, error)
	Route(ctx context.Context, req *RouteRequest) (*RouteResponse, error)
	// Ask performs a single free-text completion (no JSON mode) for
	// conversational questions grounded in caller-provided context.
	Ask(ctx context.Context, req *AskRequest) (string, error)
	// Chat runs a multi-message conversation (the user-facing AI assist
	// agent loop). jsonMode forces a JSON object response.
	Chat(ctx context.Context, messages []ChatMessage, jsonMode bool) (string, error)
	// Ping checks whether the configured LLM endpoint is reachable.
	// It should return nil when the server responds (even with auth failure);
	// network errors or 5xx responses should return an error.
	Ping(ctx context.Context) error
}

// AskRequest bundles the inputs for a free-text LLM question.
type AskRequest struct {
	System string // system prompt (grounding + language instruction)
	User   string // the question, optionally with client-side context
}

// RouteRequest bundles all inputs needed for an LLM routing decision.
type RouteRequest struct {
	Task           *domain.Task
	Output         any
	Topology       []domain.Edge // edges from the current task only
	AllEdges       []domain.Edge // full process topology (all edges)
	ReachableTasks []*domain.Task
	AllTasks       map[string]*domain.Task
	GlobalCtx      map[string]any
	UserOverride   string
	TaskOutputs    []map[string]any // full execution history: ordered list of task outputs
	DefinitionID   string
	DefinitionDesc string
	Lang           string // BCP-47 language code for LLM reasoning output (e.g. "fr", "ar")
	// BranchOutputs carries the per-task outputs from a parallel fork, keyed by task_id.
	// Set only for fork-join routing decisions; nil for all other routing calls.
	BranchOutputs map[string]map[string]any
}

// LLMOptions holds per-call inference tuning parameters supported by Ollama
// and many OpenAI-compatible servers. Zero values are omitted from the request.
type LLMOptions struct {
	NumCtx      int     `json:"num_ctx,omitempty"`
	NumBatch    int     `json:"num_batch,omitempty"`
	NumPredict  int     `json:"num_predict,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
}

// OpenAIConfig holds connection configuration for the OpenAI-compatible API.
type OpenAIConfig struct {
	APIKey    string
	BaseURL   string
	Model     string
	Options   LLMOptions
	KeepAlive string // e.g. "5m" — passed as keep_alive to Ollama
}

// OpenAIClient implements LLMClient using the OpenAI Chat Completions API.
type OpenAIClient struct {
	httpClient *http.Client
	apiKey     string
	baseURL    string
	model      string
	options    LLMOptions
	keepAlive  string
	logStore   LLMLogSaver
}

// modelFamily returns a normalised family identifier derived from the model
// name. It is used to select prompt templates and response post-processing.
// Recognised families: "qwen", "gemma". Falls back to "default".
func (c *OpenAIClient) modelFamily() string {
	name := strings.ToLower(c.model)
	switch {
	case strings.Contains(name, "qwen"):
		return "qwen"
	case strings.Contains(name, "gemma"):
		return "gemma"
	default:
		return "default"
	}
}

// stripThinkBlocks removes <think>…</think> reasoning blocks that Qwen3 and
// similar models emit before the actual JSON payload.
func stripThinkBlocks(s string) string {
	for {
		start := strings.Index(s, "<think>")
		if start == -1 {
			break
		}
		end := strings.Index(s, "</think>")
		if end == -1 {
			// Unclosed block — drop everything from <think> onward.
			s = strings.TrimSpace(s[:start])
			break
		}
		s = s[:start] + s[end+len("</think>"):]
	}
	return strings.TrimSpace(s)
}

func NewOpenAIClient(cfg OpenAIConfig) *OpenAIClient {
	// No static fallback URL: the endpoint comes exclusively from
	// OPENAI_BASE_URL (docker compose / environment).
	if cfg.Model == "" {
		cfg.Model = "gpt-4o"
	}
	// HTTP client timeout = LLM_CALL_TIMEOUT_SEC + 10 s so the context
	// deadline always fires before the transport gives up.
	httpTimeoutSec := 190
	if v := os.Getenv("LLM_CALL_TIMEOUT_SEC"); v != "" {
		if n, err := fmt.Sscanf(v, "%d", &httpTimeoutSec); n == 1 && err == nil {
			httpTimeoutSec += 10
		}
	}
	return &OpenAIClient{
		httpClient: &http.Client{Timeout: time.Duration(httpTimeoutSec) * time.Second},
		apiKey:     cfg.APIKey,
		baseURL:    cfg.BaseURL,
		model:      cfg.Model,
		options:    cfg.Options,
		keepAlive:  cfg.KeepAlive,
	}
}

// SetLogStore sets the log saver used to persist every LLM call.
func (c *OpenAIClient) SetLogStore(store LLMLogSaver) {
	c.logStore = store
}

// Ping attempts a lightweight request to the LLM endpoint to determine
// whether the server is reachable. It performs a GET on /v1/models and
// treats any network error or 5xx response as an error. 4xx responses (e.g.
// 401) indicate the server is reachable but may require an API key.
func (c *OpenAIClient) Ping(ctx context.Context) error {
	// Use a short timeout to fail fast during startup.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	url := c.baseURL + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create ping request: %w", err)
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("ping request failed: %w", err)
	}
	defer resp.Body.Close()

	// Consider 5xx as server errors. 2xx and 4xx mean server is reachable.
	if resp.StatusCode >= 500 {
		return fmt.Errorf("llm server error status: %d", resp.StatusCode)
	}
	return nil
}

// --- OpenAI / Ollama protocol types ---

type chatRequest struct {
	Model              string          `json:"model"`
	Messages           []chatMessage   `json:"messages"`
	Stream             bool            `json:"stream"`
	MaxTokens          int             `json:"max_tokens,omitempty"`
	Temperature        float64         `json:"temperature,omitempty"`
	ChatTemplateKwargs *map[string]any `json:"chat_template_kwargs,omitempty"`
	ResponseFormat     *respFormat     `json:"response_format,omitempty"`
	Options            *LLMOptions     `json:"options,omitempty"`
	KeepAlive          string          `json:"keep_alive,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string, or []part for multimodal (text/image_url)
}

type respFormat struct {
	Type string `json:"type"`
}

// TokenUsage is the token accounting reported by the model API for one call.
type TokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage *TokenUsage `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// call performs a single stateless chat completion request.
// Each call sends only the system prompt and the current user prompt with no
// prior conversation history, so every routing decision starts with a clean
// context derived solely from the workflow definition and current state.
func (c *OpenAIClient) call(ctx context.Context, systemPrompt, userPrompt string) (string, *TokenUsage, error) {
	return c.callWithFormat(ctx, systemPrompt, userPrompt, true)
}

// Ask answers a free-text question (no JSON response format) — used by the
// /ask endpoint for conversational, context-grounded answers.
func (c *OpenAIClient) Ask(ctx context.Context, req *AskRequest) (string, error) {
	content, _, err := c.callWithFormat(ctx, req.System, req.User, false)
	return content, err
}

// ChatMessage is a conversation message. Content is a string, or a list of
// parts for multimodal requests: {"type":"text","text":...} and
// {"type":"image_url","image_url":{"url":"data:image/png;base64,..."}}.
type ChatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// Chat runs a multi-message conversation against the model. jsonMode forces
// a JSON object response. Used by the user-facing AI assist agent loop
// (POST /agent/chat).
func (c *OpenAIClient) Chat(ctx context.Context, messages []ChatMessage, jsonMode bool) (string, error) {
	if len(messages) == 0 {
		return "", fmt.Errorf("chat: no messages")
	}
	msgs := make([]chatMessage, 0, len(messages))
	for _, m := range messages {
		msgs = append(msgs, chatMessage{Role: m.Role, Content: m.Content})
	}
	start := time.Now()
	content, usage, err := c.chatWithMessages(ctx, msgs, jsonMode)
	durationMS := time.Since(start).Milliseconds()
	// Log user-prompt calls too — they count toward the usage/cost ledger
	// (attribution comes from the context set by the handler).
	system, _ := json.Marshal(messages)
	c.saveLog(ctx, "", "agent_chat", string(system), "user prompt", content, err, durationMS, usage)
	return content, err
}

func (c *OpenAIClient) callWithFormat(ctx context.Context, systemPrompt, userPrompt string, jsonMode bool) (string, *TokenUsage, error) {
	content, _, err := c.chatWithMessages(ctx, []chatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}, jsonMode)
	return content, nil, err
}

// chatWithMessages issues one chat-completion request for the given message
// list, retrying empty completions in-call (see the empty-content handling
// below). All inference options and Qwen thinking suppression live here.
func (c *OpenAIClient) chatWithMessages(ctx context.Context, messages []chatMessage, jsonMode bool) (string, *TokenUsage, error) {
	// Qwen3: append /no_think to the last user message to suppress
	// chain-of-thought reasoning (stripThinkBlocks is the safety net).
	if c.modelFamily() == "qwen" {
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == "user" {
				if s, ok := messages[i].Content.(string); ok {
					messages[i].Content = s + "\n/no_think"
				}
				break
			}
		}
	}

	// Qwen3 thinking models: disable reasoning at the template level. Without
	// this, vLLM deployments spend the whole token budget on hidden reasoning
	// and return empty content (or hang for many minutes).
	var chatTpl *map[string]any
	if c.modelFamily() == "qwen" {
		v := map[string]any{"enable_thinking": false}
		chatTpl = &v
	}

	var rf *respFormat
	if jsonMode {
		rf = &respFormat{Type: "json_object"}
	}
	reqBody := chatRequest{
		Model:  c.model,
		Stream: false, // always disable streaming at the protocol level
		Messages:     messages,
		ResponseFormat:     rf,
		ChatTemplateKwargs: chatTpl,
	}
	// Attach inference options when any non-zero value is configured.
	if c.options != (LLMOptions{}) {
		opts := c.options
		reqBody.Options = &opts
		// OpenAI-compatible servers (vLLM) ignore the Ollama-style options
		// object — map the cap and temperature onto the standard fields so
		// generation is bounded (unbounded reasoning output can hang the call
		// for many minutes on thinking models).
		if opts.NumPredict > 0 {
			reqBody.MaxTokens = opts.NumPredict
		}
		if opts.Temperature > 0 {
			reqBody.Temperature = opts.Temperature
		}
	}
	if c.keepAlive != "" {
		reqBody.KeepAlive = c.keepAlive
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", nil, fmt.Errorf("marshal request: %w", err)
	}

	// do issues one request; retried once when the transport fails before a
	// response (e.g. EOF from a pooled connection the server closed idle).
	do := func() (string, *TokenUsage, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return "", nil, fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if c.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return "", nil, fmt.Errorf("api call: %w", err)
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return "", nil, fmt.Errorf("read response: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			return "", nil, fmt.Errorf("api error (status %d): %s", resp.StatusCode, respBody)
		}

		var chatResp chatResponse
		if err := json.Unmarshal(respBody, &chatResp); err != nil {
			return "", nil, fmt.Errorf("unmarshal response: %w", err)
		}
		if chatResp.Error != nil {
			return "", nil, fmt.Errorf("api error: %s", chatResp.Error.Message)
		}
		if len(chatResp.Choices) == 0 {
			return "", nil, fmt.Errorf("no choices returned")
		}

		return stripThinkBlocks(chatResp.Choices[0].Message.Content), chatResp.Usage, nil
	}

	content, usage, err := do()
	if err != nil && strings.Contains(err.Error(), "EOF") {
		content, usage, err = do()
	}
	if err != nil {
		return "", nil, err
	}

	// Thinking models occasionally return empty content (budget exhausted or
	// a confused prompt) — retry in-call with a short backoff instead of
	// surfacing an unparseable (empty) response upstream, where it would
	// stall the instance in the slow routing retry loop.
	if strings.TrimSpace(content) == "" {
		for attempt := 0; attempt < 2; attempt++ {
			time.Sleep(time.Duration(500*(1<<uint(attempt))) * time.Millisecond)
			c2, u2, e2 := do()
			if e2 == nil && strings.TrimSpace(c2) != "" {
				content, usage = c2, u2
				break
			}
			if e2 != nil {
				err = e2
			}
		}
	}
	if strings.TrimSpace(content) == "" && err == nil {
		err = fmt.Errorf("model returned empty content")
	}
	if err != nil {
		return "", usage, err
	}

	return content, usage, nil
}

// PrepareInput maps global context + previous output onto the task's input
// schema. It uses a deterministic field extractor first — which covers the
// vast majority of tasks — and only falls back to an LLM call when required
// fields cannot be resolved from the available data.
func (c *OpenAIClient) PrepareInput(ctx context.Context, req *PrepareInputRequest) (map[string]any, error) {
	// Fast path: deterministic extraction. Parse the schema, pull matching
	// keys from global context and prev output, check required fields are
	// present. If everything resolves, return immediately without an LLM call.
	if result, ok := extractInput(req); ok {
		c.saveLog(ctx, req.Task.ID, "prepare_input", "deterministic", "schema-extract", "{}", nil, 0, nil)
		return result, nil
	}

	// Slow path: LLM call for tasks where context keys don't fully cover the
	// schema (e.g. computed fields, transformations, language generation).
	system := inputSystemPrompt(c.modelFamily())
	user := BuildInputPrompt(req, c.modelFamily())

	start := time.Now()
	raw, usage, err := c.call(ctx, system, user)
	durationMS := time.Since(start).Milliseconds()

	c.saveLog(ctx, req.Task.ID, "prepare_input", system, user, raw, err, durationMS, usage)

	if err != nil {
		return nil, fmt.Errorf("prepare input for task %s: %w", req.Task.ID, err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, fmt.Errorf("parse LLM input response: %w", err)
	}
	return result, nil
}

// Route asks the LLM to decide which task should execute next using the topology map.
func (c *OpenAIClient) Route(ctx context.Context, req *RouteRequest) (*RouteResponse, error) {
	system := routeSystemPrompt(c.modelFamily())
	user := BuildRoutingPrompt(req, c.modelFamily())

	start := time.Now()
	raw, usage, err := c.call(ctx, system, user)
	durationMS := time.Since(start).Milliseconds()

	c.saveLog(ctx, req.Task.ID, "route", system, user, raw, err, durationMS, usage)

	if err != nil {
		return nil, fmt.Errorf("route after task %s: %w", req.Task.ID, err)
	}

	var result RouteResponse
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, fmt.Errorf("parse LLM route response: %w", err)
	}
	if result.NextTaskID == "" {
		return nil, fmt.Errorf("LLM returned empty next_task_id")
	}
	return &result, nil
}

// saveLog persists an LLM call log entry if a log store is configured.
func (c *OpenAIClient) saveLog(ctx context.Context, taskID, operation, systemPrompt, userPrompt, response string, callErr error, durationMS int64, usage *TokenUsage) {
	if c.logStore == nil {
		return
	}
	entry := &domain.LLMLog{
		ID:           domain.NewUUID(),
		ProcessID:    processIDFromContext(ctx),
		TaskID:       taskID,
		Operation:    operation,
		Model:        c.model,
		SystemPrompt: systemPrompt,
		UserPrompt:   userPrompt,
		Response:     response,
		DurationMS:   durationMS,
		CreatedAt:    time.Now().UTC(),
	}
	if u, ok := usageFromContext(ctx); ok {
		entry.Username = u.Username
		entry.Department = u.Department
		entry.DefinitionID = u.Definition
	}
	if usage != nil {
		entry.PromptTokens = usage.PromptTokens
		entry.CompletionTokens = usage.CompletionTokens
	}
	if callErr != nil {
		entry.Error = callErr.Error()
	}
	_ = c.logStore.SaveLLMLog(ctx, entry)
}
