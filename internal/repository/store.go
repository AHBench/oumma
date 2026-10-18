package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"oumma/internal/domain"
)

// Repository defines the persistence contract for workflow state.
type Repository interface {
	SaveState(ctx context.Context, state *domain.ExecutionState) error
	GetState(ctx context.Context, processID string) (*domain.ExecutionState, error)
	ListStates(ctx context.Context) ([]*domain.ExecutionState, error)
	RegisterDefinition(ctx context.Context, def *domain.ProcessDefinition) error
	GetDefinition(ctx context.Context, defID string) (*domain.ProcessDefinition, error)
	ListDefinitions(ctx context.Context) ([]*domain.ProcessDefinition, error)
	// Task history persistence (append-friendly storage for per-task bodies)
	SaveTaskHistory(ctx context.Context, h *domain.TaskHistory) error
	UpdateTaskHistory(ctx context.Context, id string, h *domain.TaskHistory) error
	ListTaskHistories(ctx context.Context, processID string) ([]*domain.TaskHistory, error)
}

// LLMLogStore defines the persistence contract for LLM call logs.
type LLMLogStore interface {
	SaveLLMLog(ctx context.Context, log *domain.LLMLog) error
	ListLLMLogs(ctx context.Context, processID string) ([]*domain.LLMLog, error)
	// SumTokenUsage totals the token counters of one instance's LLM calls.
	SumTokenUsage(ctx context.Context, processID string) (prompt, completion, calls int64, err error)
	// SumTokenUsageByDefinition totals tokens across every instance of a
	// definition (the designer view).
	SumTokenUsageByDefinition(ctx context.Context, definitionID string) (prompt, completion, calls int64, err error)
	// SumTokenUsageByUsernameMonth totals a user's tokens since monthStart
	// (normal-user monthly quota).
	SumTokenUsageByUsernameMonth(ctx context.Context, username string, monthStart time.Time) (prompt, completion, calls int64, err error)
}

// CheckpointStore defines the persistence contract for process execution checkpoints.
type CheckpointStore interface {
	SaveCheckpoint(ctx context.Context, cp *domain.ProcessCheckpoint) error
	GetCheckpoint(ctx context.Context, processID string) (*domain.ProcessCheckpoint, error)
	DeleteCheckpoint(ctx context.Context, processID string) error
}

// TaskCommentStore defines the persistence contract for user task comments.
type TaskCommentStore interface {
	SaveComment(ctx context.Context, c *domain.TaskComment) error
	// ListComments returns comments for a specific task instance.
	ListComments(ctx context.Context, processID, taskInstanceID string) ([]*domain.TaskComment, error)
	// ListCommentsByProcess returns all comments for a given process (instance),
	// ignoring task instance IDs. This is used by the HTTP GET endpoints when
	// clients ask for process-scoped comments.
	ListCommentsByProcess(ctx context.Context, processID string) ([]*domain.TaskComment, error)
	UpdateComment(ctx context.Context, commentID, content, updatedBy string, updatedAt time.Time) (*domain.TaskComment, error)
}

// TaskAttachmentStore defines the persistence contract for user task attachments.
type TaskAttachmentStore interface {
	SaveAttachment(ctx context.Context, a *domain.TaskAttachment) error
	GetAttachment(ctx context.Context, attachmentID string) (*domain.TaskAttachment, error)
	// ListAttachments returns attachments for a specific task instance.
	ListAttachments(ctx context.Context, processID, taskInstanceID string) ([]*domain.TaskAttachment, error)
	// ListAttachmentsByProcess returns all non-deleted attachments for a given
	// process (instance), ignoring task instance IDs.
	ListAttachmentsByProcess(ctx context.Context, processID string) ([]*domain.TaskAttachment, error)
	SoftDeleteAttachment(ctx context.Context, attachmentID string) error
}

// TrackStore persists the ordered routing history of a process execution.
// One step is appended for every LLM routing decision (from-task → to-task).
type TrackStore interface {
	// AppendTrackStep appends a new step; the store assigns StepOrder.
	AppendTrackStep(ctx context.Context, step *domain.ProcessTrackStep) error
	// ListTrackSteps returns all steps for the process ordered by StepOrder ASC.
	ListTrackSteps(ctx context.Context, processID string) ([]*domain.ProcessTrackStep, error)
	// UpdateWaitingHumanStep finds the most recent WAITING_HUMAN step for
	// (processID, fromTaskID) and promotes it to ROUTED with the given
	// toTaskID and reasoning. This collapses the two-step
	// WAITING_HUMAN → ROUTED sequence into a single updated record.
	UpdateWaitingHumanStep(ctx context.Context, processID, fromTaskID, toTaskID, reasoning string) error
}

// InMemoryStore is a thread-safe, in-memory implementation of Repository, LLMLogStore, CheckpointStore,
// TaskCommentStore, TaskAttachmentStore, and TrackStore.
type InMemoryStore struct {
	mu          sync.RWMutex
	states      map[string][]byte // JSON-serialized for deep copy
	definitions map[string]*domain.ProcessDefinition
	logs        []*domain.LLMLog
	checkpoints map[string]*domain.ProcessCheckpoint
	comments    []*domain.TaskComment
	attachments []*domain.TaskAttachment
	trackSteps  []*domain.ProcessTrackStep
	histories   []*domain.TaskHistory
	designs     *memoryDesigns
}

func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		states:      make(map[string][]byte),
		definitions: make(map[string]*domain.ProcessDefinition),
		checkpoints: make(map[string]*domain.ProcessCheckpoint),
		histories:   make([]*domain.TaskHistory, 0),
	}
}

func (s *InMemoryStore) SaveState(_ context.Context, state *domain.ExecutionState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	s.mu.Lock()
	s.states[state.ProcessID] = data
	s.mu.Unlock()
	return nil
}

func (s *InMemoryStore) GetState(_ context.Context, processID string) (*domain.ExecutionState, error) {
	s.mu.RLock()
	data, ok := s.states[processID]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("process %s not found", processID)
	}
	var state domain.ExecutionState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("unmarshal state: %w", err)
	}
	return &state, nil
}

func (s *InMemoryStore) ListStates(_ context.Context) ([]*domain.ExecutionState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*domain.ExecutionState, 0, len(s.states))
	for _, data := range s.states {
		var st domain.ExecutionState
		if err := json.Unmarshal(data, &st); err != nil {
			continue
		}
		result = append(result, &st)
	}
	return result, nil
}

func (s *InMemoryStore) RegisterDefinition(_ context.Context, def *domain.ProcessDefinition) error {
	s.mu.Lock()
	s.definitions[def.ID] = def
	s.mu.Unlock()
	return nil
}

func (s *InMemoryStore) GetDefinition(_ context.Context, defID string) (*domain.ProcessDefinition, error) {
	s.mu.RLock()
	def, ok := s.definitions[defID]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("definition %s not found", defID)
	}
	return def, nil
}

func (s *InMemoryStore) ListDefinitions(_ context.Context) ([]*domain.ProcessDefinition, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*domain.ProcessDefinition, 0, len(s.definitions))
	for _, def := range s.definitions {
		result = append(result, def)
	}
	return result, nil
}

func (s *InMemoryStore) SaveLLMLog(_ context.Context, log *domain.LLMLog) error {
	s.mu.Lock()
	s.logs = append(s.logs, log)
	s.mu.Unlock()
	return nil
}

func (s *InMemoryStore) ListLLMLogs(_ context.Context, processID string) ([]*domain.LLMLog, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*domain.LLMLog
	for _, l := range s.logs {
		if l.ProcessID == processID {
			result = append(result, l)
		}
	}
	return result, nil
}

func (s *InMemoryStore) SumTokenUsage(_ context.Context, processID string) (int64, int64, int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var p, c, n int64
	for _, l := range s.logs {
		if l.ProcessID == processID {
			p += int64(l.PromptTokens)
			c += int64(l.CompletionTokens)
			n++
		}
	}
	return p, c, n, nil
}

func (s *InMemoryStore) SumTokenUsageByDefinition(_ context.Context, definitionID string) (int64, int64, int64, error) {
	s.mu.RLock()
	// Same semantics as the Postgres query: a log belongs to the definition
	// when its process_id is one of the definition's instance ids OR when
	// it was recorded against the definition itself (no active dossier).
	instanceIDs := map[string]bool{}
	for id, data := range s.states {
		var st domain.ExecutionState
		if err := json.Unmarshal(data, &st); err == nil && st.DefinitionID == definitionID {
			instanceIDs[id] = true
		}
	}
	var p, c, n int64
	for _, l := range s.logs {
		if l.ProcessID == definitionID || instanceIDs[l.ProcessID] {
			p += int64(l.PromptTokens)
			c += int64(l.CompletionTokens)
			n++
		}
	}
	s.mu.RUnlock()
	return p, c, n, nil
}

func (s *InMemoryStore) SumTokenUsageByUsernameMonth(_ context.Context, username string, monthStart time.Time) (int64, int64, int64, error) {
	s.mu.RLock()
	var p, c, n int64
	for _, l := range s.logs {
		if l.Username == username && !l.CreatedAt.Before(monthStart) {
			p += int64(l.PromptTokens)
			c += int64(l.CompletionTokens)
			n++
		}
	}
	s.mu.RUnlock()
	return p, c, n, nil
}

// --- CheckpointStore implementation ---

func (s *InMemoryStore) SaveCheckpoint(_ context.Context, cp *domain.ProcessCheckpoint) error {
	s.mu.Lock()
	s.checkpoints[cp.ProcessID] = cp
	s.mu.Unlock()
	return nil
}

func (s *InMemoryStore) GetCheckpoint(_ context.Context, processID string) (*domain.ProcessCheckpoint, error) {
	s.mu.RLock()
	cp, ok := s.checkpoints[processID]
	s.mu.RUnlock()
	if !ok {
		return nil, nil
	}
	return cp, nil
}

func (s *InMemoryStore) DeleteCheckpoint(_ context.Context, processID string) error {
	s.mu.Lock()
	delete(s.checkpoints, processID)
	s.mu.Unlock()
	return nil
}

// --- TaskCommentStore implementation ---

func (s *InMemoryStore) SaveComment(_ context.Context, c *domain.TaskComment) error {
	s.mu.Lock()
	s.comments = append(s.comments, c)
	s.mu.Unlock()
	return nil
}

func (s *InMemoryStore) ListComments(_ context.Context, processID, taskInstanceID string) ([]*domain.TaskComment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*domain.TaskComment
	for _, c := range s.comments {
		if c.ProcessID == processID && c.TaskInstanceID == taskInstanceID {
			result = append(result, c)
		}
	}
	return result, nil
}

func (s *InMemoryStore) ListCommentsByProcess(_ context.Context, processID string) ([]*domain.TaskComment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*domain.TaskComment
	for _, c := range s.comments {
		if c.ProcessID == processID {
			result = append(result, c)
		}
	}
	return result, nil
}

func (s *InMemoryStore) UpdateComment(_ context.Context, commentID, content, updatedBy string, updatedAt time.Time) (*domain.TaskComment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.comments {
		if c.ID == commentID {
			c.Content = content
			c.UpdatedBy = updatedBy
			c.UpdatedAt = updatedAt
			return c, nil
		}
	}
	return nil, fmt.Errorf("comment %s not found", commentID)
}

// --- TaskAttachmentStore implementation ---

func (s *InMemoryStore) SaveAttachment(_ context.Context, a *domain.TaskAttachment) error {
	s.mu.Lock()
	s.attachments = append(s.attachments, a)
	s.mu.Unlock()
	return nil
}

func (s *InMemoryStore) GetAttachment(_ context.Context, attachmentID string) (*domain.TaskAttachment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.attachments {
		if a.ID == attachmentID {
			return a, nil
		}
	}
	return nil, fmt.Errorf("attachment %s not found", attachmentID)
}

func (s *InMemoryStore) ListAttachments(_ context.Context, processID, taskInstanceID string) ([]*domain.TaskAttachment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*domain.TaskAttachment
	for _, a := range s.attachments {
		if a.ProcessID == processID && a.TaskInstanceID == taskInstanceID && !a.Deleted {
			result = append(result, a)
		}
	}
	return result, nil
}

func (s *InMemoryStore) ListAttachmentsByProcess(_ context.Context, processID string) ([]*domain.TaskAttachment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*domain.TaskAttachment
	for _, a := range s.attachments {
		if a.ProcessID == processID && !a.Deleted {
			result = append(result, a)
		}
	}
	return result, nil
}

// --- TaskHistoryStore (InMemory) ---

func (s *InMemoryStore) SaveTaskHistory(_ context.Context, h *domain.TaskHistory) error {
	s.mu.Lock()
	if h.ID == "" {
		h.ID = domain.NewUUID()
	}
	now := time.Now().UTC()
	if h.CreatedAt.IsZero() {
		h.CreatedAt = now
	}
	h.UpdatedAt = now
	s.histories = append(s.histories, h)
	s.mu.Unlock()
	return nil
}

func (s *InMemoryStore) UpdateTaskHistory(_ context.Context, id string, h *domain.TaskHistory) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ex := range s.histories {
		if ex.ID == id {
			if h.TaskInstanceID != "" {
				ex.TaskInstanceID = h.TaskInstanceID
			}
			if h.TaskOutput != nil {
				ex.TaskOutput = h.TaskOutput
			}
			if h.State != "" {
				ex.State = h.State
			}
			if h.CompletedAt != nil {
				ex.CompletedAt = h.CompletedAt
			}
			ex.UpdatedAt = time.Now().UTC()
			return nil
		}
	}
	return fmt.Errorf("task history %s not found", id)
}

func (s *InMemoryStore) ListTaskHistories(_ context.Context, processID string) ([]*domain.TaskHistory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*domain.TaskHistory
	for _, h := range s.histories {
		if h.ProcessID == processID {
			out = append(out, h)
		}
	}
	// naive ordering by chronological_order
	return out, nil
}

func (s *InMemoryStore) SoftDeleteAttachment(_ context.Context, attachmentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.attachments {
		if a.ID == attachmentID {
			if a.Deleted {
				return fmt.Errorf("attachment %s already deleted", attachmentID)
			}
			now := time.Now().UTC()
			a.Deleted = true
			a.DeletedAt = &now
			return nil
		}
	}
	return fmt.Errorf("attachment %s not found", attachmentID)
}

// --- TrackStore implementation ---

func (s *InMemoryStore) AppendTrackStep(_ context.Context, step *domain.ProcessTrackStep) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	order := 0
	for _, ts := range s.trackSteps {
		if ts.ProcessID == step.ProcessID {
			order++
		}
	}
	step.StepOrder = order
	s.trackSteps = append(s.trackSteps, step)
	return nil
}

func (s *InMemoryStore) ListTrackSteps(_ context.Context, processID string) ([]*domain.ProcessTrackStep, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*domain.ProcessTrackStep
	for _, ts := range s.trackSteps {
		if ts.ProcessID == processID {
			result = append(result, ts)
		}
	}
	return result, nil
}

func (s *InMemoryStore) UpdateWaitingHumanStep(_ context.Context, processID, fromTaskID, toTaskID, reasoning string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Walk backwards to find the most recent WAITING_HUMAN step for this task.
	for i := len(s.trackSteps) - 1; i >= 0; i-- {
		ts := s.trackSteps[i]
		if ts.ProcessID == processID && ts.FromTaskID == fromTaskID && ts.Status == domain.TrackStatusWaitingHuman {
			ts.ToTaskID = toTaskID
			ts.Status = domain.TrackStatusRouted
			ts.Reasoning = reasoning
			return nil
		}
	}
	return fmt.Errorf("no WAITING_HUMAN step found for process %s task %s", processID, fromTaskID)
}
