package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/ledongthuc/pdf"

	"oumma/internal/ai"
	"oumma/internal/domain"
	"oumma/internal/engine"
	"oumma/internal/repository"
)

// flattenGroups normalizes a claims.Groups slice, splitting comma-separated
// entries, trimming whitespace and any leading "/", and preserving order.
func flattenGroups(src []string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(src))
	for _, g := range src {
		for _, part := range strings.Split(g, ",") {
			p := strings.TrimSpace(part)
			if p == "" {
				continue
			}
			p = strings.TrimPrefix(p, "/")
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	return out
}

// Handler exposes the workflow engine over HTTP.
type Handler struct {
	engine     *engine.Engine
	roleStore  RoleActionChecker
	formTracer FormTracer
	// formIDValidator, when set, checks that a launched form_id belongs to
	// the process's saved forms (oper-data). Nil = presence check only.
	formIDValidator func(processID, formID string) bool
	// mailer delivers outbound mail (quota claims) — SMTP via the Keycloak
	// realm settings, then HTTP gateway, then log mode.
	mailer func(ctx context.Context, to, subject, body string) (string, error)
}

// SetMailer wires the outbound mailer (auth.Service.SendMail).
func (h *Handler) SetMailer(fn func(ctx context.Context, to, subject, body string) (string, error)) {
	h.mailer = fn
}

// SetFormIDValidator wires the oper-data form_id ownership check for launches.
func (h *Handler) SetFormIDValidator(fn func(processID, formID string) bool) {
	h.formIDValidator = fn
}

// requireLaunchFormID enforces that a process instance can only be launched
// with a form_id — the identifier of the saved form (oper-data) the user
// filled in. It must be present and, when a validator is wired, must belong
// to the process.
func requireLaunchFormID(ctx context.Context, validator func(processID, formID string) bool, processID string, launchCtx map[string]any) error {
	fid, _ := launchCtx["form_id"].(string)
	if strings.TrimSpace(fid) == "" {
		return fmt.Errorf("launch requires form_id: the id of the saved form for the initial step")
	}
	if validator != nil && !validator(processID, fid) {
		return fmt.Errorf("form_id %s does not belong to process %s", fid, processID)
	}
	return nil
}

// RoleActionChecker enforces the per-role task-action permissions that the
// designer configures (processconfig.Store implements it).
type RoleActionChecker interface {
	CheckTaskAction(processID, taskName, action string, groups []string) error
	CheckTaskActionGroup(processID, taskName, group, action string, groups []string) error
	// CheckTaskActionGroupVersion checks against the deployed design version
	// a running instance was launched with (0 = live config).
	CheckTaskActionGroupVersion(processID, taskName, group, action string, groups []string, version int) error
}

func (h *Handler) SetRoleStore(s RoleActionChecker) { h.roleStore = s }

// FormTracer records a decision-form outcome in the oper (forms) store as a
// trace after a successful task completion. Wired in main.go. The version
// parameter is the instance's pinned design version (0 = live config) so the
// step lookup uses the form structure the instance was launched with.
type FormTracer func(ctx context.Context, processID, taskName string, version int, input map[string]any)

func (h *Handler) SetFormTracer(f FormTracer) { h.formTracer = f }

func New(eng *engine.Engine) *Handler {
	return &Handler{engine: eng}
}

// checkTaskPermission applies the designer-configured role permissions
// before a task action executes. No store wired → no extra restriction.
func (h *Handler) checkTaskPermission(w http.ResponseWriter, r *http.Request, processID, taskName, action string) bool {
	return h.checkTaskAction(w, r, processID, taskName, "action", action)
}

// checkTaskAction enforces the designer-configured permission for one
// (group, action) pair on a task — "action" for Complete/Claim/Release/skip,
// "comment" for comment read/write/update/delete, "attachment" for
// attachment read/write/update/delete.
func (h *Handler) checkTaskAction(w http.ResponseWriter, r *http.Request, processID, taskName, group, action string) bool {
	if h.roleStore == nil {
		return true
	}
	var groups []string
	if claims, ok := claimsFromContext(r.Context()); ok {
		groups = claims.Groups
	}
	// Running instances keep the permissions of the design version they
	// were launched with (the path carries the instance id).
	version := h.instanceVersion(r)
	if err := h.roleStore.CheckTaskActionGroupVersion(processID, taskName, group, action, groups, version); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return false
	}
	return true
}

// instanceVersion resolves the pinned design version of the instance in the
// request path (0 when unknown — checks then use the live config).
func (h *Handler) instanceVersion(r *http.Request) int {
	instID := r.PathValue("id")
	if instID == "" || h.engine == nil {
		return 0
	}
	if st, err := h.engine.GetStatus(r.Context(), instID); err == nil && st != nil {
		return st.DefinitionVersion
	}
	return 0
}

// RegisterRoutes wires up all HTTP endpoints onto the given mux.
//
// Route map (process_id = definition ID, id = instance UUID):
//
//	POST   /processes/{process_id}                                               — start a new instance
//	GET    /processes/{process_id}                                               — list all instances
//	GET    /processes/{process_id}/{id}                                          — get specific instance
//	DELETE /processes/{process_id}/{id}                                          — abort instance
//	POST   /processes/{process_id}/{id}/{signal}                                 — signal a running/paused instance
//	GET    /processes/{process_id}/{id}/tasks                                    — list active HUMAN tasks
//	GET    /processes/{process_id}/{id}/{task_name}/{task_id}                    — get task instance
//	POST   /processes/{process_id}/{id}/{task_name}/{task_id}                    — complete task
//	POST   /processes/{process_id}/{id}/{task_name}/{task_id}/comments           — add comment
//	GET    /processes/{process_id}/{id}/{task_name}/{task_id}/comments           — list comments
//	POST   /processes/{process_id}/{id}/{task_name}/{task_id}/attachments        — add attachment//	 GET    /processes/{process_id}/{id}/{task_name}/{task_id}/attachments        — list attachments//	DELETE /processes/{process_id}/{id}/{task_name}/{task_id}/attachments/{at_id}— delete attachment
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	// Process Instances
	mux.HandleFunc("POST /processes/{process_id}", h.StartProcess)
	// Ensure preflight CORS requests are handled for the streaming endpoint
	mux.HandleFunc("OPTIONS /processes/{process_id}/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /processes/{process_id}/stream", h.StartProcessSSE)
	mux.HandleFunc("GET /processes/{process_id}", h.ListInstances)
	mux.HandleFunc("GET /processes/{process_id}/token-usage", h.ProcessTokenUsage)
	mux.HandleFunc("GET /processes/{process_id}/{id}", h.GetInstance)
	mux.HandleFunc("POST /processes/{process_id}/{id}/ask", h.AskQuestion)
	mux.HandleFunc("DELETE /processes/{process_id}/{id}", h.AbortInstance)
	mux.HandleFunc("POST /processes/{process_id}/{id}/{signal}", h.SignalInstance)

	// Human Tasks
	mux.HandleFunc("GET /processes/{process_id}/{id}/tasks", h.ListHumanTasks)

	// User Tasks
	mux.HandleFunc("GET /processes/{process_id}/{id}/{task_name}/{task_id}", h.GetTaskInstance)
	mux.HandleFunc("POST /processes/{process_id}/{id}/{task_name}/{task_id}", h.CompleteTask)
	// Streaming completion: complete a HUMAN task and stream SSE until process terminal
	// Preflight handler for streaming completion
	mux.HandleFunc("OPTIONS /processes/{process_id}/{id}/{task_name}/{task_id}/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /processes/{process_id}/{id}/{task_name}/{task_id}/stream", h.CompleteTaskSSE)

	// Comments
	mux.HandleFunc("POST /processes/{process_id}/{id}/{task_name}/{task_id}/comments", h.AddComment)
	mux.HandleFunc("GET /processes/{process_id}/{id}/{task_name}/{task_id}/comments", h.ListComments)
	mux.HandleFunc("PUT /processes/{process_id}/{id}/{task_name}/{task_id}/comments/{comment_id}", h.UpdateComment)

	// Attachments
	mux.HandleFunc("POST /processes/{process_id}/{id}/{task_name}/{task_id}/attachments", h.AddAttachment)
	mux.HandleFunc("GET /processes/{process_id}/{id}/{task_name}/{task_id}/attachments", h.ListAttachments)
	mux.HandleFunc("DELETE /processes/{process_id}/{id}/{task_name}/{task_id}/attachments/{at_id}", h.DeleteAttachment)

	// Execution track
	mux.HandleFunc("GET /processes/{process_id}/{id}/track", h.GetTrack)

	// LLM call logs (debug)
	mux.HandleFunc("GET /processes/{process_id}/{id}/llm_logs", h.ListLLMLogs)

	// Task history for a specific instance
	mux.HandleFunc("GET /processes/{process_id}/{id}/taskshist", h.GetTaskHistory)

	// Search
	mux.HandleFunc("POST /processes/{process_id}/search", h.Search)

	// Definitions (kept for introspection)
	mux.HandleFunc("GET /definitions", h.ListDefinitions)

	// AI assist agent (normal users)
	mux.HandleFunc("POST /agent/chat", h.AgentChat)
	mux.HandleFunc("POST /agent/document", h.AgentDocument)
	mux.HandleFunc("GET /api/quota", h.QuotaStatus)
	mux.HandleFunc("POST /api/quota/claim", h.ClaimQuota)
	mux.HandleFunc("GET /processes/{process_id}/{id}/token-usage", h.InstanceTokenUsage)
	mux.HandleFunc("GET /processes/{process_id}/usage", h.RunningUsage)
}

// --- request / response types ---

type startRequest struct {
	Context map[string]any `json:"context,omitempty"`
}

type processResponse struct {
	ProcessID string `json:"process_id"`
	Status    string `json:"status"`
}

// StartProcessSSE handles POST /processes/{process_id}/stream.
// It starts a new process instance and streams SSE events until the process
// reaches a terminal state (COMPLETED, FAILED, or ABORTED) or the client
// disconnects. Each event is a JSON object on a `data:` line.
func (h *Handler) StartProcessSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	defID := r.PathValue("process_id")

	var initCtx map[string]any
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		defer r.Body.Close()
		_ = json.NewDecoder(r.Body).Decode(&initCtx)
	}

	if err := requireLaunchFormID(r.Context(), h.formIDValidator, defID, initCtx); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Inject caller identity so the engine can stamp per-task ownership.
	if claims, ok := claimsFromContext(r.Context()); ok {
		if initCtx == nil {
			initCtx = make(map[string]any)
		}
		initCtx["owner_id"] = claims.Username
		initCtx["owner_groups"] = claims.Groups
	}

	// Disable the server's write deadline for this long-lived SSE connection.
	// The default WriteTimeout (60 s) would kill the stream and trigger a
	// 'send on closed channel' panic in the engine goroutine.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	// Subscribe before starting so no events are dropped.
	processID, ch, err := h.engine.StartProcessWithStream(r.Context(), defID, initCtx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer h.engine.UnsubscribeSSE(processID, ch)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush() // send headers immediately so the client knows the stream is open

	for {
		select {
		case evt, open := <-ch:
			if !open {
				return
			}
			sendSSE(w, flusher, evt)
			if evt.Type == engine.SSEEventCompleted ||
				evt.Type == engine.SSEEventFailed ||
				evt.Type == engine.SSEEventAborted {
				return
			}
			// WAIT_HUMAN: keep the stream open so the client can observe further
			// events after the human task is resumed.
		case <-r.Context().Done():
			return
		}
	}
}

// StartProcess handles POST /processes/{process_id}.
// Body is free-form JSON passed as the initial process context.
func (h *Handler) StartProcess(w http.ResponseWriter, r *http.Request) {
	defID := r.PathValue("process_id")

	var ctx map[string]any
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		defer r.Body.Close()
		_ = json.NewDecoder(r.Body).Decode(&ctx)
	}

	if err := requireLaunchFormID(r.Context(), h.formIDValidator, defID, ctx); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Inject caller identity into the initial context so engine can populate
	// per-task ownership metadata. When no token is present this is skipped.
	if claims, ok := claimsFromContext(r.Context()); ok {
		if ctx == nil {
			ctx = make(map[string]any)
		}
		ctx["owner_id"] = claims.Username
		// store groups under owner_groups to avoid colliding with workflow fields
		ctx["owner_groups"] = claims.Groups
		// Department is mandatory on instances: the instance inherits the
		// launching user's departments so it is never invisible to them.
		if len(claims.Departments) > 0 {
			ctx["department"] = map[string]any{"codes": claims.Departments}
		}
	}

	processID, err := h.engine.StartProcess(r.Context(), defID, ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, processResponse{
		ProcessID: processID,
		Status:    "RUNNING",
	})
}

// ListInstances handles GET /{process_id}.
func (h *Handler) ListInstances(w http.ResponseWriter, r *http.Request) {
	defID := r.PathValue("process_id")

	instances, err := h.engine.ListInstances(r.Context(), defID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if instances == nil {
		instances = []*domain.ExecutionState{}
	}
	// Filter results to only the departments allowed by the caller's token.
	if claims, ok := claimsFromContext(r.Context()); ok {
		instances = filterByDepartment(instances, claims)
	}
	// Trim task_outputs to the last two entries for output.
	trimmed := trimTaskOutputsList(instances)
	writeJSON(w, http.StatusOK, trimmed)
}

// GetInstance handles GET /{process_id}/{id}.
func (h *Handler) GetInstance(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("id")

	state, err := h.engine.GetStatus(r.Context(), processID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	// Deny access when the instance belongs to a department outside the caller's scope.
	if claims, ok := claimsFromContext(r.Context()); ok {
		if allowed := allowedDeptSet(claims); allowed != nil && !instanceInDepartment(state, allowed) {
			writeError(w, http.StatusForbidden, "access denied: instance not in your department")
			return
		}
	}
	// Return a trimmed view so clients only see the last two task outputs.
	writeJSON(w, http.StatusOK, trimTaskOutputs(state))
}

// AskQuestion handles POST /{process_id}/{id}/ask — a conversational,
// context-grounded answer about the instance (replaces brief-llm-go for
// user queries). Body: {question, context?, lang?}.
func (h *Handler) AskQuestion(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("id")

	var body struct {
		Question string `json:"question"`
		Context  string `json:"context"`
		Lang     string `json:"lang"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Question) == "" {
		writeError(w, http.StatusBadRequest, "question is required")
		return
	}

	state, err := h.engine.GetStatus(r.Context(), processID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if claims, ok := claimsFromContext(r.Context()); ok {
		if allowed := allowedDeptSet(claims); allowed != nil && !instanceInDepartment(state, allowed) {
			writeError(w, http.StatusForbidden, "access denied: instance not in your department")
			return
		}
		// Attribute the user-prompt LLM calls to the caller (usage/cost ledger).
		var dept string
		if len(claims.Departments) > 0 {
			dept = claims.Departments[0]
		}
		r = r.WithContext(ai.ContextWithUsage(r.Context(), processID, ai.UsageInfo{
			Username:   claims.Username,
			Department: dept,
			Definition: state.DefinitionID,
		}))
	}

	answer, err := h.engine.AskQuestion(r.Context(), processID, strings.TrimSpace(body.Question), body.Context, body.Lang, r.Header.Get("Authorization"))
	if err != nil {
		writeError(w, http.StatusBadGateway, "ask: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"answer": answer, "process_id": processID})
}

// AbortInstance handles DELETE /{process_id}/{id}.
// Sends an abort signal; the engine marks the process as ABORTED.
func (h *Handler) AbortInstance(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("id")

	if err := h.engine.AbortProcess(r.Context(), processID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"process_id": processID,
		"status":     "ABORTED",
	})
}

// SignalInstance handles POST /{process_id}/{id}/{signal}.
// The path segment {signal} is the signal/event name (e.g. "unpaid").
// Body is optional freeform JSON passed as signal context.
func (h *Handler) SignalInstance(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("id")
	signalName := r.PathValue("signal")

	var body map[string]any
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		defer r.Body.Close()
		_ = json.NewDecoder(r.Body).Decode(&body)
	}

	if err := h.engine.SignalProcess(r.Context(), processID, signalName, body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{
		"process_id": processID,
		"signal":     signalName,
		"status":     "DELIVERED",
	})
}

// ListHumanTasks handles GET /processes/{process_id}/{id}/tasks.
// Optional query params (both validated against the caller's token claims):
//
//	?user=<username>  — only return tasks if the token username matches; 403 otherwise.
//	?group=<group>    — only return tasks whose assigned group matches, and the caller
//	                    must belong to that group; 403 otherwise.
//
// Returns an empty array when no HUMAN task is currently active or after filtering.
func (h *Handler) ListHumanTasks(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("id")

	tasks, err := h.engine.ListActiveHumanTasks(r.Context(), processID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	claims, hasClaims := claimsFromContext(r.Context())

	// --- ?user filter ---
	if userFilter := r.URL.Query().Get("user"); userFilter != "" {
		if !hasClaims || claims.Username != userFilter {
			writeError(w, http.StatusForbidden, "access denied: user filter does not match your token identity")
			return
		}
	}

	// --- ?group filter ---
	if groupFilter := r.URL.Query().Get("group"); groupFilter != "" {
		if !hasClaims || !sliceContains(claims.Groups, groupFilter) {
			writeError(w, http.StatusForbidden, "access denied: group filter references a group you do not belong to")
			return
		}
		filtered := tasks[:0]
		for _, t := range tasks {
			if t.Group == groupFilter {
				filtered = append(filtered, t)
			}
		}
		tasks = filtered
	} else if hasClaims && len(claims.Groups) > 0 {
		// Default: callers with groups only see tasks of their own groups.
		// Callers without groups (designers/admins, service tokens) see all.
		filtered := tasks[:0]
		for _, t := range tasks {
			if sliceContains(claims.Groups, t.Group) {
				filtered = append(filtered, t)
			}
		}
		tasks = filtered
	}

	writeJSON(w, http.StatusOK, tasks)
}

// sliceContains reports whether s contains target (case-sensitive).
func sliceContains(s []string, target string) bool {
	for _, v := range s {
		if v == target {
			return true
		}
	}
	return false
}

// GetTaskInstance handles GET /{process_id}/{id}/{task_name}/{task_id}.
func (h *Handler) GetTaskInstance(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("id")
	taskName := r.PathValue("task_name")
	taskID := r.PathValue("task_id")

	info, err := h.engine.GetTaskInstance(r.Context(), processID, taskName, taskID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// parseCompleteTaskBody decodes the request body for task-completion endpoints.
// It accepts two body shapes:
//
//   - Structured: {"input": {...}, "override": "...", "lang": "..."}
//   - Flat:       {...all task fields..., "override": "...", "lang": "..."}
//
// In the flat case every field except "lang" and "override" is treated as task
// input. "lang" is injected back into the returned input map so the engine
// merges it into global_context automatically.
func parseCompleteTaskBody(r *http.Request, w http.ResponseWriter) (input map[string]any, override, lang string, err error) {
	if r.Body == nil {
		return nil, "", "", nil
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()

	var raw map[string]any
	if decErr := json.NewDecoder(r.Body).Decode(&raw); decErr != nil {
		return nil, "", "", decErr
	}

	// Extract control fields before deciding the input shape.
	if v, ok := raw["lang"]; ok {
		if s, ok := v.(string); ok {
			lang = s
		}
		delete(raw, "lang")
	}
	if v, ok := raw["override"]; ok {
		if s, ok := v.(string); ok {
			override = s
		}
		delete(raw, "override")
	}

	// Structured body: reuse the nested "input" sub-map.
	// Flat body: the remaining map is the task input.
	if v, ok := raw["input"]; ok {
		if m, ok := v.(map[string]any); ok {
			input = m
		}
	} else {
		input = raw
	}

	// Propagate lang into input so it merges into global_context on resume.
	if lang != "" {
		if input == nil {
			input = make(map[string]any)
		}
		input["lang"] = lang
	}

	return input, override, lang, nil
}

// CompleteTask handles POST /{process_id}/{id}/{task_name}/{task_id}.
// Completes the paused HUMAN task and resumes process execution.
// Existing task comments are automatically injected into the LLM routing context.
func (h *Handler) CompleteTask(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("id")
	taskName := r.PathValue("task_name")
	taskID := r.PathValue("task_id")

	input, override, _, err := parseCompleteTaskBody(r, w)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Extract the caller's identity so the engine can stamp the task entry.
	var actorID string
	var actorGroups any
	var actorDepts []string
	if claims, ok := claimsFromContext(r.Context()); ok {
		actorID = claims.Username
		actorGroups = claims.Groups
		actorDepts = claims.Departments
	}

	if !h.checkTaskPermission(w, r, r.PathValue("process_id"), taskName, "Complete") {
		return
	}

	if err := h.engine.CompleteTask(r.Context(), processID, taskName, taskID, input, override, actorID, actorGroups, actorDepts); err != nil {
		if errors.Is(err, engine.ErrAccessDenied) {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if h.formTracer != nil {
		h.formTracer(r.Context(), processID, taskName, h.instanceVersion(r), input)
	}
	writeJSON(w, http.StatusAccepted, map[string]string{
		"process_id": processID,
		"task_id":    taskName,
		"status":     "RESUMED",
	})
}

type addCommentRequest struct {
	Content string `json:"content"`
}

// AddComment handles POST /{process_id}/{id}/{task_name}/{task_id}/comments.
func (h *Handler) AddComment(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("id")
	taskName := r.PathValue("task_name")
	taskID := r.PathValue("task_id")

	if !h.checkTaskAction(w, r, r.PathValue("process_id"), taskName, "comment", "write") {
		return
	}

	var req addCommentRequest
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	if strings.TrimSpace(req.Content) == "" {
		writeError(w, http.StatusBadRequest, "content is required")
		return
	}

	var createdBy string
	var group string
	if claims, ok := claimsFromContext(r.Context()); ok {
		createdBy = claims.Username
		flat := flattenGroups(claims.Groups)
		if len(flat) > 0 {
			group = flat[0]
		}
	}

	comment, err := h.engine.AddComment(r.Context(), processID, taskName, taskID, req.Content, createdBy, group)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, comment)
}

// ListComments handles GET /{process_id}/{id}/{task_name}/{task_id}/comments.
func (h *Handler) ListComments(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("id")

	if !h.checkTaskAction(w, r, r.PathValue("process_id"), r.PathValue("task_name"), "comment", "read") {
		return
	}

	// Parse allowed_groups query parameter. Accept comma-separated or
	// repeated keys: ?allowed_groups=a,b&allowed_groups=c
	var reqGroups []string
	if vals, ok := r.URL.Query()["allowed_groups"]; ok {
		for _, v := range vals {
			for _, part := range strings.Split(v, ",") {
				p := strings.TrimSpace(part)
				p = strings.TrimPrefix(p, "/")
				if p != "" {
					reqGroups = append(reqGroups, p)
				}
			}
		}
	}

	// Build allowed group set from the requested groups. We no longer
	// validate requested groups against the caller token; the endpoint
	// simply filters comments by their stored `Group` column matching any
	// of the `allowed_groups` values. If no `allowed_groups` are provided
	// we return all comments.
	claims, hasClaims := claimsFromContext(r.Context())
	allowed := make(map[string]struct{})
	for _, g := range reqGroups {
		gn := strings.ToLower(strings.TrimSpace(g))
		if gn == "" {
			continue
		}
		allowed[gn] = struct{}{}
	}

	comments, err := h.engine.ListCommentsByProcess(r.Context(), processID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if comments == nil {
		comments = []*domain.TaskComment{}
	}

	// If allowed is empty, return all comments. Otherwise include comments
	// whose stored Group is in the allowed set. Also include the caller's
	// own comments so users always see what they created.
	if len(allowed) > 0 {
		out := make([]*domain.TaskComment, 0, len(comments))
		for _, c := range comments {
			if hasClaims && c.CreatedBy == claims.Username {
				out = append(out, c)
				continue
			}
			if c.Group != "" {
				if _, ok := allowed[strings.ToLower(strings.TrimSpace(c.Group))]; ok {
					out = append(out, c)
				}
			}
		}
		comments = out
	}

	writeJSON(w, http.StatusOK, comments)
}

type updateCommentRequest struct {
	Content string `json:"content"`
}

// UpdateComment handles PUT /{process_id}/{id}/{task_name}/{task_id}/comments/{comment_id}.
// Only content, updated_by, and updated_at are modified.
func (h *Handler) UpdateComment(w http.ResponseWriter, r *http.Request) {
	commentID := r.PathValue("comment_id")

	if !h.checkTaskAction(w, r, r.PathValue("process_id"), r.PathValue("task_name"), "comment", "update") {
		return
	}

	var req updateCommentRequest
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	if strings.TrimSpace(req.Content) == "" {
		writeError(w, http.StatusBadRequest, "content is required")
		return
	}

	var updatedBy string
	if claims, ok := claimsFromContext(r.Context()); ok {
		updatedBy = claims.Username
	}

	comment, err := h.engine.UpdateComment(r.Context(), commentID, req.Content, updatedBy)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, comment)
}

type addAttachmentRequest struct {
	Name string `json:"name"`
	URI  string `json:"uri"`
}

// AddAttachment handles POST /{process_id}/{id}/{task_name}/{task_id}/attachments.
func (h *Handler) AddAttachment(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("id")
	taskName := r.PathValue("task_name")
	taskID := r.PathValue("task_id")

	if !h.checkTaskAction(w, r, r.PathValue("process_id"), taskName, "attachment", "write") {
		return
	}

	var req addAttachmentRequest
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.URI) == "" {
		writeError(w, http.StatusBadRequest, "name and uri are required")
		return
	}

	var createdBy string
	var group string
	if claims, ok := claimsFromContext(r.Context()); ok {
		createdBy = claims.Username
		flat := flattenGroups(claims.Groups)
		if len(flat) > 0 {
			group = flat[0]
		}
	}

	attachment, err := h.engine.AddAttachment(r.Context(), processID, taskName, taskID, req.Name, req.URI, createdBy, group)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, attachment)
}

// ListAttachments handles GET /{process_id}/{id}/{task_name}/{task_id}/attachments.
func (h *Handler) ListAttachments(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("id")

	if !h.checkTaskAction(w, r, r.PathValue("process_id"), r.PathValue("task_name"), "attachment", "read") {
		return
	}

	// Parse allowed_groups query parameter (comma-separated or repeated).
	var reqGroups []string
	if vals, ok := r.URL.Query()["allowed_groups"]; ok {
		for _, v := range vals {
			for _, part := range strings.Split(v, ",") {
				p := strings.TrimSpace(part)
				p = strings.TrimPrefix(p, "/")
				if p != "" {
					reqGroups = append(reqGroups, p)
				}
			}
		}
	}

	// Build allowed group set from the requested groups. We no longer
	// validate requested groups against the caller token; the endpoint
	// simply filters attachments by their stored `Group` column matching any
	// of the `allowed_groups` values. If no `allowed_groups` are provided
	// we return all attachments.
	claims, hasClaims := claimsFromContext(r.Context())
	allowed := make(map[string]struct{})
	for _, g := range reqGroups {
		gn := strings.ToLower(strings.TrimSpace(g))
		if gn == "" {
			continue
		}
		allowed[gn] = struct{}{}
	}

	attachments, err := h.engine.ListAttachmentsByProcess(r.Context(), processID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if attachments == nil {
		attachments = []*domain.TaskAttachment{}
	}

	// If allowed is empty, return all attachments. Otherwise include
	// attachments whose stored Group is in the allowed set. Also include
	// the caller's own attachments so users always see what they uploaded.
	if len(allowed) > 0 {
		out := make([]*domain.TaskAttachment, 0, len(attachments))
		for _, a := range attachments {
			if hasClaims && a.CreatedBy == claims.Username {
				out = append(out, a)
				continue
			}
			if a.Group != "" {
				if _, ok := allowed[strings.ToLower(strings.TrimSpace(a.Group))]; ok {
					out = append(out, a)
				}
			}
		}
		attachments = out
	}

	writeJSON(w, http.StatusOK, attachments)
}

// DeleteAttachment handles DELETE /{process_id}/{id}/{task_name}/{task_id}/attachments/{at_id}.
// The attachment is soft-deleted (marked deleted in DB, not physically removed).
func (h *Handler) DeleteAttachment(w http.ResponseWriter, r *http.Request) {
	attachmentID := r.PathValue("at_id")

	if !h.checkTaskAction(w, r, r.PathValue("process_id"), r.PathValue("task_name"), "attachment", "delete") {
		return
	}

	if err := h.engine.DeleteAttachment(r.Context(), attachmentID); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"attachment_id": attachmentID,
		"status":        "DELETED",
	})
}

// ListDefinitions handles GET /definitions.
func (h *Handler) ListDefinitions(w http.ResponseWriter, r *http.Request) {
	// Live view: active deployed designs replace the file-registered
	// definitions, so the designer diagram matches what new instances run.
	defs, err := h.engine.ListDefinitionsLive(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if defs == nil {
		defs = []*domain.ProcessDefinition{}
	}
	writeJSON(w, http.StatusOK, defs)
}

// GetTrack handles GET /processes/{process_id}/{id}/track.
// Returns the ordered execution history for the instance — one entry per LLM
// routing decision — from the first task to the current (or final) step.
func (h *Handler) GetTrack(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("id")
	steps, err := h.engine.ListTrackSteps(r.Context(), processID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if steps == nil {
		steps = []*domain.ProcessTrackStep{}
	}
	writeJSON(w, http.StatusOK, steps)
}

// ListLLMLogs handles GET /processes/{process_id}/{id}/llm_logs.
// Returns the ordered LLM call logs for the given process instance.
func (h *Handler) ListLLMLogs(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("id")

	logs, err := h.engine.ListLLMLogs(r.Context(), processID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if logs == nil {
		logs = []*domain.LLMLog{}
	}
	writeJSON(w, http.StatusOK, logs)
}

// GetTaskHistory handles GET /processes/{process_id}/{id}/taskshist.
// Returns the ordered task execution entries for the given process instance.
func (h *Handler) GetTaskHistory(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("id")
	// Load state for access control check
	state, err := h.engine.GetStatus(r.Context(), processID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if claims, ok := claimsFromContext(r.Context()); ok {
		if allowed := allowedDeptSet(claims); allowed != nil && !instanceInDepartment(state, allowed) {
			writeError(w, http.StatusForbidden, "access denied: instance not in your department")
			return
		}
	}

	// Fetch persisted histories
	histories, err := h.engine.ListTaskHistories(r.Context(), processID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := make([]map[string]any, 0, len(histories))
	for _, hh := range histories {
		cp := make(map[string]any)
		cp["chronological_order"] = hh.ChronologicalOrder
		if !hh.CreatedAt.IsZero() {
			cp["created_at"] = hh.CreatedAt.Format(time.RFC3339)
		} else {
			cp["created_at"] = nil
		}
		if hh.CompletedAt != nil {
			cp["completed_at"] = hh.CompletedAt.Format(time.RFC3339)
		} else {
			cp["completed_at"] = nil
		}
		if len(hh.Groups) > 0 {
			cp["group"] = hh.Groups
		}
		if hh.OwnerID != "" {
			cp["owner_id"] = hh.OwnerID
		}
		cp["task_id"] = hh.TaskID
		if hh.TaskInstanceID != "" {
			cp["task_instance_id"] = hh.TaskInstanceID
		} else {
			cp["task_instance_id"] = ""
		}
		if hh.TaskOutput != nil {
			cp["task_output"] = hh.TaskOutput
		} else {
			cp["task_output"] = nil
		}
		cp["state"] = hh.State
		out = append(out, cp)
	}

	writeJSON(w, http.StatusOK, out)
}

// --- helpers ---

// sendSSE writes a single SSE frame and flushes the connection.
func sendSSE(w http.ResponseWriter, f http.Flusher, evt engine.SSEEvent) {
	data, _ := json.Marshal(evt)
	fmt.Fprintf(w, "data: %s\n\n", data)
	f.Flush()
}

// CompleteTaskSSE handles POST /{process_id}/{id}/{task_name}/{task_id}/stream.
// It completes the paused HUMAN task (like CompleteTask) and streams SSE
// events for the resumed process until it reaches a terminal state.
func (h *Handler) CompleteTaskSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	processID := r.PathValue("id")
	taskName := r.PathValue("task_name")
	taskID := r.PathValue("task_id")

	input, override, _, err := parseCompleteTaskBody(r, w)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Extract the caller's identity so the engine can stamp the task entry.
	var actorID string
	var actorGroups any
	var actorDepts []string
	if claims, ok := claimsFromContext(r.Context()); ok {
		actorID = claims.Username
		actorGroups = claims.Groups
		actorDepts = claims.Departments
	}

	if !h.checkTaskPermission(w, r, r.PathValue("process_id"), taskName, "Complete") {
		return
	}

	// Subscribe first so we don't miss any events emitted as the engine resumes.
	ch := h.engine.SubscribeSSE(processID)
	defer h.engine.UnsubscribeSSE(processID, ch)

	if err := h.engine.CompleteTask(r.Context(), processID, taskName, taskID, input, override, actorID, actorGroups, actorDepts); err != nil {
		if errors.Is(err, engine.ErrAccessDenied) {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if h.formTracer != nil {
		h.formTracer(r.Context(), processID, taskName, h.instanceVersion(r), input)
	}

	// Disable write deadline for this SSE stream.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case evt, open := <-ch:
			if !open {
				return
			}
			sendSSE(w, flusher, evt)

			switch evt.Type {
			case engine.SSEEventForkBranchDone:
				// For parallel fork branches, the SSE stream should close as soon as
				// this branch is done and it is NOT the last required branch. When it
				// IS the last, executeFork will emit ForkJoined next and the process
				// continues — keep streaming until the next terminal event.
				if instanceIDInEvt, _ := evt.Data["instance_id"].(string); instanceIDInEvt == taskID {
					// This event is for our specific branch.
					isLast, _ := evt.Data["is_last"].(bool)
					if !isLast {
						// Other branches still pending — our turn is over.
						return
					}
					// We were the last branch; ForkJoined will follow — keep streaming.
				}

			case engine.SSEEventCompleted,
				engine.SSEEventFailed,
				engine.SSEEventAborted,
				engine.SSEEventWaitHuman:
				// SSEEventWaitHuman covers both the normal "workflow reached a new
				// human step" case and the SYSTEM-task-error recovery case (where the
				// engine routes back to the previous human task). In both situations
				// the current completion cycle is finished; the client must interact
				// with the re-paused task to continue.
				return
			}

		case <-r.Context().Done():
			return
		}
	}
}

// Search handles POST /processes/{process_id}/search.
// Body: { "q": "keyword", "groups": ["1001", "1002"] }
//
// Access control for groups:
//   - When the request carries a valid JWT, the effective groups used for
//     filtering are the INTERSECTION of the requested groups and the caller's
//     token groups. If the caller requests a group they do not belong to a
//     403 is returned.
//   - When no JWT is present (unauthenticated path), the requested groups are
//     used as-is.
//   - When groups is omitted/empty and a JWT is present, the search is
//     automatically scoped to the caller's own token groups.
func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	defID := r.PathValue("process_id")

	var req struct {
		Q      string   `json:"q"`
		Groups []string `json:"groups"`
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}

	// Enforce group-level access control against the caller's token.
	effectiveGroups := req.Groups
	if claims, ok := claimsFromContext(r.Context()); ok {
		tokenGroupSet := make(map[string]struct{}, len(claims.Groups))
		for _, g := range claims.Groups {
			tokenGroupSet[strings.ToLower(strings.TrimSpace(g))] = struct{}{}
		}

		if len(req.Groups) == 0 {
			// No groups requested — scope to the caller's own token groups.
			effectiveGroups = claims.Groups
		} else {
			// Requested groups must all be present in the token.
			effectiveGroups = effectiveGroups[:0]
			for _, g := range req.Groups {
				if _, ok := tokenGroupSet[strings.ToLower(strings.TrimSpace(g))]; !ok {
					writeError(w, http.StatusForbidden, "access denied: group '"+g+"' is not in your token")
					return
				}
				effectiveGroups = append(effectiveGroups, g)
			}
		}
	}

	// Department gate: the token must carry at least one department code.
	// Results are then filtered to those whose global_context["department"]["codes"]
	// overlaps with the token departments. Missing or empty token departments → 403.
	claims, hasClaims := claimsFromContext(r.Context())
	if !hasClaims || len(claims.Departments) == 0 {
		writeError(w, http.StatusForbidden, "access denied: no department claim in token")
		return
	}
	allowed := make(map[string]struct{}, len(claims.Departments))
	for _, d := range claims.Departments {
		allowed[strings.ToLower(strings.TrimSpace(d))] = struct{}{}
	}

	results, err := h.engine.SearchInstances(r.Context(), defID, req.Q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Keep only results whose department codes overlap with the token departments.
	filtered := results[:0]
	for _, inst := range results {
		if instanceDeptOverlaps(inst, allowed) {
			filtered = append(filtered, inst)
		}
	}
	if filtered == nil {
		filtered = []*domain.ExecutionState{}
	}
	writeJSON(w, http.StatusOK, filtered)
}

// instanceDeptOverlaps returns true when global_context["department"]["codes"]
// contains at least one code present in allowed.
func instanceDeptOverlaps(inst *domain.ExecutionState, allowed map[string]struct{}) bool {
	if inst.GlobalContext == nil {
		return false
	}
	raw, ok := inst.GlobalContext["department"]
	if !ok {
		return false
	}
	deptMap, ok := raw.(map[string]any)
	if !ok {
		return false
	}
	codes, ok := deptMap["codes"]
	if !ok {
		return false
	}
	if arr, ok := codes.([]any); ok {
		for _, c := range arr {
			if s, ok := c.(string); ok {
				if _, found := allowed[strings.ToLower(strings.TrimSpace(s))]; found {
					return true
				}
			}
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// trimTaskOutputs returns a shallow copy of state with TaskOutputs reduced
// to only the last two entries to avoid returning large histories to clients.
func trimTaskOutputs(state *domain.ExecutionState) *domain.ExecutionState {
	if state == nil {
		return nil
	}
	out := *state
	if len(state.TaskOutputs) > 2 {
		out.TaskOutputs = state.TaskOutputs[len(state.TaskOutputs)-2:]
	} else {
		out.TaskOutputs = state.TaskOutputs
	}
	return &out
}

// trimTaskOutputsList applies trimTaskOutputs to each element of a slice.
func trimTaskOutputsList(states []*domain.ExecutionState) []*domain.ExecutionState {
	if states == nil {
		return []*domain.ExecutionState{}
	}
	out := make([]*domain.ExecutionState, 0, len(states))
	for _, s := range states {
		out = append(out, trimTaskOutputs(s))
	}
	return out
}

// ---------------------------------------------------------------------------
// AI assist agent (normal users)
//
// POST /agent/chat      — multi-message LLM conversation for the client-side
//                         agent loop. Body: {messages:[{role, content}],
//                         jsonMode?}. Content may be a string or a list of
//                         text/image_url parts (multimodal).
// POST /agent/document  — fetch a DMS document for analysis. Body: {url}.
//                         PDFs are reduced to their extracted text; images
//                         are returned as base64 so the client can send them
//                         to the model as an image part.
// ---------------------------------------------------------------------------

func (h *Handler) AgentChat(w http.ResponseWriter, r *http.Request) {
	// Monthly token quota (normal users only; designers are unlimited).
	if claims, ok := claimsFromContext(r.Context()); ok && h.EnforceQuota(w, r, claims) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20) // base64 images can be large
	defer r.Body.Close()

	var body struct {
		Messages   []ai.ChatMessage `json:"messages"`
		JSONMode   bool             `json:"jsonMode"`
		ProcessID  string           `json:"processId"`  // definition (usage dimension)
		InstanceID string           `json:"instanceId"` // optional: attribute to the active dossier
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages are required")
		return
	}

	// Attribute the user-prompt LLM calls to the caller (usage/cost ledger).
	if claims, ok := claimsFromContext(r.Context()); ok {
		var dept string
		if len(claims.Departments) > 0 {
			dept = claims.Departments[0]
		}
		logID := body.InstanceID
		if logID == "" {
			logID = body.ProcessID
		}
		r = r.WithContext(ai.ContextWithUsage(r.Context(), logID, ai.UsageInfo{
			Username:   claims.Username,
			Department: dept,
			Definition: body.ProcessID,
		}))
	}

	answer, err := h.engine.AgentChat(r.Context(), body.Messages, body.JSONMode)
	if err != nil {
		writeError(w, http.StatusBadGateway, "agent chat: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"answer": answer})
}

// agentDocumentMaxBytes caps how much of a document the agent may pull.
const agentDocumentMaxBytes = 8 << 20

func (h *Handler) AgentDocument(w http.ResponseWriter, r *http.Request) {
	// Monthly token quota (normal users only; designers are unlimited).
	if claims, ok := claimsFromContext(r.Context()); ok && h.EnforceQuota(w, r, claims) {
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.URL) == "" {
		writeError(w, http.StatusBadRequest, "url is required")
		return
	}
	u, err := url.Parse(body.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		writeError(w, http.StatusBadRequest, "url must be http(s)")
		return
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(body.URL)
	if err != nil {
		writeError(w, http.StatusBadGateway, "fetch document: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("fetch document: status %d", resp.StatusCode))
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, agentDocumentMaxBytes))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read document: "+err.Error())
		return
	}
	if len(data) == 0 {
		writeError(w, http.StatusBadGateway, "document is empty")
		return
	}

	ext := strings.ToLower(filepath.Ext(u.Path))
	switch ext {
	case ".pdf":
		text, err := pdfText(data)
		if err != nil {
			writeError(w, http.StatusBadGateway, "pdf text extraction: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"kind": "pdf", "text": text, "size": len(data)})
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".bmp":
		mime := mime.TypeByExtension(ext)
		if mime == "" {
			mime = "application/octet-stream"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"kind": "image",
			"mime": mime,
			"b64":  base64.StdEncoding.EncodeToString(data),
			"size": len(data),
		})
	default:
		writeError(w, http.StatusUnsupportedMediaType,
			"unsupported document type "+ext+" (pdf, png, jpg, webp, gif, bmp)")
	}
}

// pdfText extracts the readable text of a PDF, one page at a time, capped at
// ~60KB so the LLM context stays bounded.
func pdfText(data []byte) (string, error) {
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}

	var b strings.Builder
	for p := 1; p <= r.NumPage() && b.Len() < 60000; p++ {
		page := r.Page(p)
		t, err := page.GetPlainText(nil)
		if err != nil {
			continue
		}
		b.WriteString(t)
		b.WriteString("\n")
	}
	text := strings.TrimSpace(b.String())
	if text == "" {
		return "", fmt.Errorf("no extractable text (scanned PDF?)")
	}
	return text, nil
}

// InstanceTokenUsage handles GET /processes/{process_id}/{id}/token-usage —
// the LLM tokens consumed by this instance (shown in the workspace).
func (h *Handler) InstanceTokenUsage(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("id")
	p, c, n, err := h.engine.TokenUsage(r.Context(), processID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"prompt_tokens":     p,
		"completion_tokens": c,
		"total_tokens":      p + c,
		"calls":             n,
		"cost_usd":          float64(n) * repository.CostPerCallUSD,
	})
}

// ProcessTokenUsage handles GET /processes/{process_id}/token-usage —
// tokens across every instance of the process definition (designer view).
func (h *Handler) ProcessTokenUsage(w http.ResponseWriter, r *http.Request) {
	defID := r.PathValue("process_id")
	p, c, n, err := h.engine.TokenUsageByDefinition(r.Context(), defID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"prompt_tokens":     p,
		"completion_tokens": c,
		"total_tokens":      p + c,
		"calls":             n,
		"cost_usd":          float64(n) * repository.CostPerCallUSD,
	})
}

// RunningUsage handles GET /processes/{process_id}/usage — the current
// month's running usage counters per (user, department) for the process,
// plus the total. The monthly archive (usage_monthly) is written at month
// end and resets these counters.
func (h *Handler) RunningUsage(w http.ResponseWriter, r *http.Request) {
	defID := r.PathValue("process_id")
	p, c, n, err := h.engine.TokenUsageByDefinition(r.Context(), defID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"month":           time.Now().UTC().Format("2006-01"),
		"calls":           n,
		"prompt_tokens":   p,
		"completion_tokens": c,
		"cost_usd":        float64(n) * repository.CostPerCallUSD,
		"cost_per_call":   repository.CostPerCallUSD,
	})
}
