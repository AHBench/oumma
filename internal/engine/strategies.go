package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"oumma/internal/domain"
	"oumma/internal/mcptools"
)

// SystemTaskError is returned by SystemExecutor when the remote API responds
// with a non-2xx HTTP status code. It carries the parsed response body so the
// engine can persist it as task_output before triggering error-recovery logic.
type SystemTaskError struct {
	StatusCode int
	Response   map[string]any
}

func (e *SystemTaskError) Error() string {
	return fmt.Sprintf("system task HTTP error: status %d", e.StatusCode)
}

// TaskExecutor defines the Strategy interface for task execution.
type TaskExecutor interface {
	Execute(ctx context.Context, task *domain.Task, input map[string]any) (map[string]any, error)
}

// StartExecutor handles START lifecycle tasks.
type StartExecutor struct{}

func (e *StartExecutor) Execute(_ context.Context, task *domain.Task, input map[string]any) (map[string]any, error) {
	return map[string]any{
		"status":  "initialized",
		"task_id": task.ID,
	}, nil
}

// EndExecutor handles END lifecycle tasks.
type EndExecutor struct{}

func (e *EndExecutor) Execute(_ context.Context, task *domain.Task, input map[string]any) (map[string]any, error) {
	return map[string]any{
		"status":  "completed",
		"task_id": task.ID,
		"summary": input,
	}, nil
}

// SystemExecutor handles SYSTEM tasks by making outbound REST API calls.
// HTTP calls are dispatched in a goroutine with channel-based result signaling.
type SystemExecutor struct {
	HTTPClient *http.Client
}

func (e *SystemExecutor) Execute(ctx context.Context, task *domain.Task, input map[string]any) (map[string]any, error) {
	cfg := task.Config

	// MCP-like tool task: dispatch to the tool catalog (deterministic —
	// the input was resolved from the process context, no LLM involved).
	if cfg != nil && cfg.Tool != "" {
		out, err := mcptools.Execute(ctx, cfg.Tool, cfg.ToolConfig, input)
		if err != nil {
			var toolErr *mcptools.SystemError
			if errors.As(err, &toolErr) && toolErr.Response != nil {
				return toolErr.Response, &SystemTaskError{StatusCode: toolErr.StatusCode, Response: toolErr.Response}
			}
			return nil, err
		}
		return out, nil
	}

	if cfg == nil {
		return nil, fmt.Errorf("system task %s: missing config", task.ID)
	}
	method := cfg.Method
	if method == "" {
		method = http.MethodPost
	}

	body, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("marshal input: %w", err)
	}

	type result struct {
		data map[string]any
		err  error
	}
	ch := make(chan result, 1)

	go func() {
		req, err := http.NewRequestWithContext(ctx, method, cfg.URL, bytes.NewReader(body))
		if err != nil {
			ch <- result{err: fmt.Errorf("create request: %w", err)}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range cfg.Headers {
			req.Header.Set(k, v)
		}

		resp, err := e.HTTPClient.Do(req)
		if err != nil {
			ch <- result{err: fmt.Errorf("http %s %s: %w", method, cfg.URL, err)}
			return
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			ch <- result{err: fmt.Errorf("read response body: %w", err)}
			return
		}

		var data map[string]any
		if err := json.Unmarshal(respBody, &data); err != nil {
			data = map[string]any{
				"raw_body": string(respBody),
			}
		}
		data["status_code"] = resp.StatusCode
		// Treat non-2xx as a task failure so the engine can apply error-recovery
		// and still persist the response body as task_output.
		if resp.StatusCode >= 400 {
			ch <- result{data: data, err: &SystemTaskError{StatusCode: resp.StatusCode, Response: data}}
			return
		}
		ch <- result{data: data}
	}()

	select {
	case r := <-ch:
		return r.data, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
