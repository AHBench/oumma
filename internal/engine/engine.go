package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"oumma/internal/ai"
	"oumma/internal/domain"
	"oumma/internal/events"
	"oumma/internal/repository"
)

// toolInputTemplateRe matches {{field}} placeholders in tool-task input values.
var toolInputTemplateRe = regexp.MustCompile(`\{\{\s*([\w$.\-]+)\s*\}\}`)

// Engine is the core workflow execution loop.
type Engine struct {
	repo            repository.Repository
	llm             ai.LLMClient
	executors       map[domain.TaskType]TaskExecutor
	logger          *slog.Logger
	logStore        repository.LLMLogStore
	cpStore         repository.CheckpointStore
	commentStore    repository.TaskCommentStore
	attachmentStore repository.TaskAttachmentStore
	trackStore      repository.TrackStore
	designs         repository.DesignStore
	dispatcher      events.Dispatcher
	serviceTools    ServiceTools

	mu        sync.RWMutex
	resumeChs map[string]chan resumePayload
	sseBus    map[string][]chan SSEEvent // processID → subscriber channels

	// live tracks process IDs whose execution goroutine is currently alive.
	// The reaper (StartReaper) fails RUNNING processes that are missing from
	// this set — i.e. orphaned by a dead execution loop.
	liveMu sync.Mutex
	live   map[string]struct{}
}

// SSEEventType classifies each Server-Sent Event emitted by the engine.
type SSEEventType string

const (
	SSEEventStarted         SSEEventType = "STARTED"           // process instance created
	SSEEventTaskEntered     SSEEventType = "TASK_ENTERED"      // engine is now executing a task
	SSEEventWaitHuman       SSEEventType = "WAIT_HUMAN"        // paused — awaiting human input
	SSEEventLLMCompleted    SSEEventType = "LLM_COMPLETED"     // LLM decision and task activation completed
	SSEEventLLMRouted       SSEEventType = "LLM_ROUTED"        // LLM made a routing decision
	SSEEventTaskComplete    SSEEventType = "TASK_COMPLETE"     // a single task finished
	SSEEventCompleted       SSEEventType = "COMPLETED"         // entire process finished
	SSEEventFailed          SSEEventType = "FAILED"            // process failed
	SSEEventAborted         SSEEventType = "ABORTED"           // process aborted
	SSEEventSystemTaskError SSEEventType = "SYSTEM_TASK_ERROR" // SYSTEM task failed — routed back to previous HUMAN step
	SSEEventForkStarted     SSEEventType = "FORK_STARTED"      // parallel fork launched
	SSEEventForkBranchDone  SSEEventType = "FORK_BRANCH_DONE"  // one parallel branch completed
	SSEEventForkJoined      SSEEventType = "FORK_JOINED"       // all required branches joined; routing resumes
)

// SSEEvent is one Server-Sent Event payload.
type SSEEvent struct {
	Type      SSEEventType   `json:"type"`
	ProcessID string         `json:"process_id"`
	TaskID    string         `json:"task_id,omitempty"`
	NextTask  string         `json:"next_task,omitempty"`
	Reasoning string         `json:"reasoning,omitempty"`
	Error     string         `json:"error,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}

// defaultLLMCallTimeout is the per-call timeout applied to LLM requests.
// Override with LLM_CALL_TIMEOUT_SEC env var (default 180 s).
var defaultLLMCallTimeout = time.Duration(getEnvIntInit("LLM_CALL_TIMEOUT_SEC", 180)) * time.Second

// getEnvIntInit reads an integer env var at package initialisation time,
// before getEnvInt is available inside a method receiver.
func getEnvIntInit(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// resumePayload carries both the human input and optional override directive.
type resumePayload struct {
	Input    map[string]any
	Override string
	Abort    bool
	// ActorID and ActorGroups carry the identity of the user who completed
	// the HUMAN task so the engine can stamp the task entry with them.
	ActorID     string
	ActorGroups any
}

// forkBranchResult is the outcome of one parallel branch sent back to executeFork.
type forkBranchResult struct {
	taskID      string
	instanceID  string
	output      map[string]any
	actorID     string
	actorGroups any
	cancelled   bool // true when aborted or context-cancelled
	err         error
}

// errProcessAborted is returned by executeFork when an abort signal is received.
// The caller must not call failProcess after receiving this error.
var errProcessAborted = fmt.Errorf("process aborted by user")

// New creates a new Engine with all dependencies injected.
func New(repo repository.Repository, llm ai.LLMClient, logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{
		repo:   repo,
		llm:    llm,
		logger: logger,
		executors: map[domain.TaskType]TaskExecutor{
			domain.TaskTypeStart:  &StartExecutor{},
			domain.TaskTypeEnd:    &EndExecutor{},
			domain.TaskTypeSystem: &SystemExecutor{HTTPClient: &http.Client{Timeout: 30 * time.Second}},
		},
		resumeChs: make(map[string]chan resumePayload),
		sseBus:    make(map[string][]chan SSEEvent),
		live:      make(map[string]struct{}),
	}
}

// SetLogStore sets the LLM log store for persisting and querying LLM call logs.
func (e *Engine) SetLogStore(store repository.LLMLogStore) {
	e.logStore = store
}

// SetCheckpointStore sets the checkpoint store for crash-safe resumption of running processes.
func (e *Engine) SetCheckpointStore(store repository.CheckpointStore) {
	e.cpStore = store
}

// SetCommentStore sets the store for task comments.
func (e *Engine) SetCommentStore(store repository.TaskCommentStore) {
	e.commentStore = store
}

// SetAttachmentStore sets the store for task attachments.
func (e *Engine) SetAttachmentStore(store repository.TaskAttachmentStore) {
	e.attachmentStore = store
}

// SetTrackStore sets the store for process execution track steps.
func (e *Engine) SetTrackStore(store repository.TrackStore) {
	e.trackStore = store
}

// SetEventDispatcher sets the Kafka event dispatcher.
// When set, workflow mutations are published as events in addition to being persisted.
func (e *Engine) SetEventDispatcher(d events.Dispatcher) {
	e.dispatcher = d
}

// saveCheckpoint persists a checkpoint if a checkpoint store is configured, best-effort.
func (e *Engine) saveCheckpoint(ctx context.Context, cp *domain.ProcessCheckpoint) {
	if e.cpStore != nil {
		cp.UpdatedAt = time.Now().UTC()
		_ = e.cpStore.SaveCheckpoint(ctx, cp)
	}
}

// getCheckpoint loads a checkpoint if a checkpoint store is configured.
func (e *Engine) getCheckpoint(ctx context.Context, processID string) *domain.ProcessCheckpoint {
	if e.cpStore == nil {
		return nil
	}
	cp, _ := e.cpStore.GetCheckpoint(ctx, processID)
	return cp
}

// deleteCheckpoint removes a checkpoint if a checkpoint store is configured, best-effort.
func (e *Engine) deleteCheckpoint(ctx context.Context, processID string) {
	if e.cpStore != nil {
		_ = e.cpStore.DeleteCheckpoint(ctx, processID)
	}
}

// appendTrackStep records an execution event in the process history, best-effort.
// taskType is the domain.TaskType string of the task at fromTaskID.
func (e *Engine) appendTrackStep(ctx context.Context, processID, fromTaskID, toTaskID, taskType string, status domain.TrackStatus, reasoning string) {
	if e.trackStore == nil {
		return
	}
	step := &domain.ProcessTrackStep{
		ID:         domain.NewUUID(),
		ProcessID:  processID,
		FromTaskID: fromTaskID,
		ToTaskID:   toTaskID,
		TaskType:   taskType,
		Status:     status,
		Reasoning:  reasoning,
		CreatedAt:  time.Now().UTC(),
	}
	if err := e.trackStore.AppendTrackStep(ctx, step); err != nil {
		e.logger.Warn("failed to append track step", "process_id", processID, "error", err)
	}
}

// updateWaitingHumanStep promotes the most recent WAITING_HUMAN step for
// fromTaskID to ROUTED, filling in toTaskID and reasoning. This collapses the
// two-record WAITING_HUMAN → ROUTED sequence into a single updated record so
// only the currently-active human task has a step with an empty ToTaskID.
func (e *Engine) updateWaitingHumanStep(ctx context.Context, processID, fromTaskID, toTaskID, reasoning string) {
	if e.trackStore == nil {
		return
	}
	if err := e.trackStore.UpdateWaitingHumanStep(ctx, processID, fromTaskID, toTaskID, reasoning); err != nil {
		e.logger.Warn("failed to update WAITING_HUMAN track step", "process_id", processID, "from_task", fromTaskID, "error", err)
	}
}

// SubscribeSSE registers a buffered channel that will receive every SSEEvent
// emitted for processID until the process terminates or the caller calls
// unsubscribeSSE. The caller must drain the channel; events are dropped (not
// blocked) if the channel buffer is full.
func (e *Engine) SubscribeSSE(processID string) chan SSEEvent {
	ch := make(chan SSEEvent, 64)
	e.mu.Lock()
	e.sseBus[processID] = append(e.sseBus[processID], ch)
	e.mu.Unlock()
	return ch
}

// unsubscribeSSE removes ch from the SSE bus for processID.
// The channel is intentionally NOT closed here: emitSSE may have already
// taken a snapshot that includes ch and could be mid-send when this runs.
// Closing a channel while another goroutine is trying to send to it causes
// a panic. Instead we rely on garbage collection to reclaim the channel once
// no references remain. Handlers exit via terminal events or r.Context().Done().
func (e *Engine) unsubscribeSSE(processID string, ch chan SSEEvent) {
	e.mu.Lock()
	subs := e.sseBus[processID]
	for i, s := range subs {
		if s == ch {
			e.sseBus[processID] = append(subs[:i], subs[i+1:]...)
			break
		}
	}
	if len(e.sseBus[processID]) == 0 {
		delete(e.sseBus, processID)
	}
	e.mu.Unlock()
}

// UnsubscribeSSE is the public counterpart of unsubscribeSSE for use by handlers.
func (e *Engine) UnsubscribeSSE(processID string, ch chan SSEEvent) {
	e.unsubscribeSSE(processID, ch)
}

// StartProcessWithStream subscribes to SSE events for the new process,
// starts the process, and returns the processID together with the event
// channel. The subscription is established under the engine lock before the
// goroutine is launched so no events are missed. Caller must call
// UnsubscribeSSE when done.
func (e *Engine) StartProcessWithStream(ctx context.Context, definitionID string, initialCtx map[string]any) (string, chan SSEEvent, error) {
	def, defVersion, err := e.getDefinitionWithRetry(ctx, definitionID)
	if err != nil {
		return "", nil, fmt.Errorf("get definition: %w", err)
	}
	if _, ok := def.Tasks[def.StartTaskID]; !ok {
		return "", nil, fmt.Errorf("start task %q not found in definition %q", def.StartTaskID, def.ID)
	}

	processID := domain.NewUUID()
	now := time.Now().UTC()
	if initialCtx == nil {
		initialCtx = make(map[string]any)
	}

	// Subscribe before launching so the goroutine's first emitSSE is not lost.
	ch := e.SubscribeSSE(processID)

	state := &domain.ExecutionState{
		ProcessID:            processID,
		DefinitionID:         definitionID,
		DefinitionVersion:    defVersion,
		CurrentTaskID:        def.StartTaskID,
		Status:               domain.StatusRunning,
		GlobalContext:        initialCtx,
		TaskOutputs:          make([]map[string]any, 0),
		CreatedAt:            now,
		UpdatedAt:            now,
		ChronologicalCounter: 1,
	}
	if err := e.repo.SaveState(ctx, state); err != nil {
		e.UnsubscribeSSE(processID, ch)
		return "", nil, fmt.Errorf("save initial state: %w", err)
	}

	go e.runLoop(context.Background(), processID, "")

	e.emitSSE(processID, SSEEvent{
		Type:      SSEEventStarted,
		ProcessID: processID,
		Data:      map[string]any{"definition_id": definitionID},
	})
	e.logger.Info("process started (stream)", "process_id", processID, "definition_id", definitionID)
	return processID, ch, nil
}

// emitSSE broadcasts evt to all current subscribers of processID.
// Non-blocking: slow or disconnected subscribers are skipped.
func (e *Engine) emitSSE(processID string, evt SSEEvent) {
	e.mu.RLock()
	subs := e.sseBus[processID]
	// Snapshot so we don't hold the lock while sending.
	snapshot := make([]chan SSEEvent, len(subs))
	copy(snapshot, subs)
	e.mu.RUnlock()

	for _, ch := range snapshot {
		select {
		case ch <- evt:
		default:
			// subscriber too slow — drop rather than block the engine
		}
	}
}

// ListTrackSteps returns the ordered execution history for a process.
func (e *Engine) ListTrackSteps(ctx context.Context, processID string) ([]*domain.ProcessTrackStep, error) {
	if e.trackStore == nil {
		return nil, fmt.Errorf("track store not configured")
	}
	return e.trackStore.ListTrackSteps(ctx, processID)
}

// RestoreProcesses re-enters the execution loop for every process that was
// RUNNING or PAUSED when the engine last shut down. RUNNING processes resume
// execution from their current task; PAUSED processes block on their HUMAN
// task channel and wait for POST /resume. Call once at startup after
// definitions are loaded.
func (e *Engine) RestoreProcesses(ctx context.Context) error {
	states, err := e.repo.ListStates(ctx)
	if err != nil {
		return fmt.Errorf("list states for restore: %w", err)
	}
	var count int
	for _, st := range states {
		// If the process was RUNNING or PAUSED when we shut down, resume it.
		if st.Status == domain.StatusPaused || st.Status == domain.StatusRunning {
			// Verify the definition can still be resolved (incl. pinned design version).
			if _, err := e.definitionForState(ctx, st); err != nil {
				e.logger.Warn("skipping process — definition not found",
					"process_id", st.ProcessID, "definition_id", st.DefinitionID,
					"status", st.Status)
				continue
			}
			go e.runLoop(context.Background(), st.ProcessID, "")
			count++
			e.logger.Info("restored process", "process_id", st.ProcessID,
				"definition_id", st.DefinitionID, "task_id", st.CurrentTaskID,
				"status", st.Status)
			continue
		}

		// If the process ended in FAILED and the failure looks like an LLM
		// network/timeout error, attempt a one-shot restart now that the
		// container has been restarted and dependencies may be healthy.
		if st.Status == domain.StatusFailed && !st.RestartedOnBoot && e.isRecoverableLLMFailure(st) {
			// Verify the definition still exists (allow a few short retries
			// to accommodate slow mounts or delayed registration).
			if _, _, err := e.getDefinitionWithRetry(ctx, st.DefinitionID); err != nil {
				e.logger.Warn("skipping failed process restart — definition not found",
					"process_id", st.ProcessID, "definition_id", st.DefinitionID, "error", err)
				continue
			}
			// Mark as running and clear the transient error before resuming.
			st.Status = domain.StatusRunning
			st.Error = ""
			st.RestartedOnBoot = true
			st.UpdatedAt = time.Now().UTC()
			if err := e.repo.SaveState(ctx, st); err != nil {
				e.logger.Warn("failed to update state before restarting failed process",
					"process_id", st.ProcessID, "error", err)
				continue
			}
			go e.runLoop(context.Background(), st.ProcessID, "")
			count++
			e.logger.Info("restarted failed process after node restart",
				"process_id", st.ProcessID, "definition_id", st.DefinitionID,
				"task_id", st.CurrentTaskID)
			continue
		}
	}
	if count > 0 {
		e.logger.Info("restore complete", "restored_processes", count)
	}
	return nil
}

// StartReaper runs a background watchdog that fails any RUNNING process whose
// execution goroutine is no longer alive (orphaned by a crashed loop, a lost
// LLM call, or a restart that could not restore it). Call once at startup,
// after RestoreProcesses.
func (e *Engine) StartReaper(interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			e.reapOrphans(context.Background())
		}
	}()
	e.logger.Info("orphan reaper started", "interval", interval.String())
}

func (e *Engine) reapOrphans(ctx context.Context) {
	states, err := e.repo.ListStates(ctx)
	if err != nil {
		e.logger.Warn("reaper: list states failed", "error", err)
		return
	}
	for _, st := range states {
		if st.Status != domain.StatusRunning {
			continue
		}
		e.liveMu.Lock()
		_, live := e.live[st.ProcessID]
		e.liveMu.Unlock()
		if live {
			continue
		}
		e.logger.Warn("reaping orphaned RUNNING process",
			"process_id", st.ProcessID, "task_id", st.CurrentTaskID)
		e.failProcess(ctx, st, fmt.Errorf("orphaned RUNNING process: no active execution loop (cleaned up by reaper)"))
	}
}

// ListLLMLogs returns all LLM call logs for a given process instance.
func (e *Engine) ListLLMLogs(ctx context.Context, processID string) ([]*domain.LLMLog, error) {
	if e.logStore == nil {
		return nil, fmt.Errorf("log store not configured")
	}
	return e.logStore.ListLLMLogs(ctx, processID)
}

// ListDefinitionsLive returns the definitions as they execute: an active
// versioned design (process_designs) replaces the file-registered
// definition for its process, matching getDefinitionWithRetry.
func (e *Engine) ListDefinitionsLive(ctx context.Context) ([]*domain.ProcessDefinition, error) {
	defs, err := e.repo.ListDefinitions(ctx)
	if err != nil {
		return nil, err
	}
	if e.designs != nil {
		for _, def := range defs {
			d, _, derr := e.designs.GetActiveDesign(ctx, def.ID)
			if derr != nil || d == nil {
				continue
			}
			if ldef, perr := repository.DesignOf(d); perr == nil {
				defs = replaceDefinition(defs, ldef)
			}
		}
	}
	return defs, nil
}

func replaceDefinition(defs []*domain.ProcessDefinition, def *domain.ProcessDefinition) []*domain.ProcessDefinition {
	for i, d := range defs {
		if d.ID == def.ID {
			defs[i] = def
			return defs
		}
	}
	return append(defs, def)
}

// ListTaskHistories returns persisted task history rows for a given process.
func (e *Engine) ListTaskHistories(ctx context.Context, processID string) ([]*domain.TaskHistory, error) {
	return e.repo.ListTaskHistories(ctx, processID)
}

// SetDesignStore wires the versioned design store. When absent the engine
// falls back to file-registered definitions only.
func (e *Engine) SetDesignStore(ds repository.DesignStore) {
	e.designs = ds
}

// definitionForState resolves the definition a running instance executes on:
// the versioned design pinned at start (ExecutionState.DefinitionVersion),
// falling back to the (re)registered definition. Running instances keep
// their design even after a newer version is deployed.
func (e *Engine) definitionForState(ctx context.Context, st *domain.ExecutionState) (*domain.ProcessDefinition, error) {
	if st != nil && st.DefinitionVersion > 0 && e.designs != nil {
		if dv, err := e.designs.GetDesign(ctx, st.DefinitionID, st.DefinitionVersion); err == nil && dv != nil {
			if def, perr := repository.DesignOf(dv.Design); perr == nil {
				return def, nil
			}
		}
	}
	if st == nil {
		return nil, fmt.Errorf("definitionForState: no state")
	}
	return e.repo.GetDefinition(ctx, st.DefinitionID)
}

// getDefinitionWithRetry attempts to fetch a process definition from the
// repository with a small number of retries and exponential backoff. This
// helps in environments where the definition registration may lag slightly
// behind service startup (for example when mounts or DB writes are delayed).
func (e *Engine) getDefinitionWithRetry(ctx context.Context, defID string) (*domain.ProcessDefinition, int, error) {
	// An active versioned design (process_designs) wins over the
	// file-registered definition; the version is pinned on the new instance.
	if e.designs != nil {
		if d, ver, err := e.designs.GetActiveDesign(ctx, defID); err == nil && d != nil {
			if def, perr := repository.DesignOf(d); perr == nil {
				return def, ver, nil
			}
		}
	}
	var (
		def *domain.ProcessDefinition
		err error
	)
	backoff := 100 * time.Millisecond
	maxAttempts := 10
	for i := 0; i < maxAttempts; i++ {
		def, err = e.repo.GetDefinition(ctx, defID)
		if err == nil {
			return def, 0, nil
		}

		// If context cancelled/expired, return immediately.
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		default:
		}

		// On early attempts try to auto-register the definition from disk
		// when a matching YAML file exists under the configured processes
		// directory. This helps when definitions are present on the container
		// filesystem but haven't been registered in the store yet.
		if i == 0 || i == 2 {
			// Determine processes directory (follow same logic as cmd/main.go).
			procDir := os.Getenv("PROCESSES_DIR")
			if procDir == "" {
				if info, err := os.Stat("/processes"); err == nil && info.IsDir() {
					procDir = "/processes"
				} else {
					procDir = "workflows"
				}
			}
			// Look for common filename candidates.
			candidates := []string{
				filepath.Join(procDir, defID+".yaml"),
				filepath.Join(procDir, defID+".yml"),
			}
			for _, cand := range candidates {
				if _, statErr := os.Stat(cand); statErr == nil {
					if defFile, parseErr := domain.ParseYAMLFile(cand); parseErr == nil {
						// Never auto-register a designer-managed process (app
						// section) that has no deployed design version — undeployed
						// projects stay out of the engine.
						skip := false
						if e.designs != nil && hasAppSection(cand) {
							if active, _, aerr := e.designs.GetActiveDesign(ctx, defID); aerr != nil || active == nil {
								e.logger.Info("auto-register skipped: process not deployed yet", "definition_id", defID, "file", cand)
								skip = true
							}
						}
						if !skip {
							if regErr := e.repo.RegisterDefinition(ctx, defFile); regErr == nil {
								e.logger.Info("auto-registered definition from disk", "definition_id", defID, "file", cand)
								// Try to fetch again immediately.
								def, err = e.repo.GetDefinition(ctx, defID)
								if err == nil {
									return def, 0, nil
								}
							} else {
								e.logger.Info("failed to register definition from disk", "definition_id", defID, "file", cand, "error", regErr)
							}
						}
					} else {
						e.logger.Info("failed to parse definition file", "file", cand, "error", parseErr)
					}
				}
			}
		}

		e.logger.Info("definition not found, retrying", "definition_id", defID, "attempt", i+1, "error", err)
		time.Sleep(backoff)
		if backoff < 2*time.Second {
			backoff *= 2
		}
	}
	return nil, 0, err
}


// hasAppSection reports whether the YAML file on disk belongs to a
// designer-managed process (it carries an "app:" section). Such a process
// is a designer PROJECT: the engine must not serve it until the user
// confirms a deployment in the design versions tab. (Mirrors
// processconfig.ParseAppFile; inlined here to avoid an import cycle.)
func hasAppSection(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var raw struct {
		App       any    `yaml:"app"`
		ProcessID string `yaml:"process_id"`
	}
	if yaml.Unmarshal(data, &raw) != nil {
		return false
	}
	return raw.App != nil && raw.ProcessID != ""
}

// isRecoverableLLMFailure returns true when the process failure appears to be
// caused by an external LLM/network timeout or routing error. This heuristic
// is intentionally conservative to avoid automatically restarting unrelated
// failures (e.g. business logic or SYSTEM task errors).
func (e *Engine) isRecoverableLLMFailure(st *domain.ExecutionState) bool {
	if st == nil || st.Status != domain.StatusFailed || st.Error == "" {
		return false
	}
	s := strings.ToLower(st.Error)
	keywords := []string{"input preparation", "routing after", "llm", "openai", "context deadline exceeded", "timeout"}
	for _, k := range keywords {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// StartProcess creates a new process instance from a definition and begins execution.
func (e *Engine) StartProcess(ctx context.Context, definitionID string, initialCtx map[string]any) (string, error) {
	def, defVersion, err := e.getDefinitionWithRetry(ctx, definitionID)
	if err != nil {
		return "", fmt.Errorf("get definition: %w", err)
	}
	if _, ok := def.Tasks[def.StartTaskID]; !ok {
		return "", fmt.Errorf("start task %q not found in definition %q", def.StartTaskID, def.ID)
	}

	processID := domain.NewUUID()
	now := time.Now().UTC()
	if initialCtx == nil {
		initialCtx = make(map[string]any)
	}

	state := &domain.ExecutionState{
		ProcessID:            processID,
		DefinitionID:         definitionID,
		DefinitionVersion:    defVersion,
		CurrentTaskID:        def.StartTaskID,
		Status:               domain.StatusRunning,
		GlobalContext:        initialCtx,
		TaskOutputs:          make([]map[string]any, 0),
		CreatedAt:            now,
		UpdatedAt:            now,
		ChronologicalCounter: 1,
	}
	if err := e.repo.SaveState(ctx, state); err != nil {
		return "", fmt.Errorf("save initial state: %w", err)
	}

	go e.runLoop(context.Background(), processID, "")

	e.emitSSE(processID, SSEEvent{
		Type:      SSEEventStarted,
		ProcessID: processID,
		Data:      map[string]any{"definition_id": definitionID},
	})
	e.logger.Info("process started", "process_id", processID, "definition_id", definitionID)
	return processID, nil
}

// GetStatus returns the current execution state of a process.
func (e *Engine) GetStatus(ctx context.Context, processID string) (*domain.ExecutionState, error) {
	return e.repo.GetState(ctx, processID)
}

// SetServiceTools configures the data-service tools the LLM can call while
// answering /ask questions (DMS doc service, operational data index).
func (e *Engine) SetServiceTools(tools []ServiceTool) {
	e.serviceTools = tools
}

// AskQuestion answers a user question about a process instance, grounded in
// the instance's live state (global context, task outputs, execution track)
// plus any client-side context (active task, selection) provided by the UI.
// token is the caller's Authorization header, forwarded to the data services.
// AgentChat runs a multi-message LLM conversation for the user-facing AI
// assist agent loop (POST /agent/chat). jsonMode forces a JSON object
// response (the tool protocol).
func (e *Engine) AgentChat(ctx context.Context, messages []ai.ChatMessage, jsonMode bool) (string, error) {
	return e.llm.Chat(ctx, messages, jsonMode)
}

// TokenUsage totals the LLM token counters for one instance.
func (e *Engine) TokenUsage(ctx context.Context, processID string) (prompt, completion, calls int64, err error) {
	return e.logStore.SumTokenUsage(ctx, processID)
}

// TokenUsageByDefinition totals LLM tokens across every instance of a
// definition (the designer view).
func (e *Engine) TokenUsageByDefinition(ctx context.Context, definitionID string) (prompt, completion, calls int64, err error) {
	return e.logStore.SumTokenUsageByDefinition(ctx, definitionID)
}

// TokenUsageByUsernameMonth totals a user's LLM tokens since monthStart
// (the normal-user monthly quota check).
func (e *Engine) TokenUsageByUsernameMonth(ctx context.Context, username string, monthStart time.Time) (prompt, completion, calls int64, err error) {
	if e.logStore == nil {
		return 0, 0, 0, nil
	}
	return e.logStore.SumTokenUsageByUsernameMonth(ctx, username, monthStart)
}

func (e *Engine) AskQuestion(ctx context.Context, processID, question, clientContext, lang, token string) (string, error) {
	state, err := e.repo.GetState(ctx, processID)
	if err != nil {
		return "", fmt.Errorf("ask: load state: %w", err)
	}
	if lang == "" {
		lang = extractLang(state.GlobalContext)
	}

	compGlobal, compOutputs := e.compactForLLM(state.GlobalContext, state.TaskOutputs)
	globalJSON, _ := json.Marshal(compGlobal)
	outputsJSON, _ := json.Marshal(compOutputs)

	var track string
	if steps, err := e.ListTrackSteps(ctx, processID); err == nil && len(steps) > 0 {
		var b strings.Builder
		for i, s := range steps {
			b.WriteString(fmt.Sprintf("%d. %s → %s [%s]%s\n",
				i+1, s.FromTaskID, orDash(s.ToTaskID), s.Status,
				reasoningNote(s.Reasoning)))
		}
		track = b.String()
	}

	system := fmt.Sprintf(
		"You are OUMMA, the AI assistant of an agentic business workspace. "+
			"You answer questions about the user's business process instances. "+
			"Use ONLY the provided instance context; if it is insufficient, say so briefly. "+
			"Be concise and factual, plain text. Always answer in: %s.", lang)
	if len(e.serviceTools) > 0 {
		system += e.serviceTools.ToolPromptSection()
	}

	var user strings.Builder
	fmt.Fprintf(&user, "Instance: %s\nDefinition: %s\nStatus: %s\nCurrent task: %s\n", orDash(state.ProcessID), orDash(state.DefinitionID), state.Status, orDash(state.CurrentTaskID))
	if state.Error != "" {
		fmt.Fprintf(&user, "Engine error: %s\n", state.Error)
	}
	fmt.Fprintf(&user, "Global context: %s\nTask outputs: %s\n", string(globalJSON), string(outputsJSON))
	if track != "" {
		fmt.Fprintf(&user, "Execution track:\n%s", track)
	}
	if clientContext != "" {
		fmt.Fprintf(&user, "\nUI context (active selection etc.):\n%s\n", clientContext)
	}
	fmt.Fprintf(&user, "\nQuestion: %s", question)

	// Agentic loop: let the LLM call data-service tools before answering.
	for round := 0; ; round++ {
		answer, err := e.llm.Ask(ctx, &ai.AskRequest{System: system, User: user.String()})
		if err != nil {
			return "", err
		}
		tc, ok := ExtractToolCall(answer)
		if !ok || round >= 3 {
			return strings.TrimSpace(answer), nil
		}
		result, err := e.serviceTools.Execute(ctx, tc.Name, token, tc.Args)
		if err != nil {
			result = "TOOL ERROR: " + err.Error()
		}
		fmt.Fprintf(&user, "\nTool call %s(%s) returned:\n%s\nNow answer the original question.", tc.Name, mustJSON(tc.Args), result)
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func reasoningNote(reasoning string) string {
	if reasoning == "" {
		return ""
	}
	return " (" + reasoning + ")"
}

// PendingTask describes a HUMAN task awaiting input.
type PendingTask struct {
	ProcessID    string         `json:"process_id"`
	DefinitionID string         `json:"definition_id"`
	TaskID       string         `json:"task_id"`
	Description  string         `json:"description"`
	Group        string         `json:"group"`
	Departments  []string       `json:"departments"`
	GlobalCtx    map[string]any `json:"global_context,omitempty"`
	PausedSince  string         `json:"paused_since"`
}

func hasOverlap(a, b []string) bool {
	set := make(map[string]struct{}, len(a))
	for _, v := range a {
		set[v] = struct{}{}
	}
	for _, v := range b {
		if _, ok := set[v]; ok {
			return true
		}
	}
	return false
}

// ErrAccessDenied is wrapped by engine operations when the authenticated
// caller is not allowed to perform them (group or department mismatch).
// Handlers map it to HTTP 403.
var ErrAccessDenied = errors.New("access denied")

// InstanceDepartments returns the department codes stamped on an instance.
// Single source of truth for every department visibility check. Accepted
// shapes, in priority order:
//
//	global_context["department"] = {"codes": ["10"]}   (app/Flutter wire shape)
//	global_context["department"] = ["10"]
//	global_context["departments"] = ["10"]              (legacy plural)
func InstanceDepartments(st *domain.ExecutionState) []string {
	if st == nil || st.GlobalContext == nil {
		return nil
	}
	if raw, ok := st.GlobalContext["department"]; ok {
		if m, ok := raw.(map[string]any); ok {
			if s := toStringSlice(m["codes"]); len(s) > 0 {
				return s
			}
		}
		if s := toStringSlice(raw); len(s) > 0 {
			return s
		}
	}
	return toStringSlice(st.GlobalContext["departments"])
}

func toStringSlice(v any) []string {
	switch arr := v.(type) {
	case []string:
		return arr
	case []any:
		out := make([]string, 0, len(arr))
		for _, item := range arr {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// validateTaskCaller enforces that the authenticated caller may complete the
// requested task: the task's group (when set) must be one of the caller's
// groups, and the caller's departments must overlap the instance's
// departments (when both are present). No claims (internal caller) → allow.
func (e *Engine) validateTaskCaller(ctx context.Context, state *domain.ExecutionState, taskName string, actorGroups any, actorDepartments []string) error {
	var gs []string
	switch g := actorGroups.(type) {
	case nil:
	case []string:
		gs = g
	case []any:
		for _, v := range g {
			if s, ok := v.(string); ok {
				gs = append(gs, s)
			}
		}
	default:
		return fmt.Errorf("%w: unsupported actor groups type", ErrAccessDenied)
	}
	if len(gs) == 0 && len(actorDepartments) == 0 {
		return nil // no claims — internal/service caller
	}
	def, err := e.definitionForState(ctx, state)
	if err != nil {
		return fmt.Errorf("resolve definition: %w", err)
	}
	if task, ok := def.Tasks[taskName]; ok && task.Group != "" {
		if !sliceContainsStr(gs, task.Group) {
			return fmt.Errorf("%w: task %q requires group %q", ErrAccessDenied, taskName, task.Group)
		}
	}
	instDepts := InstanceDepartments(state)
	if len(instDepts) > 0 && len(actorDepartments) > 0 && !hasOverlap(instDepts, actorDepartments) {
		return fmt.Errorf("%w: instance departments %v do not overlap caller departments %v", ErrAccessDenied, instDepts, actorDepartments)
	}
	return nil
}

func sliceContainsStr(s []string, target string) bool {
	for _, v := range s {
		if v == target {
			return true
		}
	}
	return false
}

// extractLang reads the "lang" key from the process global context and returns
// its value as a string. Returns "en" when absent or not a string.
func extractLang(globalCtx map[string]any) string {
	if v, ok := globalCtx["lang"]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return "en"
}

// getEnvInt reads an integer env var or returns the fallback when absent or invalid.
func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// compactValue returns a value suitable for LLM prompts by serializing
// complex structures and omitting large blobs. Small scalars and small
// objects are preserved; large objects are replaced with a short note.
func compactValue(v any, maxField int) any {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if len(t) <= maxField {
			return t
		}
		return fmt.Sprintf("<omitted %d bytes>", len(t))
	case bool, float64, float32, int, int64, int32:
		return t
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return "<unserializable>"
		}
		if len(b) <= maxField {
			var out any
			_ = json.Unmarshal(b, &out)
			return out
		}
		return fmt.Sprintf("<omitted %d bytes>", len(b))
	}
}

// compactTaskEntry makes a compact representation of a task output entry
// keeping identifying fields and small payloads; large fields are omitted.
func compactTaskEntry(entry map[string]any, maxField int) map[string]any {
	out := make(map[string]any)
	// Always include identifiers when available.
	if v, ok := entry["task_id"]; ok {
		out["task_id"] = v
	}
	if v, ok := entry["task_instance_id"]; ok {
		out["task_instance_id"] = v
	}
	if v, ok := entry["state"]; ok {
		out["state"] = v
	}
	if v, ok := entry["chronological_order"]; ok {
		out["chronological_order"] = v
	}

	var omitted []string
	for k, v := range entry {
		if k == "task_id" || k == "task_instance_id" || k == "state" || k == "chronological_order" {
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			omitted = append(omitted, k)
			continue
		}
		if len(b) <= maxField {
			var dv any
			_ = json.Unmarshal(b, &dv)
			out[k] = dv
		} else {
			omitted = append(omitted, k)
		}
	}
	if len(omitted) > 0 {
		out["_omitted_fields"] = omitted
	}
	return out
}

// compactForLLM returns a compacted copy of globalCtx and a short slice of
// task outputs suitable for inclusion in LLM prompts. Behavior is governed by
// env vars `LLM_MAX_TASK_HISTORY` (default 2) and `LLM_MAX_FIELD_SIZE` (default 1024).
func (e *Engine) compactForLLM(globalCtx map[string]any, taskOutputs []map[string]any) (map[string]any, []map[string]any) {
	maxHistory := getEnvInt("LLM_MAX_TASK_HISTORY", 2)
	maxField := getEnvInt("LLM_MAX_FIELD_SIZE", 1024)

	// Compact global context.
	g := make(map[string]any)
	for k, v := range globalCtx {
		g[k] = compactValue(v, maxField)
	}

	// Select last N task outputs and compact each entry.
	start := 0
	if len(taskOutputs) > maxHistory {
		start = len(taskOutputs) - maxHistory
	}
	outs := make([]map[string]any, 0, len(taskOutputs)-start)
	for _, e := range taskOutputs[start:] {
		outs = append(outs, compactTaskEntry(e, maxField))
	}
	return g, outs
}

// compactAny uses the configured max field size to compact an arbitrary value.
func (e *Engine) compactAny(v any) any {
	maxField := getEnvInt("LLM_MAX_FIELD_SIZE", 1024)
	return compactValue(v, maxField)
}

// compactForLLMWithParams is like compactForLLM but accepts explicit
// parameters for testing or aggressive fallbacks.
func (e *Engine) compactForLLMWithParams(globalCtx map[string]any, taskOutputs []map[string]any, maxHistory, maxField int) (map[string]any, []map[string]any) {
	g := make(map[string]any)
	for k, v := range globalCtx {
		g[k] = compactValue(v, maxField)
	}

	start := 0
	if len(taskOutputs) > maxHistory {
		start = len(taskOutputs) - maxHistory
	}
	outs := make([]map[string]any, 0, len(taskOutputs)-start)
	for _, e := range taskOutputs[start:] {
		outs = append(outs, compactTaskEntry(e, maxField))
	}
	return g, outs
}

// compactForLLMAggressive returns a very small compacted view used for
// token-limit fallbacks. Values are configurable via env vars
// `LLM_AGGRESSIVE_MAX_TASK_HISTORY` and `LLM_AGGRESSIVE_MAX_FIELD_SIZE`.
func (e *Engine) compactForLLMAggressive(globalCtx map[string]any, taskOutputs []map[string]any) (map[string]any, []map[string]any) {
	maxHistory := getEnvInt("LLM_AGGRESSIVE_MAX_TASK_HISTORY", 0)
	maxField := getEnvInt("LLM_AGGRESSIVE_MAX_FIELD_SIZE", 256)
	return e.compactForLLMWithParams(globalCtx, taskOutputs, maxHistory, maxField)
}

// compactAnyWithMaxField compacts a value using an explicit max field size.
func (e *Engine) compactAnyWithMaxField(v any, maxField int) any {
	return compactValue(v, maxField)
}

// isTokenLimitError returns true when the provided error appears to be an
// LLM model / API rejection due to prompt size or context length.
func isTokenLimitError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	checks := []string{
		"maximum context length",
		"input_tokens",
		"please reduce the length of the input prompt",
		"prompt contains at least",
		"context length",
		"input tokens",
		"tokens",
	}
	for _, c := range checks {
		if strings.Contains(s, c) {
			return true
		}
	}
	return false
}

// parseDirectTaskOverride inspects the override directive for an explicit
// task-jump instruction such as "forward directly to <task_id>" or
// "jump to <task_id>". When found, the target task ID is returned and the
// engine bypasses LLM routing entirely, performing a deterministic jump.
//
// Matching is case-insensitive. Directional anchors are checked in order of
// specificity (longest first) and the longest matching task ID wins when
// multiple candidates share a common prefix.
// Returns ("", false) when no recognisable directive is present.
func parseDirectTaskOverride(override, currentTaskID string, tasks map[string]*domain.Task) (string, bool) {
	if override == "" {
		return "", false
	}
	lower := strings.ToLower(override)

	// Anchors ordered by specificity — most specific first so that
	// "forward directly to" is preferred over the bare " to " anchor.
	anchors := []string{
		"forward directly to ",
		"directly to ",
		"forward to ",
		"jump to ",
		"skip to ",
		"go to ",
		"route to ",
		" to ",
	}

	for _, anchor := range anchors {
		idx := strings.Index(lower, anchor)
		if idx < 0 {
			continue
		}
		tail := strings.TrimSpace(lower[idx+len(anchor):])

		// Find the longest task ID that is a prefix of tail (case-insensitive).
		// Longest-wins avoids false matches when one task ID is a prefix of another.
		var match string
		for taskID := range tasks {
			if taskID == currentTaskID {
				continue
			}
			lowerID := strings.ToLower(taskID)
			if strings.HasPrefix(tail, lowerID) && len(taskID) > len(match) {
				match = taskID
			}
		}
		if match != "" {
			return match, true
		}
	}
	return "", false
}

// runLoop is the core execution loop that runs in its own goroutine.
// userOverride carries a contextual override directive for the next routing decision.
// On startup-restore the loop loads any persisted checkpoint and skips phases that
// already completed, avoiding duplicate LLM calls and duplicate HTTP side-effects.
func (e *Engine) runLoop(ctx context.Context, processID string, userOverride string) {
	ctx = ai.ContextWithProcessID(ctx, processID)
	log := e.logger.With("process_id", processID)
	e.liveMu.Lock()
	e.live[processID] = struct{}{}
	e.liveMu.Unlock()
	defer func() {
		e.liveMu.Lock()
		delete(e.live, processID)
		e.liveMu.Unlock()
	}()

outerLoop:
	for {
		state, err := e.repo.GetState(ctx, processID)
		if err != nil {
			log.Error("failed to load state", "error", err)
			return
		}

		// Attribute every LLM call of this instance to its owner (stamped at
		// launch), department and process — feeds the usage counters and the
		// monthly cost rollup.
		if !ai.HasUsage(ctx) {
			var dept string
			if depts := InstanceDepartments(state); len(depts) > 0 {
				dept = depts[0]
			}
			owner, _ := state.GlobalContext["owner_id"].(string)
			ctx = ai.ContextWithUsage(ctx, processID, ai.UsageInfo{
				Username:   owner,
				Department: dept,
				Definition: state.DefinitionID,
			})
		}

		def, err := e.definitionForState(ctx, state)
		if err != nil {
			log.Error("failed to load definition", "error", err)
			e.failProcess(ctx, state, err)
			return
		}

		task, ok := def.Tasks[state.CurrentTaskID]
		if !ok {
			err := fmt.Errorf("task %q not found in definition %q", state.CurrentTaskID, state.DefinitionID)
			log.Error("invalid task reference", "error", err)
			e.failProcess(ctx, state, err)
			return
		}

		log.Info("executing task", "task_id", task.ID, "task_type", task.Type)
		e.emitSSE(processID, SSEEvent{
			Type:      SSEEventTaskEntered,
			ProcessID: processID,
			TaskID:    task.ID,
			Data:      map[string]any{"task_type": string(task.Type)},
		})

		// --- END task: finalize and exit ---
		if task.Type == domain.TaskTypeEnd {
			executor := e.executors[domain.TaskTypeEnd]
			output, _ := executor.Execute(ctx, task, state.GlobalContext)
			var outputMap map[string]any
			if output != nil {
				outputMap = output
			} else {
				outputMap = make(map[string]any)
			}
			instanceID := state.ActiveTaskInstanceID
			if instanceID == "" {
				instanceID = domain.NewUUID()
			}
			setTaskEntry(state, task.ID, instanceID, outputMap)
			// Ensure the task entry carries the task's configured group when available.
			ensureTaskEntryHasGroup(state, def, task.ID)
			// Persist skeleton / completed entry to task_histories
			if err := e.persistTaskHistory(ctx, state, def, task.ID); err != nil {
				e.logger.Error("persist task history failed", "error", err, "task_id", task.ID)
			}
			state.CurrentTaskID = task.ID
			state.LastCompletedTaskID = task.ID
			state.Status = domain.StatusCompleted
			state.UpdatedAt = time.Now().UTC()
			if err := e.repo.SaveState(ctx, state); err != nil {
				log.Error("failed to persist completed state", "error", err)
			}
			e.deleteCheckpoint(ctx, processID)
			log.Info("process completed")
			e.emitSSE(processID, SSEEvent{
				Type:      SSEEventCompleted,
				ProcessID: processID,
				TaskID:    task.ID,
			})
			return
		}

		// --- Load checkpoint for this task ---
		cp := e.getCheckpoint(ctx, processID)
		if cp != nil && cp.TaskID != task.ID {
			cp = nil // stale checkpoint for a different task, ignore
		}

		// cpExists tracks whether a valid checkpoint was present (or is saved) for
		// the current task. When true, LLM failures trigger a retry rather than an
		// immediate process failure — the process state is safe and we just wait for
		// the LLM to recover.
		cpExists := cp != nil

		// Determine which phases to skip based on checkpoint.
		var skipInput, skipExec, skipRoute bool
		if cp != nil {
			switch cp.Phase {
			case domain.PhaseRouted:
				skipInput = true
				skipExec = true
				skipRoute = true
			case domain.PhaseTaskDone:
				skipInput = true
				skipExec = true
			case domain.PhaseInputReady, domain.PhaseWaitingHuman:
				skipInput = true
			}
			// Restore override from checkpoint (survives crashes).
			if cp.Override != "" {
				userOverride = cp.Override
			}
		}

		// --- Phase 1: Prepare input (skip for START, skip if checkpointed) ---
		var prevOutput any
		if state.LastCompletedTaskID != "" {
			if m, ok := findTaskOutput(state.TaskOutputs, state.LastCompletedTaskID); ok {
				// expose the full map as previous output
				prevOutput = m
			}
		}

		var preparedInput map[string]any
		// MCP-like tool task: inputs resolve deterministically from the
		// process context (tool parameter <- context field key) — NO LLM
		// call, zero tokens. Execution happens in the system executor.
		isToolTask := task.Type == domain.TaskTypeSystem && task.Config != nil && task.Config.Tool != ""
		if skipInput && cp.PreparedInput != nil {
			preparedInput = cp.PreparedInput
			log.Info("restored prepared input from checkpoint", "task_id", task.ID)
		} else if isToolTask {
			preparedInput = map[string]any{}
			for param, ctxKey := range task.Config.ToolInputs {
				ctxKey = strings.TrimSpace(ctxKey)
				if v, ok := state.GlobalContext[ctxKey]; ok {
					preparedInput[param] = v
				} else if toolInputTemplateRe.MatchString(ctxKey) {
					// Template value: interpolate {{field}} placeholders from the context.
					preparedInput[param] = toolInputTemplateRe.ReplaceAllStringFunc(ctxKey, func(m string) string {
						k := m[2 : len(m)-2]
						if v, ok := state.GlobalContext[k]; ok {
							return fmt.Sprint(v)
						}
						return ""
					})
				} else if ctxKey != "" {
					// No matching context field and no template markers: send the value as a literal.
					preparedInput[param] = ctxKey
				}
			}
			log.Info("tool task input resolved (no LLM)", "task_id", task.ID, "tool", task.Config.Tool)
			e.saveCheckpoint(ctx, &domain.ProcessCheckpoint{
				ProcessID: processID, TaskID: task.ID,
				Phase: domain.PhaseInputReady, PreparedInput: preparedInput,
				Override: userOverride,
			})
			cpExists = true
		} else if task.Type != domain.TaskTypeStart {
			// Compact global context and task outputs before sending to the LLM
			// to reduce token usage. The compacted copies are safe to send and
			// do not modify the in-memory state.
			compGlobal, compOutputs := e.compactForLLM(state.GlobalContext, state.TaskOutputs)
			prepareReq := &ai.PrepareInputRequest{
				Task:           task,
				GlobalCtx:      compGlobal,
				PrevOutput:     e.compactAny(prevOutput),
				TaskOutputs:    compOutputs,
				DefinitionID:   def.ID,
				DefinitionDesc: def.Description,
				Lang:           extractLang(state.GlobalContext),
			}
			// Apply a per-call timeout so a single slow LLM request cannot hang the engine.
			callCtx, callCancel := context.WithTimeout(ctx, defaultLLMCallTimeout)
			preparedInput, err = e.llm.PrepareInput(callCtx, prepareReq)
			callCancel()
			if err != nil {
				// If the LLM rejected the prompt due to token limits, try an
				// aggressive compaction (one-shot) before falling back to the
				// usual retry-or-fail behaviour.
				if isTokenLimitError(err) {
					log.Warn("LLM rejected prompt due to token limit — retrying with aggressive compaction", "task_id", task.ID, "error", err)
					aggrGlobal, aggrOutputs := e.compactForLLMAggressive(state.GlobalContext, state.TaskOutputs)
					aggrPrev := e.compactAnyWithMaxField(prevOutput, getEnvInt("LLM_AGGRESSIVE_MAX_FIELD_SIZE", 256))
					aggrReq := &ai.PrepareInputRequest{
						Task:           task,
						GlobalCtx:      aggrGlobal,
						PrevOutput:     aggrPrev,
						TaskOutputs:    aggrOutputs,
						DefinitionID:   def.ID,
						DefinitionDesc: def.Description,
						Lang:           extractLang(state.GlobalContext),
					}
					callCtx2, callCancel2 := context.WithTimeout(ctx, defaultLLMCallTimeout)
					preparedInput, err = e.llm.PrepareInput(callCtx2, aggrReq)
					callCancel2()
				}
				if err != nil {
					if !cpExists {
						log.Error("LLM input preparation failed (no checkpoint)", "task_id", task.ID, "error", err)
						e.failProcess(ctx, state, fmt.Errorf("input preparation for %s: %w", task.ID, err))
						return
					}
					log.Warn("LLM unreachable during input preparation — retrying (checkpoint present)", "task_id", task.ID, "error", err)
					preparedInput, err = llmRetry(ctx, log, func() (map[string]any, error) {
						callCtx, callCancel := context.WithTimeout(ctx, defaultLLMCallTimeout)
						defer callCancel()
						return e.llm.PrepareInput(callCtx, prepareReq)
					})
					if err != nil {
						log.Info("context cancelled while retrying LLM input preparation", "task_id", task.ID)
						return
					}
				}
			}
			log.Info("LLM prepared input", "task_id", task.ID)
			e.saveCheckpoint(ctx, &domain.ProcessCheckpoint{
				ProcessID: processID, TaskID: task.ID,
				Phase: domain.PhaseInputReady, PreparedInput: preparedInput,
				Override: userOverride,
			})
			cpExists = true
		} else {
			preparedInput = state.GlobalContext
		}

		// --- Phase 2: Execute task (skip if checkpointed past this phase) ---
		var output map[string]any
		currentOverride := userOverride
		userOverride = ""

		if skipExec && cp.TaskOutput != nil {
			output = cp.TaskOutput
			log.Info("restored task output from checkpoint", "task_id", task.ID)
		} else {
			switch task.Type {
			case domain.TaskTypeHuman:
				// Record as the most recent HUMAN task for SYSTEM-error recovery.
				state.LastHumanTaskID = task.ID

				// Generate a fresh task instance ID each time this task is entered.
				state.ActiveTaskInstanceID = domain.NewUUID()
				// Update the prepopulated task entry to expose the active instance id.
				setTaskEntry(state, task.ID, state.ActiveTaskInstanceID, nil)
				// Populate group info from the task definition so clients can see assigned groups.
				ensureTaskEntryHasGroup(state, def, task.ID)
				if err := e.persistTaskHistory(ctx, state, def, task.ID); err != nil {
					e.logger.Error("persist task history failed", "error", err, "task_id", task.ID)
				}

				// Persist PAUSED state and block until external input arrives.
				state.Status = domain.StatusPaused
				state.UpdatedAt = time.Now().UTC()
				if err := e.repo.SaveState(ctx, state); err != nil {
					log.Error("failed to persist paused state", "error", err)
					return
				}

				e.saveCheckpoint(ctx, &domain.ProcessCheckpoint{
					ProcessID: processID, TaskID: task.ID,
					Phase: domain.PhaseWaitingHuman, PreparedInput: preparedInput,
					Override: currentOverride,
				})
				cpExists = true

				// Record the WAITING_HUMAN step only on fresh entry (not on crash-restore).
				// skipInput==true means we restored from a PhaseWaitingHuman checkpoint.
				if !skipInput {
					e.emitSSE(processID, SSEEvent{
						Type:      SSEEventWaitHuman,
						ProcessID: processID,
						TaskID:    task.ID,
					})
					e.appendTrackStep(ctx, processID, task.ID, "",
						string(domain.TaskTypeHuman), domain.TrackStatusWaitingHuman, "")
				}

				ch := make(chan resumePayload, 1)
				e.mu.Lock()
				e.resumeChs[processID] = ch
				e.mu.Unlock()

				log.Info("process paused — awaiting human input", "task_id", task.ID,
					"task_instance_id", state.ActiveTaskInstanceID)

				// Notify listeners that LLM work and task activation are complete.
				// Emitted after the engine has created the active task instance and
				// persisted the paused state so clients observing the stream can
				// consider the LLM decision fully applied.
				e.emitSSE(processID, SSEEvent{
					Type:      SSEEventLLMCompleted,
					ProcessID: processID,
					TaskID:    task.ID,
				})

				select {
				case payload := <-ch:
					if payload.Abort {
						log.Info("abort signal received — ending process", "task_id", task.ID)
						state.Status = domain.StatusAborted
						state.Error = "process ended by user"
						state.ActiveTaskInstanceID = ""
						state.UpdatedAt = time.Now().UTC()
						_ = e.repo.SaveState(ctx, state)
						e.deleteCheckpoint(ctx, processID)
						e.mu.Lock()
						delete(e.resumeChs, processID)
						e.mu.Unlock()
						e.emitSSE(processID, SSEEvent{
							Type:      SSEEventAborted,
							ProcessID: processID,
							TaskID:    task.ID,
						})
						return
					}
					output = payload.Input
					// Stamp the task entry with the identity of the user who actioned it.
					if payload.ActorID != "" || payload.ActorGroups != nil {
						updateTaskEntryActor(state, task.ID, payload.ActorID, payload.ActorGroups)
					}
					if payload.Override != "" {
						currentOverride = payload.Override
						log.Info("user override received", "override", payload.Override)
					}
					log.Info("human input received", "task_id", task.ID)
				case <-ctx.Done():
					log.Info("context cancelled while paused")
					e.mu.Lock()
					delete(e.resumeChs, processID)
					e.mu.Unlock()
					return
				}

				e.mu.Lock()
				delete(e.resumeChs, processID)
				e.mu.Unlock()

				// Restore running status and clear the task instance ID.
				state.Status = domain.StatusRunning
				state.ActiveTaskInstanceID = ""

			default:
				executor, ok := e.executors[task.Type]
				if !ok {
					err := fmt.Errorf("no executor registered for task type %s", task.Type)
					log.Error("missing executor", "error", err)
					e.failProcess(ctx, state, err)
					return
				}
				// Ensure a task instance entry exists and record its start time
				instanceID := domain.NewUUID()
				setTaskEntry(state, task.ID, instanceID, nil)
				ensureTaskEntryHasGroup(state, def, task.ID)
				if err := e.persistTaskHistory(ctx, state, def, task.ID); err != nil {
					e.logger.Error("persist task history failed", "error", err, "task_id", task.ID)
				}
				output, err = executor.Execute(ctx, task, preparedInput)
				if err != nil {
					log.Error("task execution failed", "task_id", task.ID, "error", err)

					// Always capture whatever the task returned as task_output so the
					// GET endpoint never shows a blank entry for a failed SYSTEM task.
					var sysHTTPErr *SystemTaskError
					if errors.As(err, &sysHTTPErr) && sysHTTPErr.Response != nil {
						// HTTP error with a parseable response body.
						setTaskEntry(state, task.ID, instanceID, sysHTTPErr.Response)
						ensureTaskEntryHasGroup(state, def, task.ID)
						if err := e.persistTaskHistory(ctx, state, def, task.ID); err != nil {
							e.logger.Error("persist task history failed", "error", err, "task_id", task.ID)
						}
					} else {
						// Network / transport error — store the error string.
						setTaskEntry(state, task.ID, instanceID, map[string]any{
							"error": err.Error(),
						})
						ensureTaskEntryHasGroup(state, def, task.ID)
						if err := e.persistTaskHistory(ctx, state, def, task.ID); err != nil {
							e.logger.Error("persist task history failed", "error", err, "task_id", task.ID)
						}
					}

					// SYSTEM task failure: route back to the previous HUMAN task so the
					// operator can review and retry. The failed task ID is stored in state
					// so only that specific task is re-executed (not the whole chain).
					if task.Type == domain.TaskTypeSystem && state.LastHumanTaskID != "" {
						sysErr := fmt.Sprintf("task %s: %s", task.ID, err.Error())
						log.Warn("SYSTEM task failed — routing back to previous HUMAN task",
							"failed_task", task.ID,
							"human_task", state.LastHumanTaskID,
							"error", err,
						)
						e.emitSSE(processID, SSEEvent{
							Type:      SSEEventSystemTaskError,
							ProcessID: processID,
							TaskID:    task.ID,
							Error:     sysErr,
							Data:      map[string]any{"human_task_id": state.LastHumanTaskID},
						})
						state.FailedSystemTaskID = task.ID
						state.SystemError = sysErr
						state.CurrentTaskID = state.LastHumanTaskID
						// Ensure the HUMAN task we're routing back to has a skeleton entry.
						if findTaskEntryIndex(state.TaskOutputs, state.LastHumanTaskID) < 0 {
							setTaskEntry(state, state.LastHumanTaskID, "", nil)
							ensureTaskEntryHasGroup(state, def, state.LastHumanTaskID)
							if err := e.persistTaskHistory(ctx, state, def, state.LastHumanTaskID); err != nil {
								e.logger.Error("persist task history failed", "error", err, "task_id", state.LastHumanTaskID)
							}
						}
						state.UpdatedAt = time.Now().UTC()
						e.deleteCheckpoint(ctx, processID)
						if saveErr := e.repo.SaveState(ctx, state); saveErr != nil {
							log.Error("failed to save state during SYSTEM error recovery", "error", saveErr)
							e.failProcess(ctx, state, fmt.Errorf("task %s: %w", task.ID, err))
							return
						}
						continue outerLoop
					}
					e.failProcess(ctx, state, fmt.Errorf("task %s: %w", task.ID, err))
					return
				}
			}

			e.emitSSE(processID, SSEEvent{
				Type:      SSEEventTaskComplete,
				ProcessID: processID,
				TaskID:    task.ID,
			})
			// Checkpoint after execution so we don't re-execute on crash.
			e.saveCheckpoint(ctx, &domain.ProcessCheckpoint{
				ProcessID: processID, TaskID: task.ID,
				Phase: domain.PhaseTaskDone, PreparedInput: preparedInput,
				TaskOutput: output, Override: currentOverride,
			})
			cpExists = true
		}

		// --- Persist output ---
		var outputMap map[string]any
		if output != nil {
			outputMap = output
		} else {
			outputMap = make(map[string]any)
		}
		instanceID := state.ActiveTaskInstanceID
		if instanceID == "" {
			instanceID = domain.NewUUID()
		}
		setTaskEntry(state, task.ID, instanceID, outputMap)
		ensureTaskEntryHasGroup(state, def, task.ID)
		if err := e.persistTaskHistory(ctx, state, def, task.ID); err != nil {
			e.logger.Error("persist task history failed", "error", err, "task_id", task.ID)
		}
		state.LastCompletedTaskID = task.ID
		// advance chronological counter so next started tasks get a new order
		state.ChronologicalCounter++
		// Merge task output (excluding injected meta fields) into global context.
		for k, v := range outputMap {
			state.GlobalContext[k] = v
		}

		// --- Phase 3: Route to next task (skip if checkpointed) ---
		var nextTaskID string

		if skipRoute && cp.NextTaskID != "" {
			nextTaskID = cp.NextTaskID
			log.Info("restored routing from checkpoint",
				"from", task.ID,
				"to", nextTaskID,
			)
		} else if task.Type == domain.TaskTypeHuman && state.FailedSystemTaskID != "" {
			// A previous SYSTEM task failed and the human has just re-completed their
			// task. Route directly to the failed SYSTEM task (bypassing the LLM) so
			// only that specific task is retried, preserving outputs of any earlier
			// SYSTEM tasks in the same chain.
			nextTaskID = state.FailedSystemTaskID
			retyReason := fmt.Sprintf("retrying failed system task %s after human re-approval", nextTaskID)
			log.Info("retrying failed system task",
				"from", task.ID,
				"to", nextTaskID,
			)
			e.emitSSE(processID, SSEEvent{
				Type:      SSEEventLLMRouted,
				ProcessID: processID,
				TaskID:    task.ID,
				NextTask:  nextTaskID,
				Reasoning: retyReason,
			})
			e.updateWaitingHumanStep(ctx, processID, task.ID, nextTaskID, retyReason)
			state.FailedSystemTaskID = ""
			state.SystemError = ""
			e.saveCheckpoint(ctx, &domain.ProcessCheckpoint{
				ProcessID: processID, TaskID: task.ID,
				Phase: domain.PhaseRouted, PreparedInput: preparedInput,
				TaskOutput: output, NextTaskID: nextTaskID,
			})
		} else if directTarget, ok := parseDirectTaskOverride(currentOverride, task.ID, def.Tasks); ok {
			// The override contains an explicit task-jump directive (e.g. "forward
			// directly to Controle_Responsable"). Bypass the LLM entirely and route
			// deterministically to the named task.
			nextTaskID = directTarget
			reason := fmt.Sprintf("direct override: jumping from %s to %s (override: %q)",
				task.ID, nextTaskID, currentOverride)

			log.Info("direct task override applied — bypassing LLM routing",
				"from", task.ID,
				"to", nextTaskID,
				"override", currentOverride,
			)

			e.emitSSE(processID, SSEEvent{
				Type:      SSEEventLLMRouted,
				ProcessID: processID,
				TaskID:    task.ID,
				NextTask:  nextTaskID,
				Reasoning: reason,
				Data:      map[string]any{"override_applied": true, "direct_jump": true},
			})

			if task.Type == domain.TaskTypeHuman {
				e.updateWaitingHumanStep(ctx, processID, task.ID, nextTaskID, reason)
			} else {
				e.appendTrackStep(ctx, processID, task.ID, nextTaskID,
					string(task.Type), domain.TrackStatusRouted, reason)
			}

			e.saveCheckpoint(ctx, &domain.ProcessCheckpoint{
				ProcessID: processID, TaskID: task.ID,
				Phase: domain.PhaseRouted, PreparedInput: preparedInput,
				TaskOutput: output, NextTaskID: nextTaskID,
			})
		} else {
			forkEdges := def.ForkEdgesFrom(task.ID)
			if len(forkEdges) >= 2 {
				// --- Parallel fork path ---
				nextTaskID, err = e.executeFork(ctx, processID, task, outputMap, forkEdges, def, state)
				if err != nil {
					if errors.Is(err, errProcessAborted) {
						return // state already saved as ABORTED by executeFork
					}
					if ctx.Err() != nil {
						return
					}
					e.failProcess(ctx, state, err)
					return
				}
				// executeFork already saved state; fall through to update CurrentTaskID.
			} else {
				// --- Routing: deterministic shortcut first, LLM only when the
				// design offers a real choice. ---
				edges := def.NeighborsOf(task.ID)
				routeResp := &ai.RouteResponse{}
				if det := deterministicNext(def, task.ID); det != "" {
					// Deterministic route: the design has exactly one successor
					// (forks are handled above; joins below). No LLM reasoning is
					// needed — zero token consumption. Input preparation for the
					// next task still runs as usual.
					routeResp.NextTaskID = det
					routeResp.Reasoning = "Deterministic route: single successor in design"
					log.Info("deterministic routing (no LLM)", "from", task.ID, "to", det)
				} else {
					// --- LLM routing (genuine choice) ---
					reachable := def.ReachableFrom(task.ID)

					// Compact context before routing to reduce prompt size.
					compGlobal, compOutputs := e.compactForLLM(state.GlobalContext, state.TaskOutputs)
					routeReq := &ai.RouteRequest{
						Task:           task,
						Output:         e.compactAny(outputMap),
						Topology:       edges,
						AllEdges:       def.Topology,
						ReachableTasks: reachable,
						AllTasks:       def.Tasks,
						GlobalCtx:      compGlobal,
						UserOverride:   currentOverride,
						TaskOutputs:    compOutputs,
						DefinitionID:   def.ID,
						DefinitionDesc: def.Description,
						Lang:           extractLang(state.GlobalContext),
					}
					// Use a per-call timeout for routing as well.
					callCtx, callCancel := context.WithTimeout(ctx, defaultLLMCallTimeout)
					routeResp, err = e.llm.Route(callCtx, routeReq)
					callCancel()
					if err != nil {
						// If the model rejected the prompt due to context/token limits,
						// attempt a one-shot aggressive compaction retry before falling
						// back to the normal retry-or-fail behaviour.
						if isTokenLimitError(err) {
							log.Warn("LLM routing rejected due to token limit — retrying with aggressive compaction", "task_id", task.ID, "error", err)
							aggrGlobal, aggrOutputs := e.compactForLLMAggressive(state.GlobalContext, state.TaskOutputs)
							aggrOutput := e.compactAnyWithMaxField(outputMap, getEnvInt("LLM_AGGRESSIVE_MAX_FIELD_SIZE", 256))
							aggrReq := &ai.RouteRequest{
								Task:           task,
								Output:         aggrOutput,
								Topology:       edges,
								ReachableTasks: reachable,
								AllTasks:       def.Tasks,
								GlobalCtx:      aggrGlobal,
								UserOverride:   currentOverride,
								TaskOutputs:    aggrOutputs,
								DefinitionID:   def.ID,
								DefinitionDesc: def.Description,
								Lang:           extractLang(state.GlobalContext),
							}
							callCtx2, callCancel2 := context.WithTimeout(ctx, defaultLLMCallTimeout)
							routeResp, err = e.llm.Route(callCtx2, aggrReq)
							callCancel2()
						}
						if err != nil {
							if !cpExists {
								log.Error("LLM routing failed (no checkpoint)", "error", err)
								e.failProcess(ctx, state, fmt.Errorf("routing after %s: %w", task.ID, err))
								return
							}
							log.Warn("LLM unreachable during routing — retrying (checkpoint present)", "task_id", task.ID, "error", err)
							routeResp, err = llmRetry(ctx, log, func() (*ai.RouteResponse, error) {
								callCtx, callCancel := context.WithTimeout(ctx, defaultLLMCallTimeout)
								defer callCancel()
								return e.llm.Route(callCtx, routeReq)
							})
							if err != nil {
								log.Info("context cancelled while retrying LLM routing", "task_id", task.ID)
								return
							}
						}
					}
				}

				if _, ok := def.Tasks[routeResp.NextTaskID]; !ok {
					err := fmt.Errorf("LLM routed to unknown task %q", routeResp.NextTaskID)
					log.Error("invalid routing decision", "error", err)
					e.failProcess(ctx, state, err)
					return
				}

				nextTaskID = routeResp.NextTaskID

				log.Info("routed to next task",
					"from", task.ID,
					"to", routeResp.NextTaskID,
					"reasoning", routeResp.Reasoning,
					"override_applied", routeResp.OverrideApplied,
				)

				e.emitSSE(processID, SSEEvent{
					Type:      SSEEventLLMRouted,
					ProcessID: processID,
					TaskID:    task.ID,
					NextTask:  routeResp.NextTaskID,
					Reasoning: routeResp.Reasoning,
				})

				// Record this routing decision in the execution history.
				// For HUMAN tasks the WAITING_HUMAN step already exists; promote it
				// in-place so only the currently-active human task has an empty ToTaskID.
				// For all other task types, append a fresh ROUTED step.
				if task.Type == domain.TaskTypeHuman {
					e.updateWaitingHumanStep(ctx, processID, task.ID, routeResp.NextTaskID, routeResp.Reasoning)
				} else {
					e.appendTrackStep(ctx, processID, task.ID, routeResp.NextTaskID,
						string(task.Type), domain.TrackStatusRouted, routeResp.Reasoning)
				}

				// Publish a process-stream event for this LLM-driven transition.
				if e.dispatcher != nil {
					pevt := events.NewProcessEvent(
						processID,
						task.ID,
						routeResp.NextTaskID,
						routeResp.Reasoning,
						routeResp,
					)
					e.dispatcher.DispatchProcessEvent(ctx, pevt)
				}

				// Checkpoint after routing so we don't re-route on crash.
				e.saveCheckpoint(ctx, &domain.ProcessCheckpoint{
					ProcessID: processID, TaskID: task.ID,
					Phase: domain.PhaseRouted, PreparedInput: preparedInput,
					TaskOutput: output, NextTaskID: nextTaskID,
				})
			} // end normal LLM routing else
		} // end fork-or-LLM outer else

		// Ensure the next task already has a skeleton entry in task_outputs so
		// clients can see it immediately (before the next loop iteration runs and
		// calls its own setTaskEntry). Always create a new skeleton when the task
		// has no existing skeleton-without-instanceID (i.e. it either never ran or
		// already has a completed entry from a previous loop iteration).
		if nextTaskID != "" {
			idx := findTaskEntryIndex(state.TaskOutputs, nextTaskID)
			// Only skip creating a skeleton if the existing entry is already a
			// skeleton (no instanceID yet) — meaning we haven't started the task yet.
			needSkeleton := true
			if idx >= 0 {
				existingID, _ := state.TaskOutputs[idx]["task_instance_id"].(string)
				if existingID == "" {
					// Already has an unclaimed skeleton — no need for another.
					needSkeleton = false
				}
			}
			if needSkeleton {
				setTaskEntry(state, nextTaskID, "", nil)
				// Annotate skeleton with the task's configured group (if any).
				ensureTaskEntryHasGroup(state, def, nextTaskID)
				if err := e.persistTaskHistory(ctx, state, def, nextTaskID); err != nil {
					e.logger.Error("persist task history failed", "error", err, "task_id", nextTaskID)
				}
			}
		}

		state.CurrentTaskID = nextTaskID
		state.UpdatedAt = time.Now().UTC()
		if err := e.repo.SaveState(ctx, state); err != nil {
			log.Error("failed to persist state after routing", "error", err)
			return
		}

		// Iteration complete — clear checkpoint.
		e.deleteCheckpoint(ctx, processID)
	}
}

// findTaskOutput searches the TaskOutputs slice for the most recent entry with
// the given task_id and returns it. Search is performed from the end so the
// latest occurrence is returned when a task runs multiple times.
func findTaskOutput(list []map[string]any, taskID string) (map[string]any, bool) {
	for i := len(list) - 1; i >= 0; i-- {
		if list[i] == nil {
			continue
		}
		if id, ok := list[i]["task_id"].(string); ok && id == taskID {
			if to, ok := list[i]["task_output"].(map[string]any); ok {
				return to, true
			}
			// If task_output isn't a map (unexpected), return nil but found==false.
			return nil, false
		}
	}
	return nil, false
}

// findTaskEntryIndex returns the index of the entry for taskID or -1 if not found.
func findTaskEntryIndex(list []map[string]any, taskID string) int {
	for i := len(list) - 1; i >= 0; i-- {
		if list[i] == nil {
			continue
		}
		if id, ok := list[i]["task_id"].(string); ok && id == taskID {
			return i
		}
	}
	return -1
}

// updateTaskEntryActor stamps the task entry for taskID with the identity of
// the user who actioned the task (owner_id and group). It is a no-op when
// the entry does not yet exist.
func updateTaskEntryActor(state *domain.ExecutionState, taskID, actorID string, actorGroups any) {
	idx := findTaskEntryIndex(state.TaskOutputs, taskID)
	if idx < 0 {
		return
	}
	entry := state.TaskOutputs[idx]
	if actorID != "" {
		entry["owner_id"] = actorID
	}
	if actorGroups != nil {
		entry["group"] = actorGroups
	}
	state.TaskOutputs[idx] = entry
}

// setTaskEntry ensures the task entry exists and updates its task_instance_id and task_output.
// When taskID has been executed before but instanceID differs (re-execution / loop-back),
// a new entry is appended only if the last entry is already completed. If the last
// entry is a skeleton or still in-progress (no completed_at), it will be updated
// in-place so that the original placeholder reflects the eventual outcome.
func setTaskEntry(state *domain.ExecutionState, taskID, instanceID string, output map[string]any) {
	now := time.Now().UTC()
	idx := findTaskEntryIndex(state.TaskOutputs, taskID)
	if idx >= 0 {
		entry := state.TaskOutputs[idx]

		// If the most recent entry is not completed yet, update it in-place so
		// skeletons become the canonical record for that execution.
		if _, completed := entry["completed_at"]; !completed {
			if instanceID != "" {
				entry["task_instance_id"] = instanceID
				if _, ok := entry["created_at"]; !ok {
					entry["created_at"] = now.Format(time.RFC3339)
				}
			}
			if output != nil {
				entry["task_output"] = output
				entry["completed_at"] = now.Format(time.RFC3339)
			}
			if _, ok := entry["chronological_order"]; !ok {
				entry["chronological_order"] = state.ChronologicalCounter
			}
			state.TaskOutputs[idx] = entry
			return
		}

		// Most recent entry was already completed. If this is a re-execution with
		// a distinct instance ID, append a new entry to preserve prior run data.
		existingInstanceID, _ := entry["task_instance_id"].(string)
		if instanceID != "" && existingInstanceID != "" && existingInstanceID != instanceID {
			// fall through to append new entry below
		} else {
			// Otherwise update in-place (rare) — for example same instance id.
			if instanceID != "" {
				entry["task_instance_id"] = instanceID
				if _, ok := entry["created_at"]; !ok {
					entry["created_at"] = now.Format(time.RFC3339)
				}
			}
			if output != nil {
				entry["task_output"] = output
				entry["completed_at"] = now.Format(time.RFC3339)
			}
			if _, ok := entry["chronological_order"]; !ok {
				entry["chronological_order"] = state.ChronologicalCounter
			}
			state.TaskOutputs[idx] = entry
			return
		}
	}

	// Create a new entry (first execution OR explicit re-execution with a
	// different instance ID when the last run was already completed).
	newEntry := map[string]any{
		"task_id":             taskID,
		"task_instance_id":    instanceID,
		"task_output":         output,
		"chronological_order": state.ChronologicalCounter,
	}
	// Always record when the engine created the skeleton/entry so clients can
	// order and inspect paused tasks consistently.
	newEntry["created_at"] = now.Format(time.RFC3339)
	if output != nil {
		newEntry["completed_at"] = now.Format(time.RFC3339)
	}
	state.TaskOutputs = append(state.TaskOutputs, newEntry)
}

// ensureTaskEntryHasGroup sets the `group` field on the most recent task entry
// for taskID using the group declared in the process definition (if present)
// and only when the entry does not already have a group value.
func ensureTaskEntryHasGroup(state *domain.ExecutionState, def *domain.ProcessDefinition, taskID string) {
	if def == nil {
		return
	}
	task, ok := def.Tasks[taskID]
	if !ok || strings.TrimSpace(task.Group) == "" {
		return
	}
	idx := findTaskEntryIndex(state.TaskOutputs, taskID)
	if idx < 0 {
		return
	}
	entry := state.TaskOutputs[idx]
	if _, exists := entry["group"]; !exists {
		entry["group"] = []string{task.Group}
		state.TaskOutputs[idx] = entry
	}
}

// persistTaskHistory ensures the current task_outputs entry is mirrored in the
// persistent `task_histories` table. It inserts a new history row for newly
// observed skeletons and updates the row when the entry is completed.
func (e *Engine) persistTaskHistory(ctx context.Context, state *domain.ExecutionState, def *domain.ProcessDefinition, taskID string) error {
	if e.repo == nil {
		return nil
	}
	idx := findTaskEntryIndex(state.TaskOutputs, taskID)
	if idx < 0 {
		return nil
	}
	entry := state.TaskOutputs[idx]

	// Build TaskHistory from entry
	var h domain.TaskHistory
	if v, ok := entry["history_id"].(string); ok && v != "" {
		h.ID = v
	} else {
		h.ID = ""
	}
	h.ProcessID = state.ProcessID
	if co, ok := entry["chronological_order"].(int); ok {
		h.ChronologicalOrder = co
	} else if cof, ok := entry["chronological_order"].(float64); ok {
		h.ChronologicalOrder = int(cof)
	} else {
		h.ChronologicalOrder = state.ChronologicalCounter
	}
	if tid, ok := entry["task_id"].(string); ok {
		h.TaskID = tid
	} else {
		h.TaskID = taskID
	}
	if inst, ok := entry["task_instance_id"].(string); ok {
		h.TaskInstanceID = inst
	}
	if owner, ok := entry["owner_id"].(string); ok {
		h.OwnerID = owner
	}
	// groups may be []string or []any
	if g, ok := entry["group"]; ok {
		switch vv := g.(type) {
		case []string:
			h.Groups = vv
		case []any:
			for _, el := range vv {
				if s, ok := el.(string); ok {
					h.Groups = append(h.Groups, s)
				}
			}
		}
	} else if def != nil {
		if t, found := def.Tasks[taskID]; found && strings.TrimSpace(t.Group) != "" {
			h.Groups = []string{t.Group}
		}
	}
	if pi, ok := entry["prepared_input"].(map[string]any); ok {
		h.PreparedInput = pi
	}
	if to, ok := entry["task_output"].(map[string]any); ok {
		h.TaskOutput = to
	}
	// Parse created_at/completed_at if present (strings in RFC3339)
	if ca, ok := entry["created_at"].(string); ok && ca != "" {
		if t, err := time.Parse(time.RFC3339, ca); err == nil {
			h.CreatedAt = t
		}
	} else {
		h.CreatedAt = time.Now().UTC()
	}
	if cb, ok := entry["completed_at"].(string); ok && cb != "" {
		if t, err := time.Parse(time.RFC3339, cb); err == nil {
			h.CompletedAt = &t
		}
	}
	// Determine state
	stateLabel := "running"
	if h.CompletedAt != nil {
		stateLabel = "completed"
	} else {
		if h.TaskOutput != nil {
			if _, hasErr := h.TaskOutput["error"]; hasErr {
				stateLabel = "failed"
			}
		}
	}
	h.State = stateLabel
	h.TaskType = ""
	if def != nil {
		if t, ok := def.Tasks[taskID]; ok {
			h.TaskType = string(t.Type)
		}
	}
	// sizeBytes: marshal prepared+output to estimate
	var sb []byte
	if h.PreparedInput != nil {
		if b, err := json.Marshal(h.PreparedInput); err == nil {
			sb = append(sb, b...)
		}
	}
	if h.TaskOutput != nil {
		if b, err := json.Marshal(h.TaskOutput); err == nil {
			sb = append(sb, b...)
		}
	}
	h.SizeBytes = len(sb)
	h.UpdatedAt = time.Now().UTC()

	if h.ID == "" {
		// insert
		if err := e.repo.SaveTaskHistory(ctx, &h); err != nil {
			e.logger.Error("failed to save task history", "error", err, "task_id", taskID)
			return err
		}
		// persist mapping back into state so subsequent updates can find it
		entry["history_id"] = h.ID
		state.TaskOutputs[idx] = entry
		if err := e.repo.SaveState(ctx, state); err != nil {
			e.logger.Error("failed to persist state with history_id", "error", err)
		}
		return nil
	}
	// update existing
	if err := e.repo.UpdateTaskHistory(ctx, h.ID, &h); err != nil {
		e.logger.Error("failed to update task history", "error", err, "history_id", h.ID)
		return err
	}
	return nil
}

func (e *Engine) failProcess(ctx context.Context, state *domain.ExecutionState, err error) {
	state.Status = domain.StatusFailed
	state.Error = err.Error()
	state.UpdatedAt = time.Now().UTC()
	_ = e.repo.SaveState(ctx, state)
	e.deleteCheckpoint(ctx, state.ProcessID)
	e.emitSSE(state.ProcessID, SSEEvent{
		Type:      SSEEventFailed,
		ProcessID: state.ProcessID,
		TaskID:    state.CurrentTaskID,
		Error:     err.Error(),
	})
}

func forkSetMatches(persisted map[string]*domain.ParallelBranch, targets []string) bool {
	if len(persisted) != len(targets) {
		return false
	}
	for _, t := range targets {
		if _, ok := persisted[t]; !ok {
			return false
		}
	}
	return true
}

// executeFork runs all fork-edge targets in parallel, waits for the join
// condition (AND = all complete, OR = first to complete), merges outputs into
// GlobalContext, then calls the LLM to route via join edges and returns the
// next task ID.
//
// The join mode is read from the JoinEdgesForTasks set; it defaults to "and".
// On abort the state is marked ABORTED and errProcessAborted is returned.
func (e *Engine) executeFork(
	ctx context.Context,
	processID string,
	originTask *domain.Task,
	originOutput map[string]any,
	forkEdges []domain.Edge,
	def *domain.ProcessDefinition,
	state *domain.ExecutionState,
) (string, error) {
	log := e.logger.With("process_id", processID, "fork_origin", originTask.ID)

	forkTargetIDs := make([]string, 0, len(forkEdges))
	for _, fe := range forkEdges {
		forkTargetIDs = append(forkTargetIDs, fe.To)
	}

	joinEdges, joinMode := def.JoinEdgesForTasks(forkTargetIDs)
	// Resume mode: if the state already carries branch records for exactly
	// these targets (persisted before a restart/crash), resume from them
	// instead of re-initializing — re-initialization would reset completed
	// branches to running and lose their outputs, stranding the join (I-7).
	resuming := len(state.ParallelBranches) > 0 && forkSetMatches(state.ParallelBranches, forkTargetIDs)
	if resuming {
		log.Info("resuming fork from persisted state", "branches", forkTargetIDs, "join_mode", joinMode)
	} else {
		log.Info("starting fork", "branches", forkTargetIDs, "join_mode", joinMode)
	}

	// --- 1. Prepare all branch inputs before touching channels or state. ---
	// Skipped when resuming: running branches keep their persisted task
	// entries (prepared_input included) and completed branches need none.
	branchInputs := make(map[string]map[string]any, len(forkEdges))
	if !resuming {
		for _, fe := range forkEdges {
			task := def.Tasks[fe.To]
			// Compact global context and task outputs for the fork branch LLM call.
			compGlobal, compOutputs := e.compactForLLM(state.GlobalContext, state.TaskOutputs)
			prepReq := &ai.PrepareInputRequest{
				Task:           task,
				GlobalCtx:      compGlobal,
				PrevOutput:     e.compactAny(originOutput),
				TaskOutputs:    compOutputs,
				DefinitionID:   def.ID,
				DefinitionDesc: def.Description,
				Lang:           extractLang(state.GlobalContext),
			}
			// Apply per-call timeout for fork branch input preparation.
			callCtx, callCancel := context.WithTimeout(ctx, defaultLLMCallTimeout)
			prepInput, err := e.llm.PrepareInput(callCtx, prepReq)
			callCancel()
			if err != nil {
				// If the LLM rejected the prompt due to token limits, try an
				// aggressive compaction for this branch and retry once.
				if isTokenLimitError(err) {
					log.Warn("fork branch prepare input rejected due to token limit — retrying with aggressive compaction", "branch", fe.To, "error", err)
					aggrGlobal, aggrOutputs := e.compactForLLMAggressive(state.GlobalContext, state.TaskOutputs)
					aggrPrev := e.compactAnyWithMaxField(originOutput, getEnvInt("LLM_AGGRESSIVE_MAX_FIELD_SIZE", 256))
					aggrReq := &ai.PrepareInputRequest{
						Task:           task,
						GlobalCtx:      aggrGlobal,
						PrevOutput:     aggrPrev,
						TaskOutputs:    aggrOutputs,
						DefinitionID:   def.ID,
						DefinitionDesc: def.Description,
						Lang:           extractLang(state.GlobalContext),
					}
					callCtx2, callCancel2 := context.WithTimeout(ctx, defaultLLMCallTimeout)
					prepInput, err = e.llm.PrepareInput(callCtx2, aggrReq)
					callCancel2()
				}
				if err != nil {
					return "", fmt.Errorf("prepare input for fork branch %s: %w", fe.To, err)
				}
			}
			branchInputs[fe.To] = prepInput
		}
	}

	// --- 2. Initialise branch state and skeleton task entries. ---
	branches := make(map[string]*domain.ParallelBranch, len(forkEdges))
	if resuming {
		// Reuse the persisted branch records (statuses, outputs, instance
		// IDs) so completed branches stay completed and running branches
		// keep the instance ID the client already sees.
		for _, fe := range forkEdges {
			branches[fe.To] = state.ParallelBranches[fe.To]
			log.Info("fork branch resumed", "branch", fe.To, "status", branches[fe.To].Status, "instance", branches[fe.To].InstanceID)
		}
	} else {
		for _, fe := range forkEdges {
			instanceID := domain.NewUUID()
			branches[fe.To] = &domain.ParallelBranch{
				TaskID:     fe.To,
				InstanceID: instanceID,
				Status:     domain.BranchRunning,
			}
			setTaskEntry(state, fe.To, instanceID, nil)
			// Annotate branch entry with configured group (if any)
			ensureTaskEntryHasGroup(state, def, fe.To)
			// If the LLM prepared an input for this branch, store it as prepared_input
			// so clients can inspect the prepopulated values.
			if prep, ok := branchInputs[fe.To]; ok && prep != nil {
				idx := findTaskEntryIndex(state.TaskOutputs, fe.To)
				if idx >= 0 {
					entry := state.TaskOutputs[idx]
					entry["prepared_input"] = prep
					state.TaskOutputs[idx] = entry
				}
			}
			// Persist the skeleton branch entry
			if err := e.persistTaskHistory(ctx, state, def, fe.To); err != nil {
				e.logger.Error("persist task history failed", "error", err, "task_id", fe.To)
			}
		}
	}

	// --- 3. Register per-instance resume channels BEFORE saving state so that
	//        CompleteTask can find them as soon as the DB is updated. ---
	// Completed branches (resume mode) need no channel.
	for _, branch := range branches {
		task := def.Tasks[branch.TaskID]
		if task.Type == domain.TaskTypeHuman && branch.Status == domain.BranchRunning {
			ch := make(chan resumePayload, 1)
			e.mu.Lock()
			e.resumeChs[branch.InstanceID] = ch
			e.mu.Unlock()
		}
	}

	state.ParallelBranches = branches
	state.Status = domain.StatusPaused
	state.UpdatedAt = time.Now().UTC()
	if err := e.repo.SaveState(ctx, state); err != nil {
		// Roll back channel registrations.
		e.mu.Lock()
		for _, branch := range branches {
			delete(e.resumeChs, branch.InstanceID)
		}
		e.mu.Unlock()
		return "", fmt.Errorf("save fork state: %w", err)
	}

	e.emitSSE(processID, SSEEvent{
		Type:      SSEEventForkStarted,
		ProcessID: processID,
		TaskID:    originTask.ID,
		Data:      map[string]any{"branches": forkTargetIDs, "join_mode": joinMode},
	})

	// joinCtx is cancelled when the join condition is met or the parent context
	// is cancelled. This unblocks goroutines waiting on OR-mode stragglers.
	joinCtx, joinCancel := context.WithCancel(ctx)
	defer joinCancel()

	doneCh := make(chan forkBranchResult, len(forkEdges))

	// --- 4. Launch one goroutine per branch. ---
	for _, fe := range forkEdges {
		targetTaskID := fe.To
		task := def.Tasks[targetTaskID]
		branch := branches[targetTaskID]
		preparedInput := branchInputs[targetTaskID]

		// Resumed branch that had already finished before the restart: feed
		// its persisted output straight into the join collection.
		if branch.Status == domain.BranchCompleted {
			b := branch
			go func() {
				doneCh <- forkBranchResult{taskID: b.TaskID, instanceID: b.InstanceID, output: b.Output, actorID: b.ActorID, actorGroups: b.ActorGroups}
			}()
			continue
		}

		switch task.Type {
		case domain.TaskTypeHuman:
			if !resuming {
				e.emitSSE(processID, SSEEvent{
					Type:      SSEEventWaitHuman,
					ProcessID: processID,
					TaskID:    targetTaskID,
					Data:      map[string]any{"task_instance_id": branch.InstanceID, "parallel": true},
				})
				e.appendTrackStep(ctx, processID, targetTaskID, "", string(task.Type),
					domain.TrackStatusWaitingHuman, "parallel fork branch")
			}

			ch := e.resumeChs[branch.InstanceID] // already registered above
			instanceID := branch.InstanceID

			go func(taskID, instanceID string, ch chan resumePayload) {
				defer func() {
					e.mu.Lock()
					delete(e.resumeChs, instanceID)
					e.mu.Unlock()
				}()
				select {
				case payload := <-ch:
					doneCh <- forkBranchResult{
						taskID:      taskID,
						instanceID:  instanceID,
						output:      payload.Input,
						actorID:     payload.ActorID,
						actorGroups: payload.ActorGroups,
						cancelled:   payload.Abort,
					}
				case <-joinCtx.Done():
					doneCh <- forkBranchResult{taskID: taskID, instanceID: instanceID, cancelled: true}
				}
			}(targetTaskID, instanceID, ch)

		case domain.TaskTypeSystem:
			executor := e.executors[domain.TaskTypeSystem]
			instanceID := branch.InstanceID

			go func(task *domain.Task, instanceID string, input map[string]any) {
				out, execErr := executor.Execute(joinCtx, task, input)
				if execErr != nil {
					var sysHTTPErr *SystemTaskError
					var errOut map[string]any
					if errors.As(execErr, &sysHTTPErr) && sysHTTPErr.Response != nil {
						errOut = sysHTTPErr.Response
					} else {
						errOut = map[string]any{"error": execErr.Error()}
					}
					doneCh <- forkBranchResult{
						taskID:     task.ID,
						instanceID: instanceID,
						output:     errOut,
						err:        execErr,
					}
					return
				}
				doneCh <- forkBranchResult{taskID: task.ID, instanceID: instanceID, output: out}
			}(task, instanceID, preparedInput)

		default:
			// Unsupported branch type — treat as instant completion with empty output.
			go func(taskID, instanceID string) {
				doneCh <- forkBranchResult{taskID: taskID, instanceID: instanceID, output: map[string]any{}}
			}(targetTaskID, branch.InstanceID)
		}
	}

	// Emit LLM_COMPLETED after all branch resume channels and WAIT_HUMAN events
	// have been emitted/registered so listeners observe the final activation
	// state of the forked human tasks.
	e.emitSSE(processID, SSEEvent{
		Type:      SSEEventLLMCompleted,
		ProcessID: processID,
		Data:      map[string]any{"branches": forkTargetIDs},
	})

	// --- 5. Collect results based on join mode. ---
	requiredCount := len(forkEdges) // AND: wait for all
	if joinMode == "or" {
		requiredCount = 1
	}

	collected := 0
	var lastCompletedTaskID string

	for collected < requiredCount {
		select {
		case result := <-doneCh:
			if result.cancelled {
				// Abort received — cancel all remaining branches, drain, then exit.
				joinCancel()
				remaining := len(forkEdges) - collected - 1
				for range remaining {
					<-doneCh
				}
				state.ParallelBranches = nil
				state.Status = domain.StatusAborted
				state.Error = "process ended by user"
				state.UpdatedAt = time.Now().UTC()
				_ = e.repo.SaveState(ctx, state)
				e.emitSSE(processID, SSEEvent{
					Type:      SSEEventAborted,
					ProcessID: processID,
					TaskID:    originTask.ID,
				})
				return "", errProcessAborted
			}

			branch := branches[result.taskID]
			branch.Status = domain.BranchCompleted
			branch.Output = result.output
			if result.actorID != "" {
				branch.ActorID = result.actorID
				branch.ActorGroups = result.actorGroups
			}

			setTaskEntry(state, result.taskID, result.instanceID, result.output)
			ensureTaskEntryHasGroup(state, def, result.taskID)
			if result.actorID != "" {
				updateTaskEntryActor(state, result.taskID, result.actorID, result.actorGroups)
			}
			if err := e.persistTaskHistory(ctx, state, def, result.taskID); err != nil {
				e.logger.Error("persist task history failed", "error", err, "task_id", result.taskID)
			}
			for k, v := range result.output {
				state.GlobalContext[k] = v
			}
			lastCompletedTaskID = result.taskID
			state.UpdatedAt = time.Now().UTC()
			if saveErr := e.repo.SaveState(ctx, state); saveErr != nil {
				log.Error("failed to save state after branch completion", "error", saveErr, "task_id", result.taskID)
			}

			isLast := (collected + 1) == requiredCount
			e.emitSSE(processID, SSEEvent{
				Type:      SSEEventForkBranchDone,
				ProcessID: processID,
				TaskID:    result.taskID,
				Data:      map[string]any{"instance_id": result.instanceID, "is_last": isLast},
			})
			e.appendTrackStep(ctx, processID, result.taskID, "", string(def.Tasks[result.taskID].Type),
				domain.TrackStatusRouted, "fork branch completed")

			log.Info("fork branch completed",
				"task_id", result.taskID,
				"remaining", requiredCount-collected-1,
			)
			collected++

		case <-ctx.Done():
			joinCancel()
			return "", ctx.Err()
		}
	}

	// Cancel any remaining goroutines (important in OR mode) and drain.
	joinCancel()
	remaining := len(forkEdges) - collected
	for range remaining {
		r := <-doneCh
		// Capture stragglers that completed naturally before the cancel arrived.
		if !r.cancelled && r.err == nil && r.output != nil {
			if b, ok := branches[r.taskID]; ok && b.Status != domain.BranchCompleted {
				b.Status = domain.BranchCompleted
				setTaskEntry(state, r.taskID, r.instanceID, r.output)
				ensureTaskEntryHasGroup(state, def, r.taskID)
				if err := e.persistTaskHistory(ctx, state, def, r.taskID); err != nil {
					e.logger.Error("persist task history failed", "error", err, "task_id", r.taskID)
				}
				for k, v := range r.output {
					state.GlobalContext[k] = v
				}
			}
		}
	}

	// --- 6. Fork resolved: clear branches and route via join edges. ---
	state.ParallelBranches = nil
	state.Status = domain.StatusRunning
	state.LastCompletedTaskID = lastCompletedTaskID
	state.ChronologicalCounter++

	e.emitSSE(processID, SSEEvent{
		Type:      SSEEventForkJoined,
		ProcessID: processID,
		TaskID:    originTask.ID,
		Data:      map[string]any{"branches": forkTargetIDs, "join_mode": joinMode},
	})

	reachableTasks := def.ReachableFromJoin(forkTargetIDs)
	if len(reachableTasks) == 0 {
		return "", fmt.Errorf("no join edges defined for fork branches %v", forkTargetIDs)
	}

	joinTaskID := fmt.Sprintf("fork-join[%s]", strings.Join(forkTargetIDs, ","))

	// Deterministic join: a single merge target with no condition — the
	// design fixes the route, so no LLM reasoning is needed (zero tokens).
	if det := deterministicJoinTarget(joinEdges); det != "" {
		if _, ok := def.Tasks[det]; !ok {
			return "", fmt.Errorf("join target %q not found in definition", det)
		}
		log.Info("fork join routed (deterministic, no LLM)", "next_task", det)
		e.emitSSE(processID, SSEEvent{
			Type:      SSEEventLLMRouted,
			ProcessID: processID,
			TaskID:    joinTaskID,
			NextTask:  det,
			Reasoning: "Deterministic join: single merge target in design",
		})
		e.appendTrackStep(ctx, processID, joinTaskID, det, "FORK_JOIN",
			domain.TrackStatusRouted, "Deterministic join: single merge target in design")
		return det, nil
	}

	// Build a synthetic task descriptor so the LLM understands the routing context.
	// Include key decision-relevant fields from each branch so the LLM can evaluate
	// join conditions without having to mine the full global context.
	joinBranchSummary := func() string {
		var parts []string
		for _, branchID := range forkTargetIDs {
			if b, ok := branches[branchID]; ok && b.Output != nil {
				if out, err := json.Marshal(b.Output); err == nil {
					parts = append(parts, fmt.Sprintf("%s=%s", branchID, string(out)))
				}
			}
		}
		if len(parts) == 0 {
			return ""
		}
		return " Branch outputs: " + strings.Join(parts, "; ") + "."
	}()
	joinTask := &domain.Task{
		ID:   joinTaskID,
		Type: domain.TaskTypeSystem,
		Description: fmt.Sprintf(
			"Parallel fork-join: branches [%s] completed (mode: %s).%s Route to the correct next task based on the combined branch outputs and the join edge conditions.",
			strings.Join(forkTargetIDs, ", "), joinMode, joinBranchSummary,
		),
	}

	// Build a branch-keyed output map: this is the primary signal for the
	// fork-join routing decision. Passing the (large, merged) global context
	// as Output causes it to be compacted away; using per-branch outputs keeps
	// the payload small and semantically precise.
	branchOutputsMap := make(map[string]map[string]any, len(forkTargetIDs))
	for _, branchID := range forkTargetIDs {
		if b, ok := branches[branchID]; ok && b.Output != nil {
			branchOutputsMap[branchID] = b.Output
		} else {
			branchOutputsMap[branchID] = map[string]any{}
		}
	}

	// Compact context for fork-join routing (used as supplementary context only).
	compGlobal, compOutputs := e.compactForLLM(state.GlobalContext, state.TaskOutputs)
	callCtx, callCancel := context.WithTimeout(ctx, defaultLLMCallTimeout)
	routeResp, err := e.llm.Route(callCtx, &ai.RouteRequest{
		Task:           joinTask,
		Output:         branchOutputsMap,
		Topology:       joinEdges,
		AllEdges:       def.Topology,
		ReachableTasks: reachableTasks,
		AllTasks:       def.Tasks,
		GlobalCtx:      compGlobal,
		TaskOutputs:    compOutputs,
		DefinitionID:   def.ID,
		DefinitionDesc: def.Description,
		Lang:           extractLang(state.GlobalContext),
		BranchOutputs:  branchOutputsMap,
	})
	callCancel()
	if err != nil {
		// Token-limit fallback: branch outputs are already small so this rarely
		// triggers, but keep parity with the normal routing retry path.
		if isTokenLimitError(err) {
			log.Warn("fork-join routing rejected due to token limit — retrying with aggressive compaction", "fork", forkTargetIDs, "error", err)
			aggrGlobal, aggrOutputs := e.compactForLLMAggressive(state.GlobalContext, state.TaskOutputs)
			callCtx2, callCancel2 := context.WithTimeout(ctx, defaultLLMCallTimeout)
			routeResp, err = e.llm.Route(callCtx2, &ai.RouteRequest{
				Task:           joinTask,
				Output:         branchOutputsMap,
				Topology:       joinEdges,
				AllEdges:       def.Topology,
				ReachableTasks: reachableTasks,
				AllTasks:       def.Tasks,
				GlobalCtx:      aggrGlobal,
				TaskOutputs:    aggrOutputs,
				DefinitionID:   def.ID,
				DefinitionDesc: def.Description,
				Lang:           extractLang(state.GlobalContext),
				BranchOutputs:  branchOutputsMap,
			})
			callCancel2()
		}
		if err != nil {
			return "", fmt.Errorf("routing after fork join: %w", err)
		}
	}
	if _, ok := def.Tasks[routeResp.NextTaskID]; !ok {
		return "", fmt.Errorf("LLM routed to unknown task %q after fork join", routeResp.NextTaskID)
	}

	log.Info("fork join routed", "next_task", routeResp.NextTaskID, "reasoning", routeResp.Reasoning)
	e.emitSSE(processID, SSEEvent{
		Type:      SSEEventLLMRouted,
		ProcessID: processID,
		TaskID:    joinTaskID,
		NextTask:  routeResp.NextTaskID,
		Reasoning: routeResp.Reasoning,
	})
	e.appendTrackStep(ctx, processID, joinTaskID, routeResp.NextTaskID, "FORK_JOIN",
		domain.TrackStatusRouted, routeResp.Reasoning)

	return routeResp.NextTaskID, nil
}

// deterministicNext returns the forced next task when the design fixes the
// route — exactly one non-fork outgoing edge from taskID — or "" when the
// LLM must choose (multiple successors).
func deterministicNext(def *domain.ProcessDefinition, taskID string) string {
	edges := def.NeighborsOf(taskID)
	if len(edges) == 1 {
		return edges[0].To
	}
	return ""
}

// deterministicJoinTarget returns the forced join target when a fork-join has
// a single merge edge without a condition, or "" when the LLM must evaluate
// the join conditions.
func deterministicJoinTarget(joinEdges []domain.Edge) string {
	if len(joinEdges) == 1 && joinEdges[0].Condition == "" {
		return joinEdges[0].To
	}
	return ""
}

// llmRetry calls fn with exponential back-off until it succeeds or ctx is
// cancelled. It is used when a checkpoint exists so the process state is
// safe to resume — we just wait for the LLM to become reachable again.
// Delays start at 2 s and double up to a 60 s ceiling.
func llmRetry[T any](ctx context.Context, log *slog.Logger, fn func() (T, error)) (T, error) {
	const (
		baseDelay = 2 * time.Second
		maxDelay  = 60 * time.Second
	)
	delay := baseDelay
	for {
		result, err := fn()
		if err == nil {
			return result, nil
		}
		log.Warn("LLM not reachable, will retry", "error", err, "retry_in", delay)
		select {
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
}

// ListInstances returns all process instances for a given definition ID.
func (e *Engine) ListInstances(ctx context.Context, definitionID string) ([]*domain.ExecutionState, error) {
	all, err := e.repo.ListStates(ctx)
	if err != nil {
		return nil, err
	}
	var result []*domain.ExecutionState
	for _, st := range all {
		if st.DefinitionID == definitionID {
			result = append(result, st)
		}
	}
	return result, nil
}

// contextContainsKeyword recursively walks v (which may be a map, slice, or
// scalar) and returns true as soon as any leaf value, when converted to a
// string, contains keyword (case-insensitive).
func contextContainsKeyword(v any, keyword string) bool {
	if keyword == "" {
		return true
	}
	switch val := v.(type) {
	case map[string]any:
		for _, child := range val {
			if contextContainsKeyword(child, keyword) {
				return true
			}
		}
	case []any:
		for _, item := range val {
			if contextContainsKeyword(item, keyword) {
				return true
			}
		}
	case string:
		return strings.Contains(strings.ToLower(val), keyword)
	case bool:
		return strings.Contains(fmt.Sprintf("%v", val), keyword)
	case float64:
		return strings.Contains(fmt.Sprintf("%v", val), keyword)
	case nil:
		// nothing to match
	default:
		return strings.Contains(strings.ToLower(fmt.Sprintf("%v", val)), keyword)
	}
	return false
}

// SearchInstances returns all instances for definitionID whose global_context
// contains a value matching keyword (case-insensitive substring match,
// recursive). groups is used only for authorization in the HTTP layer; it is
// not applied as a result filter here.
func (e *Engine) SearchInstances(ctx context.Context, definitionID, keyword string) ([]*domain.ExecutionState, error) {
	all, err := e.repo.ListStates(ctx)
	if err != nil {
		return nil, err
	}

	kw := strings.ToLower(strings.TrimSpace(keyword))

	var result []*domain.ExecutionState
	for _, st := range all {
		if st.DefinitionID != definitionID {
			continue
		}
		// Keyword filter: recursive scan of the whole global_context.
		if kw != "" && !contextContainsKeyword(st.GlobalContext, kw) {
			continue
		}
		result = append(result, st)
	}
	return result, nil
}

// AbortProcess marks a process as aborted. If it is paused, the execution loop
// receives an abort signal and exits cleanly. For running processes the state
// is written to the database; the loop detects the abort on its next cycle.
func (e *Engine) AbortProcess(ctx context.Context, processID string) error {
	state, err := e.repo.GetState(ctx, processID)
	if err != nil {
		return fmt.Errorf("get state: %w", err)
	}
	if state.Status == domain.StatusCompleted || state.Status == domain.StatusAborted {
		return fmt.Errorf("process %s is already %s", processID, state.Status)
	}

	// Parallel fork mode: send abort to each running branch channel. The
	// executeFork goroutine will detect the first abort signal, cancel the rest,
	// and update the state itself.
	if len(state.ParallelBranches) > 0 {
		e.mu.Lock()
		aborted := 0
		for _, branch := range state.ParallelBranches {
			if branch.Status == domain.BranchRunning {
				if ch, ok := e.resumeChs[branch.InstanceID]; ok {
					select {
					case ch <- resumePayload{Abort: true}:
						aborted++
					default:
					}
				}
			}
		}
		e.mu.Unlock()
		if aborted > 0 {
			// executeFork handles the state update when it processes the abort.
			return nil
		}
		// Fall through: branches were running but channels are gone (race — fork
		// completed); handle as a normal abort below.
	}

	e.mu.RLock()
	ch, paused := e.resumeChs[processID]
	e.mu.RUnlock()

	if paused {
		// Signal the blocked runLoop to abort — it will update the state itself.
		select {
		case ch <- resumePayload{Abort: true}:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	}

	// Process is RUNNING — mark it directly; the loop exits on its next state read.
	state.Status = domain.StatusAborted
	state.Error = "process ended by user"
	state.ActiveTaskInstanceID = ""
	state.UpdatedAt = time.Now().UTC()
	return e.repo.SaveState(ctx, state)
}

// TaskInstanceInfo bundles a paused task's state for the GET /{def}/{proc}/{taskName}/{taskId} endpoint.
type TaskInstanceInfo struct {
	ProcessID      string         `json:"process_id"`
	DefinitionID   string         `json:"definition_id"`
	TaskID         string         `json:"task_id"`
	TaskInstanceID string         `json:"task_instance_id"`
	Description    string         `json:"description"`
	Group          string         `json:"group"`
	GlobalCtx      map[string]any `json:"global_context,omitempty"`
	PausedSince    string         `json:"paused_since"`
}

// GetTaskInstance returns metadata about the currently paused task instance,
// validating that task_name and task_instance_id match the live state.
// Works for both normal single-task pause and parallel fork branches.
func (e *Engine) GetTaskInstance(ctx context.Context, processID, taskName, taskInstanceID string) (*TaskInstanceInfo, error) {
	state, err := e.repo.GetState(ctx, processID)
	if err != nil {
		return nil, err
	}
	if state.Status != domain.StatusPaused {
		return nil, fmt.Errorf("process %s is not paused", processID)
	}

	def, err := e.definitionForState(ctx, state)
	if err != nil {
		return nil, err
	}

	// Parallel fork mode: look up by branch.
	if len(state.ParallelBranches) > 0 {
		branch, ok := state.ParallelBranches[taskName]
		if !ok || branch.InstanceID != taskInstanceID {
			return nil, fmt.Errorf("task instance %q not found in parallel branches", taskInstanceID)
		}
		task, ok := def.Tasks[taskName]
		if !ok {
			return nil, fmt.Errorf("task %q not found", taskName)
		}
		return &TaskInstanceInfo{
			ProcessID:      state.ProcessID,
			DefinitionID:   state.DefinitionID,
			TaskID:         task.ID,
			TaskInstanceID: branch.InstanceID,
			Description:    task.Description,
			Group:          task.Group,
			GlobalCtx:      state.GlobalContext,
			PausedSince:    state.UpdatedAt.Format("2006-01-02T15:04:05Z"),
		}, nil
	}

	if state.CurrentTaskID != taskName {
		return nil, fmt.Errorf("active task is %q, not %q", state.CurrentTaskID, taskName)
	}
	if state.ActiveTaskInstanceID != taskInstanceID {
		return nil, fmt.Errorf("task instance %q not found", taskInstanceID)
	}

	task := def.Tasks[state.CurrentTaskID]

	return &TaskInstanceInfo{
		ProcessID:      state.ProcessID,
		DefinitionID:   state.DefinitionID,
		TaskID:         task.ID,
		TaskInstanceID: state.ActiveTaskInstanceID,
		Description:    task.Description,
		Group:          task.Group,
		GlobalCtx:      state.GlobalContext,
		PausedSince:    state.UpdatedAt.Format("2006-01-02T15:04:05Z"),
	}, nil
}

// ActiveHumanTask describes the single HUMAN task currently paused in an instance.
type ActiveHumanTask struct {
	ProcessID      string         `json:"process_id"`
	DefinitionID   string         `json:"definition_id"`
	TaskID         string         `json:"task_id"`
	TaskInstanceID string         `json:"task_instance_id"`
	Description    string         `json:"description"`
	Group          string         `json:"group"`
	InputSchema    any            `json:"input_schema,omitempty"`
	GlobalCtx      map[string]any `json:"global_context,omitempty"`
	PausedSince    string         `json:"paused_since"`
}

// ListActiveHumanTasks returns the active HUMAN tasks for a specific process instance.
// In normal mode at most one item is returned. In parallel-fork mode one item is
// returned per running branch that is a HUMAN task.
// Returns an empty slice (not an error) when the instance is not paused at a HUMAN task.
func (e *Engine) ListActiveHumanTasks(ctx context.Context, processID string) ([]ActiveHumanTask, error) {
	state, err := e.repo.GetState(ctx, processID)
	if err != nil {
		return nil, err
	}

	if state.Status != domain.StatusPaused {
		return []ActiveHumanTask{}, nil
	}

	def, err := e.definitionForState(ctx, state)
	if err != nil {
		return nil, err
	}

	// Parallel fork mode: return one entry per running HUMAN branch.
	if len(state.ParallelBranches) > 0 {
		var tasks []ActiveHumanTask
		for taskID, branch := range state.ParallelBranches {
			if branch.Status != domain.BranchRunning {
				continue
			}
			task, ok := def.Tasks[taskID]
			if !ok || task.Type != domain.TaskTypeHuman {
				continue
			}
			tasks = append(tasks, ActiveHumanTask{
				ProcessID:      state.ProcessID,
				DefinitionID:   state.DefinitionID,
				TaskID:         task.ID,
				TaskInstanceID: branch.InstanceID,
				Description:    task.Description,
				Group:          task.Group,
				InputSchema:    task.InputSchema,
				GlobalCtx:      state.GlobalContext,
				PausedSince:    state.UpdatedAt.Format("2006-01-02T15:04:05Z"),
			})
		}
		return tasks, nil
	}

	task, ok := def.Tasks[state.CurrentTaskID]
	if !ok || task.Type != domain.TaskTypeHuman {
		return []ActiveHumanTask{}, nil
	}

	return []ActiveHumanTask{{
		ProcessID:      state.ProcessID,
		DefinitionID:   state.DefinitionID,
		TaskID:         task.ID,
		TaskInstanceID: state.ActiveTaskInstanceID,
		Description:    task.Description,
		Group:          task.Group,
		InputSchema:    task.InputSchema,
		GlobalCtx:      state.GlobalContext,
		PausedSince:    state.UpdatedAt.Format("2006-01-02T15:04:05Z"),
	}}, nil
}

// CompleteTask resumes a paused HUMAN task after validating task identity.
// It fetches existing task comments and injects them into the resume payload so
// the LLM routing step has full comment context for its decision.
// actorID and actorGroups identify the user who is completing the task and are
// stored in the task_outputs entry for that task.
func (e *Engine) CompleteTask(ctx context.Context, processID, taskName, taskInstanceID string, input map[string]any, override string, actorID string, actorGroups any, actorDepartments []string) error {
	state, err := e.repo.GetState(ctx, processID)
	if err != nil {
		return fmt.Errorf("get state: %w", err)
	}
	if state.Status != domain.StatusPaused {
		return fmt.Errorf("process %s is not paused", processID)
	}

	// Authorization: the caller must belong to the task's group and, when
	// departments are involved, to a department of the instance.
	if err := e.validateTaskCaller(ctx, state, taskName, actorGroups, actorDepartments); err != nil {
		return err
	}

	// Inject any stored comments into the input payload.
	if e.commentStore != nil {
		comments, err := e.commentStore.ListComments(ctx, processID, taskInstanceID)
		if err == nil && len(comments) > 0 {
			var summaries []string
			for _, c := range comments {
				summaries = append(summaries, c.Content)
			}
			if input == nil {
				input = make(map[string]any)
			}
			input["_task_comments"] = summaries
		}
	}

	// --- Parallel fork mode: route to the per-instance channel. ---
	if len(state.ParallelBranches) > 0 {
		branch, ok := state.ParallelBranches[taskName]
		if !ok {
			return fmt.Errorf("task %q is not an active parallel branch for process %s", taskName, processID)
		}
		if branch.InstanceID != taskInstanceID {
			return fmt.Errorf("task instance %q not found in parallel branch %q", taskInstanceID, taskName)
		}
		if branch.Status != domain.BranchRunning {
			return fmt.Errorf("parallel branch task %q has already been completed", taskName)
		}

		e.mu.RLock()
		ch, ok := e.resumeChs[taskInstanceID]
		e.mu.RUnlock()
		if !ok {
			return fmt.Errorf("no active channel for parallel branch %q (instance %s)", taskName, taskInstanceID)
		}

		select {
		case ch <- resumePayload{Input: input, Override: override, ActorID: actorID, ActorGroups: actorGroups}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// --- Normal (non-parallel) mode. ---
	if state.CurrentTaskID != taskName {
		return fmt.Errorf("active task is %q, not %q", state.CurrentTaskID, taskName)
	}
	if state.ActiveTaskInstanceID != taskInstanceID {
		return fmt.Errorf("task instance %q not found", taskInstanceID)
	}

	e.mu.RLock()
	ch, ok := e.resumeChs[processID]
	e.mu.RUnlock()
	if !ok {
		return fmt.Errorf("no active execution loop for process %s", processID)
	}

	select {
	case ch <- resumePayload{Input: input, Override: override, ActorID: actorID, ActorGroups: actorGroups}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SignalProcess sends a named signal (domain event) to a process.
// If the process is paused at a HUMAN task the signal acts as task completion input;
// the signal_name is passed as the routing override so the LLM can factor it in.
func (e *Engine) SignalProcess(ctx context.Context, processID, signalName string, body map[string]any) error {
	state, err := e.repo.GetState(ctx, processID)
	if err != nil {
		return fmt.Errorf("get state: %w", err)
	}
	if state.Status != domain.StatusPaused && state.Status != domain.StatusRunning {
		return fmt.Errorf("process %s cannot receive signals in status %s", processID, state.Status)
	}

	e.mu.RLock()
	ch, ok := e.resumeChs[processID]
	e.mu.RUnlock()
	if !ok {
		return fmt.Errorf("no active execution loop for process %s — signal could not be delivered", processID)
	}

	if body == nil {
		body = make(map[string]any)
	}
	body["_signal"] = signalName

	select {
	case ch <- resumePayload{Input: body, Override: signalName}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// AddComment adds a comment to a task instance.
func (e *Engine) AddComment(ctx context.Context, processID, taskName, taskInstanceID, content, createdBy, group string) (*domain.TaskComment, error) {
	if e.commentStore == nil {
		return nil, fmt.Errorf("comment store not configured")
	}
	now := time.Now().UTC()
	c := &domain.TaskComment{
		ID:             domain.NewUUID(),
		ProcessID:      processID,
		TaskID:         taskName,
		TaskInstanceID: taskInstanceID,
		Content:        content,
		Group:          group,
		CreatedBy:      createdBy,
		UpdatedBy:      createdBy,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := e.commentStore.SaveComment(ctx, c); err != nil {
		return nil, err
	}
	if e.dispatcher != nil {
		evt := events.NewTaskEvent(events.OpAddComment, processID, taskInstanceID, map[string]any{
			"comment_id": c.ID,
			"task_name":  taskName,
			"content":    content,
			"created_at": c.CreatedAt,
		})
		e.dispatcher.DispatchTaskEvent(ctx, evt)
	}
	return c, nil
}

// ListComments returns all comments for a task instance.
func (e *Engine) ListComments(ctx context.Context, processID, taskInstanceID string) ([]*domain.TaskComment, error) {
	if e.commentStore == nil {
		return nil, fmt.Errorf("comment store not configured")
	}
	return e.commentStore.ListComments(ctx, processID, taskInstanceID)
}

// ListCommentsByProcess returns all comments persisted for the given process
// (instance), regardless of task instance. This is intended for HTTP GET
// endpoints that present process-scoped comments to clients.
func (e *Engine) ListCommentsByProcess(ctx context.Context, processID string) ([]*domain.TaskComment, error) {
	if e.commentStore == nil {
		return nil, fmt.Errorf("comment store not configured")
	}
	return e.commentStore.ListCommentsByProcess(ctx, processID)
}

// UpdateComment updates the content of an existing comment and records who made the change.
func (e *Engine) UpdateComment(ctx context.Context, commentID, content, updatedBy string) (*domain.TaskComment, error) {
	if e.commentStore == nil {
		return nil, fmt.Errorf("comment store not configured")
	}
	return e.commentStore.UpdateComment(ctx, commentID, content, updatedBy, time.Now().UTC())
}

// AddAttachment adds an attachment record to a task instance.
func (e *Engine) AddAttachment(ctx context.Context, processID, taskName, taskInstanceID, name, uri, createdBy, group string) (*domain.TaskAttachment, error) {
	if e.attachmentStore == nil {
		return nil, fmt.Errorf("attachment store not configured")
	}
	now := time.Now().UTC()
	a := &domain.TaskAttachment{
		ID:             domain.NewUUID(),
		ProcessID:      processID,
		TaskID:         taskName,
		TaskInstanceID: taskInstanceID,
		Name:           name,
		URI:            uri,
		Group:          group,
		CreatedBy:      createdBy,
		UpdatedBy:      createdBy,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := e.attachmentStore.SaveAttachment(ctx, a); err != nil {
		return nil, err
	}
	if e.dispatcher != nil {
		evt := events.NewTaskEvent(events.OpAddAttachment, processID, taskInstanceID, map[string]any{
			"attachment_id": a.ID,
			"task_name":     taskName,
			"name":          name,
			"uri":           uri,
			"created_at":    a.CreatedAt,
		})
		e.dispatcher.DispatchTaskEvent(ctx, evt)
	}
	return a, nil
}

// DeleteAttachment soft-deletes an attachment.
func (e *Engine) DeleteAttachment(ctx context.Context, attachmentID string) error {
	if e.attachmentStore == nil {
		return fmt.Errorf("attachment store not configured")
	}
	return e.attachmentStore.SoftDeleteAttachment(ctx, attachmentID)
}

// ListAttachments returns active (non-deleted) attachments for a task instance.
func (e *Engine) ListAttachments(ctx context.Context, processID, taskInstanceID string) ([]*domain.TaskAttachment, error) {
	if e.attachmentStore == nil {
		return nil, fmt.Errorf("attachment store not configured")
	}
	return e.attachmentStore.ListAttachments(ctx, processID, taskInstanceID)
}

// ListAttachmentsByProcess returns non-deleted attachments for the given
// process instance, ignoring task instance scoping. Used by HTTP GET
// endpoints when clients request process-scoped attachments.
func (e *Engine) ListAttachmentsByProcess(ctx context.Context, processID string) ([]*domain.TaskAttachment, error) {
	if e.attachmentStore == nil {
		return nil, fmt.Errorf("attachment store not configured")
	}
	return e.attachmentStore.ListAttachmentsByProcess(ctx, processID)
}
