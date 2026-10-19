package processconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"oumma/internal/ai"
	"oumma/internal/domain"
	"oumma/internal/mcptools"
	"oumma/internal/handler"
	"oumma/internal/repository"
)

// ---------------------------------------------------------------------------
// Process designer endpoints (Designer group only):
//
//	POST /api/management/design/save     — validate + save a NEW design
//	                                     version (postgres JSON). The YAML
//	                                     file is NOT touched.
//	POST /api/management/design/deploy   — make a version active: new
//	                                     instances run on it, running
//	                                     instances keep their pinned version.
//	GET  /api/management/design/versions?processId=
//	GET  /api/management/design/{processId}?version=
//	POST /api/management/design/ask      — LLM chat for the designer. The
//	                                     answer may contain a ```tool fenced
//	                                     block ({"name","args"}); the CLIENT
//	                                     executes the tool (with user
//	                                     confirmation) and feeds the result
//	                                     back as history.
// ---------------------------------------------------------------------------

// LLMAsker is the subset of *ai.OpenAIClient used by the designer chat.
type LLMAsker interface {
	Ask(ctx context.Context, req *ai.AskRequest) (string, error)
}

// DesignRepo is the versioned design store (repository.DesignStore).
type DesignRepo interface {
	SaveDesign(ctx context.Context, processID string, design []byte, createdBy string) (int, error)
	UpsertPendingDesign(ctx context.Context, processID string, design []byte, createdBy string) error
	GetPendingDesign(ctx context.Context, processID string) (*repository.DesignVersion, error)
	DeletePendingDesign(ctx context.Context, processID string) error
	SetActiveDesign(ctx context.Context, processID string, version int) error
	SetActiveDesignSemver(ctx context.Context, processID string, version int, semver string) error
	GetActiveDesign(ctx context.Context, processID string) ([]byte, int, error)
	GetDesign(ctx context.Context, processID string, version int) (*repository.DesignVersion, error)
	ListDesigns(ctx context.Context, processID string) ([]*repository.DesignVersion, error)
}

// DefinitionRegistrar mirrors the subset of repository.Repository used by
// the design service: register deployed designs, and read registered
// definitions so engine-only templates (no app: section file) still get
// full context in the designer chat.
type DefinitionRegistrar interface {
	RegisterDefinition(ctx context.Context, def *domain.ProcessDefinition) error
	GetDefinition(ctx context.Context, processID string) (*domain.ProcessDefinition, error)
}

// StateLister exposes the executed instances (used by the delete guard:
// a process can be deleted only when no instance was ever executed in it).
type StateLister interface {
	ListStates(ctx context.Context) ([]*domain.ExecutionState, error)
}

type DesignService struct {
	store   *Store
	designs DesignRepo
	reg     DefinitionRegistrar
	llm     LLMAsker
	log     *slog.Logger
	states  StateLister
	// Snapshot ordering: design mutations fire async pending-snapshots;
	// the sequence number taken at mutation time + the mutex make the
	// pending row converge on the LATEST mutation (a stale, out-of-order
	// snapshot is dropped instead of resurrecting deleted steps).
	snapMu   sync.Mutex
	snapSeq  int64
	lastSnap map[string]int64
	// reviewStore, when set, keeps the AI design-review exchanges in the
	// process's design history and feeds recent reviews back into the
	// reviewer context.
	reviewStore repository.DesignReviewStore
	// quotaGuard, when set, refuses LLM-consuming design calls (ask) with a
	// 429 when the caller's monthly token budget is exhausted.
	quotaGuard func(w http.ResponseWriter, r *http.Request, claims *handler.AuthClaims) bool
}

// SetQuotaGuard wires the token-quota enforcement for the design chat.
func (d *DesignService) SetQuotaGuard(fn func(w http.ResponseWriter, r *http.Request, claims *handler.AuthClaims) bool) {
	d.quotaGuard = fn
}

// SetReviewStore wires the design-history store for AI reviews.
func (d *DesignService) SetReviewStore(rs repository.DesignReviewStore) {
	d.reviewStore = rs
}

func NewDesignService(store *Store, designs DesignRepo, reg DefinitionRegistrar, llm LLMAsker, log *slog.Logger) *DesignService {
	return &DesignService{store: store, designs: designs, reg: reg, llm: llm, log: log, lastSnap: make(map[string]int64)}
}

// SetStateLister wires the instance store for the delete guard.
func (d *DesignService) SetStateLister(sl StateLister) { d.states = sl }

func (d *DesignService) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/management/design/save", d.designGuard(d.saveDesign))
	mux.HandleFunc("POST /api/management/design/deploy", d.designGuard(d.deployDesign))
	mux.HandleFunc("POST /api/management/design/discard", d.designGuard(d.discardDesign))
	mux.HandleFunc("GET /api/management/design/versions", d.designGuard(d.listDesigns))
	mux.HandleFunc("GET /api/management/design/{processId}", d.designGuard(d.getDesign))
	mux.HandleFunc("GET /api/management/design/{processId}/ai-reviews", d.designGuard(d.listAiReviews))
	mux.HandleFunc("POST /api/management/design/ask", d.designGuard(d.askDesign))
	mux.HandleFunc("POST /api/management/design/{processId}/sync-roles", d.designGuard(d.syncRoles))
	mux.HandleFunc("POST /api/management/design/{processId}/sync-forms", d.designGuard(d.syncForms))
	mux.HandleFunc("POST /api/management/design/{processId}/rename-task", d.designGuard(d.renameTask))
	mux.HandleFunc("GET /api/management/design/tools", d.designGuard(d.toolCatalog))
	mux.HandleFunc("POST /api/management/design/{processId}/add-tool-task", d.designGuard(d.addToolTask))
	mux.HandleFunc("POST /api/management/design/{processId}/add-system-task", d.designGuard(d.addSystemTask))
	mux.HandleFunc("POST /api/management/design/{processId}/set-task-config", d.designGuard(d.setTaskConfig))
	mux.HandleFunc("POST /api/management/design/{processId}/add-task", d.designGuard(d.addHumanTask))
	mux.HandleFunc("POST /api/management/design/{processId}/rename", d.designGuard(d.renameProcess))
	mux.HandleFunc("POST /api/management/design/{processId}/delete", d.designGuard(d.deleteProcess))
	mux.HandleFunc("POST /api/management/design/{processId}/add-edge", d.designGuard(d.addEdge))
	mux.HandleFunc("POST /api/management/design/{processId}/update-edge", d.designGuard(d.updateEdge))
	mux.HandleFunc("POST /api/management/design/{processId}/delete-task", d.designGuard(d.deleteTask))
	mux.HandleFunc("POST /api/management/design/{processId}/delete-edge", d.designGuard(d.deleteEdge))
	mux.HandleFunc("POST /api/management/design/{processId}/update-task", d.designGuard(d.updateTask))
}

// ---------------------------------------------------------------------------
// POST /api/management/design/{processId}/rename-task  (Designer)
//
// Renames an engine task and cascades the rename into the app section:
// form steps (step_id) and role task references (task_name). The engine
// section edit (task_id, topology from/to/join_from, start_task_id) and the
// app cascade land in the document together, so the single pending version
// carries the complete change. Deploying it activates the rename; running
// instances keep their pinned definition.
// ---------------------------------------------------------------------------

func (d *DesignService) renameTask(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("processId")
	var req struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	req.From = strings.TrimSpace(req.From)
	req.To = strings.TrimSpace(req.To)
	if req.From == "" || req.To == "" {
		writeErr(w, http.StatusBadRequest, "'from' and 'to' task names are required")
		return
	}
	if req.From == req.To {
		writeErr(w, http.StatusBadRequest, "'from' and 'to' must differ")
		return
	}
	if _, ok := d.store.Get(processID); !ok {
		writeErr(w, http.StatusNotFound, "Process not found in the management store: "+processID)
		return
	}
	// Validate against the LIVE document (it carries un-deployed pending
	// renames), not the deployed registry definition — otherwise a second
	// rename on top of a pending one would 404 on the not-yet-deployed name.
	liveDoc, derr := d.store.DocumentJSON(processID)
	if derr != nil {
		writeErr(w, http.StatusInternalServerError, "read process document: "+derr.Error())
		return
	}
	liveIDs := taskIDsFromDoc(liveDoc)
	if !liveIDs[req.From] {
		writeErr(w, http.StatusNotFound, "Task not found in the engine design: "+req.From)
		return
	}
	if liveIDs[req.To] {
		writeErr(w, http.StatusConflict, "A task named "+req.To+" already exists")
		return
	}
	from, to := req.From, req.To
		err := d.store.Update(processID, func(doc map[string]any) (map[string]any, error) {
		// 1. Engine section: task id + every topology reference.
		if tasks, ok := doc["tasks"].(map[string]any); ok {
			// MAP format (task name = key)
			if tm, ok := tasks[from].(map[string]any); ok {
				if _, has := tasks[to]; !has {
					delete(tasks, from)
					tasks[to] = tm
					if _, hasID := tm["task_id"]; hasID {
						tm["task_id"] = to
					}
					if _, hasID := tm["id"]; hasID {
						tm["id"] = to
					}
				}
			}
		} else if list, ok := doc["tasks"].([]any); ok {
			for _, t := range list {
				tm, ok := t.(map[string]any)
				if !ok {
					continue
				}
				if strEq(tm["task_id"], from) {
					tm["task_id"] = to
					if _, has := tm["id"]; has {
						tm["id"] = to
					}
					if _, has := tm["task"]; has {
						tm["task"] = to
					}
				}
			}
		}
		if edges, ok := doc["topology"].([]any); ok {
			for _, e := range edges {
				em, ok := e.(map[string]any)
				if !ok {
					continue
				}
				if strEq(em["from"], from) {
					em["from"] = to
				}
				if strEq(em["to"], from) {
					em["to"] = to
				}
				if jf, ok := em["join_from"].([]any); ok {
					for i, j := range jf {
						if strEq(j, from) {
							jf[i] = to
						}
					}
				}
			}
		}
		if strEq(doc["start_task_id"], from) {
			doc["start_task_id"] = to
		}
		// 2. App section: form steps + role task references.
		if appM, ok := doc["app"].(map[string]any); ok {
			if forms, ok := appM["forms"].(map[string]any); ok {
				if steps, ok := forms["steps"].([]any); ok {
					for _, st := range steps {
						sm, ok := st.(map[string]any)
						if ok && strEq(sm["step_id"], from) {
							sm["step_id"] = to
						}
					}
				}
			}
			if roles, ok := appM["roles"].([]any); ok {
				for _, rl := range roles {
					rm, ok := rl.(map[string]any)
					if !ok {
						continue
					}
					if rts, ok := rm["role_tasks"].([]any); ok {
						for _, rt := range rts {
							rtm, ok := rt.(map[string]any)
							if ok && strEq(rtm["task_name"], from) {
								rtm["task_name"] = to
							}
						}
					}
				}
			}
		}
		return doc, nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "rename task: "+err.Error())
		return
	}
	d.log.Info("task renamed (pending version)", "processId", processID, "from", from, "to", to)
	writeJSON(w, http.StatusOK, map[string]any{
		"renamed":  true,
		"processId": processID,
		"from":     from,
		"to":       to,
		"message":  "Task renamed; the change is in the pending design version — deploy it to activate.",
	})
}

// toolCatalog returns the MCP-like tool set the designer can add as
// system tasks (Gmail, Telegram, WhatsApp, generic HTTP).
func (d *DesignService) toolCatalog(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"tools": mcptools.Catalog()})
}

// addToolTask appends a SYSTEM task that executes a catalog tool with the
// given configuration and input mapping (tool parameter <- context field
// key). Optionally wires an edge from `after_task` to the new task.
func (d *DesignService) addToolTask(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("processId")
	var req struct {
		Name       string            `json:"name"`
		Tool       string            `json:"tool"`
		ToolConfig map[string]string `json:"tool_config"`
		ToolInputs map[string]string `json:"tool_inputs"`
		Group      string            `json:"group"`
		AfterTask  string            `json:"after_task"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Tool = strings.TrimSpace(req.Tool)
	if req.Name == "" || req.Tool == "" {
		writeErr(w, http.StatusBadRequest, "'name' and 'tool' are required")
		return
	}
	if _, ok := mcptools.Lookup(req.Tool); !ok {
		writeErr(w, http.StatusBadRequest, "Unknown tool: "+req.Tool)
		return
	}
	if err := mcptools.ValidateConfig(req.Tool, req.ToolConfig); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, ok := d.store.Get(processID); !ok {
		writeErr(w, http.StatusNotFound, "Process not found in the management store: "+processID)
		return
	}
	err := d.store.Update(processID, func(doc map[string]any) (map[string]any, error) {
		// The engine section stores tasks either as a MAP (task name = key,
		// on-disk format) or a LIST (task_id field, save-design format).
		var tasksMap map[string]any
		var tasksList []any
		switch v := doc["tasks"].(type) {
		case map[string]any:
			tasksMap = v
		case []any:
			tasksList = v
		}
		taskExists := func(name string) bool {
			if tasksMap != nil {
				if _, ok := tasksMap[name]; ok {
					return true
				}
			}
			for _, t := range tasksList {
				if tm, ok := t.(map[string]any); ok && (strEq(tm["task_id"], name) || strEq(tm["id"], name)) {
					return true
				}
			}
			return false
		}
		if taskExists(req.Name) {
			return nil, fmt.Errorf("a task named %q already exists", req.Name)
		}
		if req.AfterTask != "" && !taskExists(req.AfterTask) {
			return nil, fmt.Errorf("after_task not found: %q", req.AfterTask)
		}
		cfg := map[string]any{"tool": req.Tool}
		if len(req.ToolConfig) > 0 {
			cfg["tool_config"] = req.ToolConfig
		}
		if len(req.ToolInputs) > 0 {
			cfg["tool_inputs"] = req.ToolInputs
		}
		newTask := map[string]any{
			"task_id":     req.Name,
			"id":          req.Name,
			"type":        "SYSTEM",
			"description": req.Name,
			"config":      cfg,
		}
		if g := strings.TrimSpace(req.Group); g != "" {
			newTask["group"] = g
		}
		if tasksMap != nil {
			tasksMap[req.Name] = newTask
			doc["tasks"] = tasksMap
		} else {
			if tasksList == nil {
				tasksList = []any{}
			}
			doc["tasks"] = append(tasksList, newTask)
		}
		if req.AfterTask != "" {
			edges, _ := doc["topology"].([]any)
			edges = append(edges, map[string]any{"from": req.AfterTask, "to": req.Name})
			doc["topology"] = edges
		}
		return doc, nil
	})
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	d.log.Info("tool task added (pending version)", "processId", processID, "task", req.Name, "tool", req.Tool)
	writeJSON(w, http.StatusCreated, map[string]any{
		"added":   true,
		"processId": processID,
		"task":    req.Name,
		"tool":    req.Tool,
		"message": "Tool task added to the pending design version — deploy it to activate.",
	})
}

// addSystemTask appends a plain SYSTEM task (HTTP executor) to the pending
// design. The designer's chat may suggest a configuration in the prompt
// (url/method/headers) — or omit it, in which case the task is created
// WITHOUT config and the user completes it later in the Tools tab
// (set-task-config). Without a config the task must not be deployed/used:
// the engine refuses to execute a config-less system task.
func (d *DesignService) addSystemTask(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("processId")
	var req struct {
		Name        string            `json:"name"`
		Description string            `json:"description"`
		Group       string            `json:"group"`
		AfterTask   string            `json:"after_task"`
		URL         string            `json:"url"`
		Method      string            `json:"method"`
		Headers     map[string]string `json:"headers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "'name' is required")
		return
	}
	if _, ok := d.store.Get(processID); !ok {
		writeErr(w, http.StatusNotFound, "Process not found in the management store: " + processID)
		return
	}
	cfg := map[string]any{}
	if u := strings.TrimSpace(req.URL); u != "" {
		cfg["url"] = u
		m := strings.ToUpper(strings.TrimSpace(req.Method))
		if m == "" {
			m = "POST"
		}
		cfg["method"] = m
		if len(req.Headers) > 0 {
			cfg["headers"] = req.Headers
		}
	}
	hasCfg := len(cfg) > 0
	err := d.store.Update(processID, func(doc map[string]any) (map[string]any, error) {
		if docTaskExists(doc, req.Name) {
			return nil, fmt.Errorf("a task named %q already exists", req.Name)
		}
		if req.AfterTask != "" && !docTaskExists(doc, req.AfterTask) {
			return nil, fmt.Errorf("after_task not found: %q", req.AfterTask)
		}
		newTask := map[string]any{
			"task_id":     req.Name,
			"id":          req.Name,
			"type":        "SYSTEM",
			"description": firstNonEmpty(req.Description, req.Name),
		}
		if g := strings.TrimSpace(req.Group); g != "" {
			newTask["group"] = g
		}
		if hasCfg {
			newTask["config"] = cfg
		}
		docAddTask(doc, newTask)
		if req.AfterTask != "" {
			edges, _ := doc["topology"].([]any)
			edges = append(edges, map[string]any{"from": req.AfterTask, "to": req.Name})
			doc["topology"] = edges
		}
		return doc, nil
	})
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	msg := "System task added to the pending design version — deploy it to activate."
	if !hasCfg {
		msg = "System task added WITHOUT configuration (configure it in the Tools tab before deploying)."
	}
	d.log.Info("system task added (pending version)", "processId", processID, "task", req.Name, "configured", hasCfg)
	writeJSON(w, http.StatusCreated, map[string]any{
		"added":     true,
		"processId": processID,
		"task":      req.Name,
		"configured": hasCfg,
		"message":   msg,
	})
}

// setTaskConfig sets/updates the HTTP configuration of an existing system
// task (the "configure later" path from the Tools tab).
func (d *DesignService) setTaskConfig(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("processId")
	var req struct {
		Task    string            `json:"task"`
		URL     string            `json:"url"`
		Method  string            `json:"method"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	req.Task = strings.TrimSpace(req.Task)
	if req.Task == "" {
		writeErr(w, http.StatusBadRequest, "'task' is required")
		return
	}
	if _, ok := d.store.Get(processID); !ok {
		writeErr(w, http.StatusNotFound, "Process not found in the management store: " + processID)
		return
	}
	err := d.store.Update(processID, func(doc map[string]any) (map[string]any, error) {
		if !docTaskExists(doc, req.Task) {
			return nil, fmt.Errorf("task not found: %q", req.Task)
		}
		m := map[string]any{}
		if u := strings.TrimSpace(req.URL); u != "" {
			m["url"] = u
			mm := strings.ToUpper(strings.TrimSpace(req.Method))
			if mm == "" {
				mm = "POST"
			}
			m["method"] = mm
			if len(req.Headers) > 0 {
				m["headers"] = req.Headers
			}
		}
		changed := docSetTaskConfig(doc, req.Task, m)
		if !changed {
			return nil, fmt.Errorf("task %q was not found", req.Task)
		}
		return doc, nil
	})
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	d.log.Info("system task config updated (pending version)", "processId", processID, "task", req.Task)
	writeJSON(w, http.StatusOK, map[string]any{
		"updated": true,
		"processId": processID,
		"task":      req.Task,
		"message": "System task configuration updated in the pending design version — deploy it to activate.",
	})
}

// renameProcess changes the PROCESS ID itself — allowed only while the
// project has NEVER been deployed (no non-pending design version). Once
// a version is deployed, instances, roles, forms and usage attribution
// are pinned to the id, so it becomes immutable.
func (d *DesignService) renameProcess(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("processId")
	var req struct {
		NewProcessID string `json:"newProcessId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	newID := strings.ToLower(strings.TrimSpace(req.NewProcessID))
	if newID == "" || !processIDPattern.MatchString(newID) {
		writeErr(w, http.StatusBadRequest, "newProcessId is required (lowercase letters, digits, hyphens)")
		return
	}
	if newID == processID {
		writeErr(w, http.StatusBadRequest, "new id equals the current id")
		return
	}
	if _, ok := d.store.Get(processID); !ok {
		writeErr(w, http.StatusNotFound, "Process not found: "+processID)
		return
	}
	if _, ok := d.store.Get(newID); ok {
		writeErr(w, http.StatusConflict, "A process with the id "+newID+" already exists")
		return
	}
	// Never-deployed check: any non-pending version blocks the rename.
	versions, err := d.designs.ListDesigns(r.Context(), processID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read design versions: "+err.Error())
		return
	}
	for _, v := range versions {
		if v.Status != "pending" {
			writeErr(w, http.StatusConflict, "Process already deployed (v"+itoa(v.Version)+") — its id can no longer be changed")
			return
		}
	}
	if err := d.store.RenameProcess(processID, newID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Move the pending version to the new id (the on-changed snapshot
	// recreates it for the new id right after the rename).
	if oldPending, err := d.designs.GetPendingDesign(r.Context(), processID); err == nil && oldPending != nil {
		_ = d.designs.DeletePendingDesign(r.Context(), processID)
	}
	d.log.Info("process id renamed (never-deployed project)", "from", processID, "to", newID)
	writeJSON(w, http.StatusOK, map[string]any{
		"renamed":    true,
		"processId":  processID,
		"newProcessId": newID,
		"message":    "Process id changed while the project was undeployed. Continue building; the user deploys the pending version under the new id.",
	})
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// deleteProcess removes a process when NO instance was ever executed in
// it (a brand-new project — undeployed, or deployed but never launched):
// its YAML file, the in-memory entry and its pending design version.
// Processes with executed instances are rejected — their id is pinned by
// instances, forms, roles and usage attribution.
func (d *DesignService) deleteProcess(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("processId")
	if _, ok := d.store.Get(processID); !ok {
		writeErr(w, http.StatusNotFound, "Process not found: "+processID)
		return
	}
	// Guard: no instance may have ever been executed in this process.
	if d.states != nil {
		sts, err := d.states.ListStates(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "read instances: "+err.Error())
			return
		}
		for _, st := range sts {
			if st != nil && st.DefinitionID == processID {
				writeErr(w, http.StatusConflict, "Process has executed instances — it can no longer be deleted")
				return
			}
		}
	} else {
		// No state lister wired (tests/embedded): fall back to the
		// stricter never-deployed rule.
		versions, err := d.designs.ListDesigns(r.Context(), processID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "read design versions: "+err.Error())
			return
		}
		for _, v := range versions {
			if v.Status != "pending" {
				writeErr(w, http.StatusConflict, "Process already deployed (v"+itoa(v.Version)+") — only never-deployed projects can be deleted")
				return
			}
		}
	}
	if err := d.store.DeleteProcess(processID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if oldPending, perr := d.designs.GetPendingDesign(r.Context(), processID); perr == nil && oldPending != nil {
		_ = d.designs.DeletePendingDesign(r.Context(), processID)
	}
	d.log.Info("process deleted (no executed instances)", "processId", processID)
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted":   true,
		"processId": processID,
		"message":   "Process deleted — no instance was ever executed in it.",
	})
}

var processIDPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func itoa(v int) string { return strconv.Itoa(v) }

// addHumanTask appends a HUMAN task to the pending design (Diagram tab via
// chat). Requires a group (the task's role).
func (d *DesignService) addHumanTask(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("processId")
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Group       string `json:"group"`
		AfterTask   string `json:"after_task"`
		Type        string `json:"type"` // HUMAN (default) | START | END
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Group = strings.TrimSpace(req.Group)
	req.Type = strings.ToUpper(strings.TrimSpace(req.Type))
	if req.Type == "" {
		req.Type = "HUMAN"
	}
	switch req.Type {
	case "HUMAN":
		if req.Name == "" || req.Group == "" {
			writeErr(w, http.StatusBadRequest, "'name' and 'group' are required for a human task")
			return
		}
	case "START", "END":
		if req.Name == "" {
			writeErr(w, http.StatusBadRequest, "'name' is required")
			return
		}
	default:
		// Visual gateway nodes (exclusive / fork / join AND / join OR) are
		// standalone canvas elements compiled into edge semantics at deploy.
		if !strings.HasPrefix(req.Type, "GATEWAY_") {
			writeErr(w, http.StatusBadRequest, "type must be HUMAN, START, END or GATEWAY_* (system tasks use add-system-task/add-tool-task)")
			return
		}
		if req.Name == "" {
			writeErr(w, http.StatusBadRequest, "'name' is required")
			return
		}
	}
	if _, ok := d.store.Get(processID); !ok {
		writeErr(w, http.StatusNotFound, "Process not found in the management store: "+processID)
		return
	}
	err := d.store.Update(processID, func(doc map[string]any) (map[string]any, error) {
		if docTaskExists(doc, req.Name) {
			return nil, fmt.Errorf("a task named %q already exists", req.Name)
		}
		if req.AfterTask != "" && !docTaskExists(doc, req.AfterTask) {
			return nil, fmt.Errorf("after_task not found: %q", req.AfterTask)
		}
		newTask := map[string]any{
			"task_id":     req.Name,
			"id":          req.Name,
			"type":        req.Type,
			"description": firstNonEmpty(req.Description, req.Name),
		}
		if req.Type == "HUMAN" {
			newTask["group"] = req.Group
		}
		docAddTask(doc, newTask)
		if req.AfterTask != "" {
			edges, _ := doc["topology"].([]any)
			edges = append(edges, map[string]any{"from": req.AfterTask, "to": req.Name})
			doc["topology"] = edges
		}
		return doc, nil
	})
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	d.log.Info("human task added (pending version)", "processId", processID, "task", req.Name, "group", req.Group)
	writeJSON(w, http.StatusCreated, map[string]any{
		"added":     true,
		"processId": processID,
		"task":      req.Name,
		"message":   "Human task added to the pending design version — deploy it to activate.",
	})
}

// addEdge appends a topology edge to the pending design (Diagram tab via
// chat). condition defaults to "always"; fork true marks a parallel branch.
func (d *DesignService) addEdge(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("processId")
	var req struct {
		From      string   `json:"from"`
		To        string   `json:"to"`
		Condition string   `json:"condition"`
		Fork      bool     `json:"fork"`
		JoinFrom  []string `json:"join_from"`
		Join      string   `json:"join"` // "and" (default) | "or"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	req.From = strings.TrimSpace(req.From)
	req.To = strings.TrimSpace(req.To)
	if req.To == "" {
		writeErr(w, http.StatusBadRequest, "'to' is required")
		return
	}
	if len(req.JoinFrom) == 0 && req.From == "" {
		writeErr(w, http.StatusBadRequest, "'from' is required (or join_from for a parallel join)")
		return
	}
	if _, ok := d.store.Get(processID); !ok {
		writeErr(w, http.StatusNotFound, "Process not found in the management store: "+processID)
		return
	}
	err := d.store.Update(processID, func(doc map[string]any) (map[string]any, error) {
		isJoin := len(req.JoinFrom) > 0
		if !isJoin {
			if !docTaskExists(doc, req.From) {
				return nil, fmt.Errorf("from task not found: %q", req.From)
			}
		}
		if !docTaskExists(doc, req.To) {
			return nil, fmt.Errorf("to task not found: %q", req.To)
		}
		for _, jf := range req.JoinFrom {
			if !docTaskExists(doc, jf) {
				return nil, fmt.Errorf("join_from task not found: %q", jf)
			}
		}
		edges, _ := doc["topology"].([]any)
		if !isJoin {
			for _, e := range edges {
				if em, ok := e.(map[string]any); ok && strEq(em["from"], req.From) && strEq(em["to"], req.To) {
					return nil, fmt.Errorf("edge %q -> %q already exists", req.From, req.To)
				}
			}
		}
		edge := map[string]any{
			"to":        req.To,
			"condition": firstNonEmpty(req.Condition, "always"),
		}
		if isJoin {
			edge["join_from"] = req.JoinFrom
			if j := strings.TrimSpace(req.Join); j == "or" || j == "and" {
				edge["join"] = j
			}
		} else {
			edge["from"] = req.From
			if req.Fork {
				edge["fork"] = true
			}
		}
		doc["topology"] = append(edges, edge)
		return doc, nil
	})
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	d.log.Info("topology edge added (pending version)", "processId", processID, "from", req.From, "to", req.To)
	writeJSON(w, http.StatusCreated, map[string]any{
		"added":     true,
		"processId": processID,
		"from":      req.From,
		"to":        req.To,
		"message":   "Edge added to the pending design version — deploy it to activate.",
	})
}

// deleteTask removes a task and every topology reference to/from it.
// Lifecycle nodes (start/end) are protected.
// updateEdge updates an existing topology edge in the pending design:
// condition, fork flag, join kind and join_from. Plain edges match on
// from+to; join edges (no "from") match on to + a join_from member.
func (d *DesignService) updateEdge(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("processId")
	var req struct {
		From      string   `json:"from"`
		To        string   `json:"to"`
		Condition *string  `json:"condition"`
		Fork      *bool    `json:"fork"`
		Join      string   `json:"join"`
		JoinFrom  []string `json:"join_from"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	req.From = strings.TrimSpace(req.From)
	req.To = strings.TrimSpace(req.To)
	if req.To == "" {
		writeErr(w, http.StatusBadRequest, "'to' is required")
		return
	}
	if len(req.JoinFrom) == 0 && req.From == "" {
		writeErr(w, http.StatusBadRequest, "'from' is required (or join_from for a join edge)")
		return
	}
	err := d.store.Update(processID, func(doc map[string]any) (map[string]any, error) {
		edges, _ := doc["topology"].([]any)
		for _, e := range edges {
			m, ok := e.(map[string]any)
			if !ok || !strEq(m["to"], req.To) {
				continue
			}
			jf, isJoin := m["join_from"].([]any)
			if isJoin {
				if req.From == "" {
					continue
				}
				found := false
				for _, j := range jf {
					if strEq(j, req.From) {
						found = true
						break
					}
				}
				if !found {
					continue
				}
			} else if !strEq(m["from"], req.From) {
				continue
			}
			if req.Condition != nil {
				c := strings.TrimSpace(*req.Condition)
				m["condition"] = firstNonEmpty(c, "always")
			}
			if req.Fork != nil {
				if *req.Fork {
					m["fork"] = true
				} else {
					delete(m, "fork")
				}
			}
			if j := strings.TrimSpace(req.Join); j == "and" || j == "or" {
				m["join"] = j
			}
			if len(req.JoinFrom) > 0 {
				nf := make([]any, 0, len(req.JoinFrom))
				for _, j := range req.JoinFrom {
					if j == "" {
						continue
					}
					if !docTaskExists(doc, j) {
						return nil, fmt.Errorf("join_from task not found: %q", j)
					}
					nf = append(nf, j)
				}
				m["join_from"] = nf
			}
			return doc, nil
		}
		return nil, fmt.Errorf("edge not found")
	})
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	d.log.Info("topology edge updated (pending version)", "processId", processID, "from", req.From, "to", req.To)
	writeJSON(w, http.StatusOK, map[string]any{"updated": true, "from": req.From, "to": req.To})
}

func (d *DesignService) deleteTask(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("processId")
	var req struct {
		Task string `json:"task"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Task) == "" {
		writeErr(w, http.StatusBadRequest, "task is required")
		return
	}
	if strings.EqualFold(strings.TrimSpace(req.Task), "start") || strings.EqualFold(strings.TrimSpace(req.Task), "end") {
		writeErr(w, http.StatusBadRequest, "lifecycle nodes (start/end) cannot be removed")
		return
	}
	err := d.store.Update(processID, func(doc map[string]any) (map[string]any, error) {
		if !docTaskExists(doc, req.Task) {
			return nil, fmt.Errorf("task not found: %q", req.Task)
		}
		if tasks, ok := doc["tasks"].(map[string]any); ok {
			delete(tasks, req.Task)
		} else if list, ok := doc["tasks"].([]any); ok {
			kept := list[:0]
			for _, t := range list {
				if tm, ok := t.(map[string]any); ok && (strEq(tm["task_id"], req.Task) || strEq(tm["name"], req.Task) || strEq(tm["task"], req.Task)) {
					continue
				}
				kept = append(kept, t)
			}
			doc["tasks"] = kept
		}
		edges, _ := doc["topology"].([]any)
		kept := make([]any, 0, len(edges))
		for _, e := range edges {
			m, ok := e.(map[string]any)
			if !ok {
				kept = append(kept, e)
				continue
			}
			if strEq(m["from"], req.Task) || strEq(m["to"], req.Task) {
				continue
			}
			if jf, ok := m["join_from"].([]any); ok {
				filtered := make([]any, 0, len(jf))
				for _, j := range jf {
					if strEq(j, req.Task) {
						continue
					}
					filtered = append(filtered, j)
				}
				if len(filtered) == 0 {
					continue // join edge with no branches left
				}
				m["join_from"] = filtered
			}
			kept = append(kept, m)
		}
		doc["topology"] = kept
		return doc, nil
	})
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	d.log.Info("design task deleted", "processId", processID, "task", req.Task)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "task": req.Task})
}

// deleteEdge removes one edge (matched by to, plus from when given; join
// edges have no from).
func (d *DesignService) deleteEdge(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("processId")
	var req struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.To) == "" {
		writeErr(w, http.StatusBadRequest, "to is required (from optional for join edges)")
		return
	}
	err := d.store.Update(processID, func(doc map[string]any) (map[string]any, error) {
		edges, _ := doc["topology"].([]any)
		kept := make([]any, 0, len(edges))
		removed := false
		for _, e := range edges {
			m, ok := e.(map[string]any)
			if !ok {
				kept = append(kept, e)
				continue
			}
			isTarget := strEq(m["to"], req.To) && (req.From == "" || strEq(m["from"], req.From) || m["join_from"] != nil)
			if isTarget && !removed {
				removed = true
				continue
			}
			kept = append(kept, e)
		}
		if !removed {
			return nil, fmt.Errorf("edge not found")
		}
		doc["topology"] = kept
		return doc, nil
	})
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	d.log.Info("design edge deleted", "processId", processID, "from", req.From, "to", req.To)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "from": req.From, "to": req.To})
}

// updateTask changes a HUMAN task's group (responsible role) and/or its
// description in the pending version.
func (d *DesignService) updateTask(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("processId")
	var req struct {
		Task        string `json:"task"`
		Group       string `json:"group"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Task) == "" {
		writeErr(w, http.StatusBadRequest, "task is required")
		return
	}
	if strings.TrimSpace(req.Group) == "" && strings.TrimSpace(req.Description) == "" {
		writeErr(w, http.StatusBadRequest, "provide group and/or description")
		return
	}
	err := d.store.Update(processID, func(doc map[string]any) (map[string]any, error) {
		if !docTaskExists(doc, req.Task) {
			return nil, fmt.Errorf("task not found: %q", req.Task)
		}
		tc := docTaskConfig(doc, req.Task)
		if strings.TrimSpace(req.Group) != "" {
			tc["group"] = strings.TrimSpace(req.Group)
		}
		if strings.TrimSpace(req.Description) != "" {
			tc["description"] = strings.TrimSpace(req.Description)
		}
		docSetTaskConfig(doc, req.Task, tc)
		return doc, nil
	})
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	d.log.Info("design task updated", "processId", processID, "task", req.Task)
	writeJSON(w, http.StatusOK, map[string]any{"updated": true, "task": req.Task})
}

// docTaskConfig returns the raw config map of a task (either storage
// format); a fresh map when the task has none.
func docTaskConfig(doc map[string]any, task string) map[string]any {
	if tasks, ok := doc["tasks"].(map[string]any); ok {
		if tm, ok := tasks[task].(map[string]any); ok {
			return tm
		}
		return map[string]any{}
	}
	if list, ok := doc["tasks"].([]any); ok {
		for _, t := range list {
			if tm, ok := t.(map[string]any); ok && strEq(tm["task"], task) {
				return tm
			}
		}
	}
	return map[string]any{}
}

// docTaskExists reports whether a task with the given name exists in the
// engine section — map format (name = key) or list format (task_id/id).
func docTaskExists(doc map[string]any, name string) bool {
	switch v := doc["tasks"].(type) {
	case map[string]any:
		_, ok := v[name]
		return ok
	case []any:
		for _, t := range v {
			if tm, ok := t.(map[string]any); ok && (strEq(tm["task_id"], name) || strEq(tm["id"], name)) {
				return true
			}
		}
	}
	return false
}

// docAddTask appends a task to the engine section (either format).
func docAddTask(doc map[string]any, newTask map[string]any) {
	if m, ok := doc["tasks"].(map[string]any); ok {
		if m == nil {
			m = map[string]any{}
		}
		m[newTaskName(newTask)] = newTask
		doc["tasks"] = m
		return
	}
	l, _ := doc["tasks"].([]any)
	if l == nil {
		l = []any{}
	}
	doc["tasks"] = append(l, newTask)
}

func newTaskName(t map[string]any) string {
	if s, ok := t["task_id"].(string); ok && s != "" {
		return s
	}
	if s, ok := t["id"].(string); ok && s != "" {
		return s
	}
	return ""
}

// docSetTaskConfig merges the given config keys into the task's config
// (creating it when absent); returns false when the task is missing.
func docSetTaskConfig(doc map[string]any, name string, cfg map[string]any) bool {
	switch v := doc["tasks"].(type) {
	case map[string]any:
		tm, ok := v[name].(map[string]any)
		if !ok {
			return false
		}
		mergeTaskConfig(tm, cfg)
		return true
	case []any:
		for _, t := range v {
			tm, ok := t.(map[string]any)
			if !ok || !(strEq(tm["task_id"], name) || strEq(tm["id"], name)) {
				continue
			}
			mergeTaskConfig(tm, cfg)
			return true
		}
	}
	return false
}

func mergeTaskConfig(tm map[string]any, cfg map[string]any) {
	existing, _ := tm["config"].(map[string]any)
	if existing == nil {
		existing = map[string]any{}
	}
	for k, val := range cfg {
		if val == nil || val == "" {
			continue
		}
		existing[k] = val
	}
	tm["config"] = existing
}

func strEq(v any, s string) bool {
	str, _ := v.(string)
	return str == s
}

// designGuard mirrors mgmtRequireAdmin (Designer group).
func (d *DesignService) designGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := handler.ClaimsFromContext(r.Context())
		if !ok {
			writeErr(w, http.StatusUnauthorized, "Authentication required")
			return
		}
		if !isAdminGroups(c.Groups) {
			writeErr(w, http.StatusForbidden, "Access denied. Designer role required.")
			return
		}
		// The ask endpoint consumes LLM tokens — enforce the designer's
		// monthly quota (DESIGNER_TOKEN_QUOTA, default 50 000).
		if r.URL.Path == "/api/management/design/ask" && d.quotaGuard != nil && d.quotaGuard(w, r, c) {
			return
		}
		next(w, r)
	}
}

func isAdminGroups(groups []string) bool {
	for _, g := range groups {
		if strings.TrimPrefix(g, "/") == "Designer" {
			return true
		}
	}
	return false
}

func createdByFrom(r *http.Request) string {
	if c, ok := handler.ClaimsFromContext(r.Context()); ok && c.Username != "" {
		return c.Username
	}
	return ""
}

// designFromInput parses the designer input (YAML or JSON) into a validated
// ProcessDefinition and returns its canonical JSON form.
func designFromInput(processID, input string) ([]byte, error) {
	var in map[string]any
	if err := yaml.Unmarshal([]byte(input), &in); err != nil {
		return nil, err
	}
	if _, ok := in["tasks"]; !ok {
		return nil, errMissingTasks
	}
	// The engine section must parse as a valid workflow definition.
	merged := map[string]any{"process_id": processID}
	for k, v := range in {
		if k == "app" || k == "process_id" {
			continue
		}
		merged[k] = v
	}
	mergedYAML, err := yaml.Marshal(merged)
	if err != nil {
		return nil, err
	}
	def, err := domain.ParseYAML(mergedYAML)
	if err != nil {
		return nil, err
	}
	return json.Marshal(def)
}

var errMissingTasks = &designError{msg: "design must contain a tasks list"}

type designError struct{ msg string }

func (e *designError) Error() string { return e.msg }

// --- POST /api/management/design/save --------------------------------------

type saveDesignRequest struct {
	ProcessID string `json:"processId"`
	// YAML (or JSON) of the engine section: description?, tasks[],
	// topology[], start_task_id?.
	YAML string `json:"yaml"`
}

func (d *DesignService) saveDesign(w http.ResponseWriter, r *http.Request) {
	var req saveDesignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.ProcessID) == "" || strings.TrimSpace(req.YAML) == "" {
		writeErr(w, http.StatusBadRequest, "processId and yaml are required")
		return
	}
	if !d.store.Has(req.ProcessID) {
		writeErr(w, http.StatusNotFound, "Process not found: "+req.ProcessID)
		return
	}

	design, err := designFromInput(req.ProcessID, req.YAML)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "design rejected: "+err.Error())
		return
	}

	// Unconfirmed changes collapse into the single pending version: the
	// current full document with the new engine section applied.
	doc, err := d.pendingDocument(r.Context(), req.ProcessID, design)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := d.designs.UpsertPendingDesign(r.Context(), req.ProcessID, doc, createdByFrom(r)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"processId": req.ProcessID, "saved": true, "pending": true})
}

// pendingDocument returns the full process document (engine + app) as JSON,
// with the engine section replaced by the validated design bytes.
func (d *DesignService) pendingDocument(ctx context.Context, processID string, engineDesign []byte) ([]byte, error) {
	doc, err := d.store.DocumentJSON(processID)
	if err != nil {
		return nil, fmt.Errorf("read process document: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(doc, &m); err != nil {
		return nil, err
	}
	var defMap map[string]any
	if err := json.Unmarshal(engineDesign, &defMap); err != nil {
		return nil, err
	}
	for k, v := range defMap {
		m[k] = v
	}
	return json.Marshal(m)
}

// --- POST /api/management/design/deploy ------------------------------------

type deployDesignRequest struct {
	ProcessID string `json:"processId"`
	// Version to activate; 0 = the latest saved version.
	Version int `json:"version"`
}

func (d *DesignService) deployDesign(w http.ResponseWriter, r *http.Request) {
	var req deployDesignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.ProcessID) == "" {
		writeErr(w, http.StatusBadRequest, "processId is required")
		return
	}

	var dv *repository.DesignVersion
	if req.Version > 0 {
		d, err := d.designs.GetDesign(r.Context(), req.ProcessID, req.Version)
		if err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		dv = d
	} else {
		// Default: deploy the single pending (unconfirmed) version.
		// Re-snapshot the designer's CURRENT document first so the deploy
		// always reflects the latest mutations (a stale async pending row
		// can never deploy a step that was just deleted).
		d.syncPendingNow(r.Context(), req.ProcessID)
		p, err := d.designs.GetPendingDesign(r.Context(), req.ProcessID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if p == nil {
			writeErr(w, http.StatusBadRequest, "no pending changes to deploy for this process")
			return
		}
		dv = p
	}

	// Automatic semver: diff the deployed version against the currently
	// active one (rules in design_semver.go).
	activeBytes, activeVersion, _ := d.designs.GetActiveDesign(r.Context(), req.ProcessID)
	actSemver := ""
	if activeVersion > 0 {
		if activeDV, err := d.designs.GetDesign(r.Context(), req.ProcessID, activeVersion); err == nil && activeDV != nil {
			actSemver = activeDV.Semver
		}
	}
	semver := computeDesignSemver(actSemver, activeBytes, dv.Design)

	// Compile the visual gateway nodes the designer places on the canvas
	// into the engine's edge-based gateway semantics. The stored design
	// keeps the user's gateway nodes (the canvas shows them with their
	// input/output connections); the engine registers the compiled form.
	compiled, gerr := compileGateways(dv.Design)
	if gerr != nil {
		writeErr(w, http.StatusBadRequest, gerr.Error())
		return
	}

	if err := d.designs.SetActiveDesignSemver(r.Context(), req.ProcessID, dv.Version, semver); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Register the deployed design so /definitions and engine startup see it.
	if d.reg != nil {
		if def, perr := repository.DesignOf(compiled); perr == nil {
			if err := d.reg.RegisterDefinition(r.Context(), def); err != nil {
				d.log.Warn("deploy: definition registration failed", "processId", req.ProcessID, "error", err)
			}
		} else {
			d.log.Warn("deploy: design document does not parse as an engine definition — registry keeps the previous version", "processId", req.ProcessID, "error", perr)
		}
	}
	// Keep the on-disk document in sync with the deployed engine section so
	// the file always reflects the live design (discard restores from it).
	d.writeEngineSectionToDisk(r.Context(), req.ProcessID, compiled)
	// Form configuration is part of the design version: roll the live form
	// steps back to this version's snapshot (form instances are untouched).
	d.writeFormsSectionToDisk(r.Context(), req.ProcessID, dv.Design)
	// If the deployed design renamed a task (1:1 swap vs the previous
	// active version), cascade the rename into the app section so form
	// steps and role tasks keep pointing at the task.
	d.cascadeTaskRenames(r.Context(), req.ProcessID, activeBytes, dv.Design)
	writeJSON(w, http.StatusOK, map[string]any{
		"processId": req.ProcessID,
		"version":   dv.Version,
		"semver":    semver,
		"deployed":  true,
	})
}

// compileGateways rewrites the visual gateway nodes (GATEWAY_* tasks placed
// on the design canvas) into the engine's edge-based gateway semantics:
//
//	GATEWAY_EXCLUSIVE        1 input + 1..n outputs -> one conditional edge
//	                         per output (input -> output, keeping condition)
//	GATEWAY_FORK             1 input + 1..n outputs -> fork edges
//	GATEWAY_JOIN_AND / _OR   2..n inputs + 1 output -> a single join edge
//
// The design document's tasks/topology are rewritten so the engine never
// sees a gateway as an executable task. Documents without gateways are
// returned unchanged.
func compileGateways(doc []byte) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(doc, &raw); err != nil {
		return doc, nil // not a JSON design doc; leave as-is
	}
	topRaw, hasTop := raw["topology"]
	if !hasTop {
		return doc, nil
	}
	var edges []domain.Edge
	if err := json.Unmarshal(topRaw, &edges); err != nil {
		return doc, nil
	}

	// Task names + types, accepting both list and map encodings.
	type gwTask struct{ id, typ string }
	var gwTasks []gwTask
	if tRaw, ok := raw["tasks"]; ok {
		var list []map[string]any
		isList := json.Unmarshal(tRaw, &list) == nil
		items := map[string]map[string]any{}
		if isList {
			for _, t := range list {
				id, _ := t["task_id"].(string)
				if id == "" {
					id, _ = t["name"].(string)
				}
				if id != "" {
					items[id] = t
				}
			}
		} else {
			_ = json.Unmarshal(tRaw, &items)
		}
		for id, t := range items {
			typ, _ := t["type"].(string)
			if strings.HasPrefix(typ, "GATEWAY_") {
				gwTasks = append(gwTasks, gwTask{id, typ})
			}
		}
	}
	if len(gwTasks) == 0 {
		return doc, nil
	}
	isGw := make(map[string]bool, len(gwTasks))
	for _, g := range gwTasks {
		isGw[g.id] = true
	}

	out := make([]domain.Edge, 0, len(edges))
	for _, e := range edges {
		if isGw[e.From] || isGw[e.To] {
			continue // rewritten below
		}
		out = append(out, e)
	}
	for _, g := range gwTasks {
		var inputs []string
		var outputs []domain.Edge
		for _, e := range edges {
			if e.To == g.id && e.From != "" && len(e.JoinFrom) == 0 {
				inputs = append(inputs, e.From)
			}
			if e.From == g.id {
				outputs = append(outputs, e)
			}
		}
		switch g.typ {
		case "GATEWAY_EXCLUSIVE", "GATEWAY_FORK":
			if len(inputs) != 1 || len(outputs) < 1 {
				return nil, fmt.Errorf("gateway %q: connect exactly 1 input step and at least 1 output step before deploying", g.id)
			}
			for _, o := range outputs {
				ne := domain.Edge{From: inputs[0], To: o.To, Condition: o.Condition}
				if g.typ == "GATEWAY_FORK" {
					ne.Fork = true
				}
				out = append(out, ne)
			}
		case "GATEWAY_JOIN_AND", "GATEWAY_JOIN_OR":
			if len(inputs) < 2 || len(outputs) != 1 {
				return nil, fmt.Errorf("gateway %q: connect at least 2 input steps and exactly 1 output step before deploying", g.id)
			}
			join := "and"
			if g.typ == "GATEWAY_JOIN_OR" {
				join = "or"
			}
			out = append(out, domain.Edge{JoinFrom: inputs, To: outputs[0].To, Join: join})
		default:
			return nil, fmt.Errorf("unknown gateway type %q on %q", g.typ, g.id)
		}
	}

	op, err := json.Marshal(out)
	if err != nil {
		return doc, nil
	}
	raw["topology"] = op

	// Remove the gateway tasks from the tasks section (either encoding).
	var list []map[string]any
	if tRaw, ok := raw["tasks"]; ok && json.Unmarshal(tRaw, &list) == nil {
		kept := make([]map[string]any, 0, len(list))
		for _, t := range list {
			id, _ := t["task_id"].(string)
			if id == "" {
				id, _ = t["name"].(string)
			}
			if !isGw[id] {
				kept = append(kept, t)
			}
		}
		if kb, err := json.Marshal(kept); err == nil {
			raw["tasks"] = kb
		}
	} else if m, ok := raw["tasks"]; ok {
		var items map[string]map[string]any
		if json.Unmarshal(m, &items) == nil {
			for id := range isGw {
				delete(items, id)
			}
			if mb, err := json.Marshal(items); err == nil {
				raw["tasks"] = mb
			}
		}
	}
	return json.Marshal(raw)
}

// cascadeTaskRenames aligns the app section (form step ids, role task
// references) with a task rename performed in the engine design via
// save_design. Only an unambiguous 1:1 swap (exactly one task removed and
// exactly one added) is treated as a rename; anything else is left alone
// (sync-forms/sync-roles remain the cleanup path).
func (d *DesignService) cascadeTaskRenames(ctx context.Context, processID string, oldDoc, newDoc []byte) {
	from, to, ok := taskRenamePair(oldDoc, newDoc)
	if !ok {
		return
	}
	if _, err := d.store.mutateApp(processID, func(app *AppConfig) error {
		for i := range app.Forms.Steps {
			if app.Forms.Steps[i].StepID == from {
				app.Forms.Steps[i].StepID = to
			}
		}
		for ri := range app.Roles {
			for i := range app.Roles[ri].RoleTasks {
				if app.Roles[ri].RoleTasks[i].TaskName == from {
					app.Roles[ri].RoleTasks[i].TaskName = to
				}
			}
		}
		return nil
	}); err != nil {
		d.log.Warn("deploy: task-rename cascade failed", "processId", processID, "from", from, "to", to, "error", err)
	}
}

// taskRenamePair returns (from, to, true) when the new design replaces
// exactly one task id with exactly one new one.
func taskRenamePair(oldDoc, newDoc []byte) (string, string, bool) {
	oldIDs := taskIDsFromDoc(oldDoc)
	newIDs := taskIDsFromDoc(newDoc)
	var removed, added []string
	for id := range oldIDs {
		if !newIDs[id] {
			removed = append(removed, id)
		}
	}
	for id := range newIDs {
		if !oldIDs[id] {
			added = append(added, id)
		}
	}
	if len(removed) == 1 && len(added) == 1 {
		return removed[0], added[0], true
	}
	return "", "", false
}

func taskIDsFromDoc(doc []byte) map[string]bool {
	out := map[string]bool{}
	var m map[string]any
	if json.Unmarshal(doc, &m) != nil {
		return out
	}
	if tasks, ok := m["tasks"].([]any); ok {
		for _, t := range tasks {
			if tm, ok := t.(map[string]any); ok {
				if id, ok := tm["task_id"].(string); ok && id != "" {
					out[id] = true
				}
			}
		}
		return out
	}
	if tasks, ok := m["tasks"].(map[string]any); ok {
		for k := range tasks {
			out[k] = true
		}
	}
	return out
}

// writeEngineSectionToDisk replaces the ENGINE keys of the process YAML with
// the deployed design. The app section (roles, search items, ...) is never
// touched by this — forms are handled separately by
// writeFormsSectionToDisk (form configuration belongs to the design
// version and is rolled back on deploy); the rest of the app section has
// its own write path (management endpoints).
func (d *DesignService) writeEngineSectionToDisk(ctx context.Context, processID string, design []byte) {
	var defMap map[string]any
	if err := json.Unmarshal(design, &defMap); err != nil {
		return
	}
	engineKeys := map[string]bool{
		"description":   true,
		"tasks":         true,
		"topology":      true,
		"start_task_id": true,
		"end_task_ids":  true,
	}
	yamlMap := map[string]any{}
	for k, v := range defMap {
		if k == "process_id" || !engineKeys[k] {
			continue
		}
		b, _ := json.Marshal(v)
		var yv any
		if err := yaml.Unmarshal(b, &yv); err == nil {
			yamlMap[k] = yv
		}
	}
	if len(yamlMap) == 0 {
		return
	}
	_ = d.store.Update(processID, func(doc map[string]any) (map[string]any, error) {
		for k, v := range yamlMap {
			doc[k] = v
		}
		return doc, nil
	})
}

// writeFormsSectionToDisk rolls the live form CONFIGURATION back to the
// snapshot stored in the version being deployed: form configuration is
// versioned with the process design, so deploying (or rolling back to)
// a version restores that version's form steps, exactly like the engine
// section. Form INSTANCES (filled-in values / form input data) live per
// process instance and are never touched.
func (d *DesignService) writeFormsSectionToDisk(ctx context.Context, processID string, designDoc []byte) {
	var doc map[string]any
	if err := json.Unmarshal(designDoc, &doc); err != nil {
		return
	}
	appM, _ := doc["app"].(map[string]any)
	// A version document without a forms key must not wipe the live forms.
	if _, ok := appM["forms"]; !ok {
		return
	}
	app, err := appFromMap(doc)
	if err != nil || app == nil {
		d.log.Warn("deploy: version app section unparseable; forms not rolled back", "processId", processID, "error", err)
		return
	}
	if _, err := d.store.mutateApp(processID, func(a *AppConfig) error {
		a.Forms = app.Forms
		return nil
	}); err != nil {
		d.log.Warn("deploy: form rollback failed", "processId", processID, "error", err)
	}
}

// discardDesign reverts the unconfirmed (pending) changes: the live process
// document is restored from the last deployed version, then the pending
// version is removed.
type discardDesignRequest struct {
	ProcessID string `json:"processId"`
}

func (d *DesignService) discardDesign(w http.ResponseWriter, r *http.Request) {
	var req discardDesignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.ProcessID) == "" {
		writeErr(w, http.StatusBadRequest, "processId is required")
		return
	}
	activeBytes, activeVersion, err := d.designs.GetActiveDesign(r.Context(), req.ProcessID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if activeBytes != nil {
		var doc map[string]any
		if err := json.Unmarshal(activeBytes, &doc); err == nil && len(doc) > 0 {
			// Only restore from a full document snapshot (engine + app).
			// Legacy engine-only snapshots must not clobber the live app
			// section — in that case just drop the pending version.
			if _, hasApp := doc["app"]; hasApp || !d.liveDocHasApp(req.ProcessID) {
				if err := d.store.Update(req.ProcessID, func(_ map[string]any) (map[string]any, error) {
					return doc, nil
				}); err != nil {
					writeErr(w, http.StatusInternalServerError, "restore failed: "+err.Error())
					return
				}
				if d.reg != nil {
					if def, perr := repository.DesignOf(activeBytes); perr == nil {
						if err := d.reg.RegisterDefinition(r.Context(), def); err != nil {
							d.log.Warn("discard: definition re-registration failed", "processId", req.ProcessID, "error", err)
						}
					}
				}
			} else {
				d.log.Warn("discard: active snapshot has no app section, skipping restore", "processId", req.ProcessID)
			}
		}
	}
	if err := d.designs.DeletePendingDesign(r.Context(), req.ProcessID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"processId": req.ProcessID,
		"discarded": true,
		"restored":  activeVersion,
	})
}

// liveDocHasApp reports whether the on-disk process document currently has
// an app section (used to decide if a legacy snapshot restore is safe).
func (d *DesignService) liveDocHasApp(processID string) bool {
	doc, err := d.store.DocumentJSON(processID)
	if err != nil {
		return false
	}
	var m map[string]any
	if json.Unmarshal(doc, &m) != nil {
		return false
	}
	_, ok := m["app"]
	return ok
}

// SnapshotPending refreshes the single pending version from the current
// live process document. Called by the store after every design write.
// When the live document is identical to the deployed (active) snapshot —
// e.g. right after a deploy or a discard-restore — no pending version is
// kept: unconfirmed changes are exactly the diff against the deploy.
func (d *DesignService) SnapshotPending(ctx context.Context, processID string, createdBy string) {
	// Sequence number at MUTATION time: snapshots that run out of order
	// (goroutine scheduling) are dropped, so the pending row always
	// converges on the latest mutation.
	seq := atomic.AddInt64(&d.snapSeq, 1)
	go d.takeSnapshot(context.Background(), processID, createdBy, seq)
}

// syncPendingNow snapshots the live store document synchronously — used by
// deploy so a just-deleted step can never be resurrected by a queued
// stale snapshot or a stale pending row.
func (d *DesignService) syncPendingNow(ctx context.Context, processID string) {
	seq := atomic.AddInt64(&d.snapSeq, 1)
	d.takeSnapshot(ctx, processID, "deploy", seq)
}

func (d *DesignService) takeSnapshot(ctx context.Context, processID, createdBy string, seq int64) {
	d.snapMu.Lock()
	defer d.snapMu.Unlock()
	if seq < d.lastSnap[processID] {
		return // a newer mutation already snapshotted this process
	}
	d.lastSnap[processID] = seq
	doc, err := d.store.DocumentJSON(processID)
	if err != nil {
		d.log.Warn("pending snapshot: read document failed", "processId", processID, "error", err)
		return
	}
	active, _, err := d.designs.GetActiveDesign(ctx, processID)
	if err == nil && docEqual(active, doc) {
		_ = d.designs.DeletePendingDesign(ctx, processID)
		return
	}
	if err := d.designs.UpsertPendingDesign(ctx, processID, doc, createdBy); err != nil {
		d.log.Warn("pending snapshot failed", "processId", processID, "error", err)
	}
}

// docEqual compares two JSON documents by key-sorted marshalled bytes.
func docEqual(a, b []byte) bool {
	var am, bm map[string]any
	if json.Unmarshal(a, &am) != nil || json.Unmarshal(b, &bm) != nil {
		return false
	}
	ab, _ := json.Marshal(am)
	bb, _ := json.Marshal(bm)
	return string(ab) == string(bb)
}

// --- GET /api/management/design/versions ------------------------------------

func (d *DesignService) listDesigns(w http.ResponseWriter, r *http.Request) {
	processID := r.URL.Query().Get("processId")
	if processID == "" {
		writeErr(w, http.StatusBadRequest, "processId query param is required")
		return
	}
	versions, err := d.designs.ListDesigns(r.Context(), processID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(versions))
	for _, dv := range versions {
		m := map[string]any{
			"version":    dv.Version,
			"semver":     dv.Semver,
			"status":     dv.Status,
			"created_by": dv.CreatedBy,
			"created_at": dv.CreatedAt,
		}
		if def, perr := repository.DesignOf(dv.Design); perr == nil {
			m["tasks"] = len(def.Tasks)
		}
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, out)
}

// --- GET /api/management/design/{processId} ---------------------------------

func (d *DesignService) getDesign(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("processId")
	version := 0
	if v := r.URL.Query().Get("version"); v != "" {
		version, _ = strconv.Atoi(v)
	}
	// ?pending=true returns the pending (unconfirmed) version — the live
	// editing target of the interactive designer canvas.
	if r.URL.Query().Get("pending") == "true" {
		p, err := d.designs.GetPendingDesign(r.Context(), processID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if p == nil {
			writeErr(w, http.StatusNotFound, "no pending design for this process")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"processId":  processID,
			"version":    p.Version,
			"status":     p.Status,
			"created_by": p.CreatedBy,
			"created_at": p.CreatedAt,
			"design":     json.RawMessage(p.Design),
		})
		return
	}
	if version <= 0 {
		_, av, err := d.designs.GetActiveDesign(r.Context(), processID)
		if err != nil || av == 0 {
			writeErr(w, http.StatusNotFound, "no active design for this process")
			return
		}
		version = av
	}
	dv, err := d.designs.GetDesign(r.Context(), processID, version)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"processId":  processID,
		"version":    dv.Version,
		"status":     dv.Status,
		"created_by": dv.CreatedBy,
		"created_at": dv.CreatedAt,
		"design":     json.RawMessage(dv.Design),
	})
}

// --- GET /api/management/design/{processId}/ai-reviews -----------------------

// listAiReviews returns the process's AI design-review history (newest
// first) — the prompts and verdicts kept for the design history.
func (d *DesignService) listAiReviews(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("processId")
	reviews := []*repository.DesignReview{}
	if d.reviewStore != nil {
		lst, err := d.reviewStore.ListDesignReviews(r.Context(), pid, 20)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		reviews = lst
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviews": reviews})
}

// --- POST /api/management/design/ask ----------------------------------------

type designChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type askDesignRequest struct {
	ProcessID string              `json:"processId"`
	Prompt    string              `json:"prompt"`
	Language  string              `json:"lang"`
	History   []designChatMessage `json:"history"`
	// Context: free-form client context (e.g. current UI selection).
	Context string `json:"context"`
}

func (d *DesignService) askDesign(w http.ResponseWriter, r *http.Request) {
	var req askDesignRequest
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Prompt) == "" {
		writeErr(w, http.StatusBadRequest, "prompt is required")
		return
	}
	lang := req.Language
	if lang == "" {
		lang = "en"
	}

	system := designSystemPrompt(lang)

	var user strings.Builder
	activeVersion := 0
	if req.ProcessID != "" {
		// Harness: ground the reviewer in THIS process, anchored on the
		// deployed (active) version, with the working copy and history.
		user.WriteString("PROCESS CONTEXT (ground every answer in this, per deployed version):\n")
		if activeBytes, v, err := d.designs.GetActiveDesign(r.Context(), req.ProcessID); err == nil && len(activeBytes) > 0 {
			activeVersion = v
			semver := ""
			if dv, e2 := d.designs.GetDesign(r.Context(), req.ProcessID, v); e2 == nil && dv != nil {
				semver = dv.Semver
			}
			fmt.Fprintf(&user, "Deployed (active) version: v%d (semver %s) — this is what runs in production.\n", v, semver)
			if def, perr := repository.DesignOf(activeBytes); perr == nil {
				if y, merr := definitionToYAML(def); merr == nil {
					fmt.Fprintf(&user, "Deployed definition (engine view):\n```yaml\n%s\n```\n", y)
			}
			}
		}
		if p, err := d.designs.GetPendingDesign(r.Context(), req.ProcessID); err == nil && p != nil {
			fmt.Fprintf(&user, "Working (pending, not yet deployed) version: v%d — the designer is editing this:\n```yaml\n%s\n```\n", p.Version, string(p.Design))
		}
		if versions, err := d.designs.ListDesigns(r.Context(), req.ProcessID); err == nil && len(versions) > 0 {
			user.WriteString("Saved design versions (newest first):\n")
			for _, dv := range versions {
				extra := ""
				if def, perr := repository.DesignOf(dv.Design); perr == nil {
					extra = fmtTasksCount(def)
				}
				fmt.Fprintf(&user, "  v%d [%s] %s %s\n", dv.Version, dv.Status, dv.CreatedAt.Format("2006-01-02 15:04"), extra)
			}
			user.WriteString("\n")
		}
		if d.reviewStore != nil {
			if reviews, rerr := d.reviewStore.ListDesignReviews(r.Context(), req.ProcessID, 3); rerr == nil && len(reviews) > 0 {
				user.WriteString("Previous AI design reviews for this process (newest first — stay consistent with them):\n")
				for _, rv := range reviews {
					fmt.Fprintf(&user, "  [%s, deployed v%d] Q: %s\n  A: %s\n", rv.CreatedAt.Format("2006-01-02 15:04"), rv.DesignVersion, clipRunes(rv.Prompt, 300), clipRunes(rv.Answer, 800))
				}
				user.WriteString("\n")
			}
		}
	}
	if len(req.History) > 0 {
		user.WriteString("Conversation so far (earlier turns and tool results):\n")
		for _, h := range req.History {
			fmt.Fprintf(&user, "[%s]: %s\n", h.Role, h.Content)
		}
		user.WriteString("\n")
	}
	if req.Context != "" {
		fmt.Fprintf(&user, "UI context:\n%s\n", req.Context)
	}
	fmt.Fprintf(&user, "Request: %s", req.Prompt)

	answer, err := d.llm.Ask(r.Context(), &ai.AskRequest{System: system, User: user.String()})
	if err != nil {
		d.log.Error("design ask: llm call failed", "error", err)
		writeErr(w, http.StatusBadGateway, "LLM call failed: "+err.Error())
		return
	}
	// Keep the exchange in the design history (tagged with the deployed version).
	if d.reviewStore != nil && req.ProcessID != "" {
		_ = d.reviewStore.SaveDesignReview(r.Context(), &repository.DesignReview{
			ID: domain.NewUUID(), ProcessID: req.ProcessID, DesignVersion: activeVersion,
			Prompt: strings.TrimSpace(req.Prompt), Answer: answer, CreatedAt: time.Now().UTC(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"answer": answer})
}

func fmtTasksCount(def *domain.ProcessDefinition) string {
	return fmt.Sprintf("(%d tasks)", len(def.Tasks))
}

// clipRunes truncates s to at most n runes (ellipsis when cut).
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// definitionToYAML renders a registered definition as the engine-section
// YAML the designer works with (process_id, description, tasks, topology,
// start_task_id).
func definitionToYAML(def *domain.ProcessDefinition) (string, error) {
	doc := map[string]any{
		"process_id": def.ID,
		"tasks":      def.TaskList,
		"topology":   def.Topology,
	}
	if def.Description != "" {
		doc["description"] = def.Description
	}
	if def.StartTaskID != "" {
		doc["start_task_id"] = def.StartTaskID
	}
	b, err := yaml.Marshal(doc)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func designSystemPrompt(lang string) string {
	const fence = "```"
	prompt := `You are the process-design assistant of the OUMMA agentic workspace.
A process designer describes in natural language what they want; you either
answer, or request exactly ONE tool call per message using a fenced block:

@FENCE@tool
{"name": "<tool>", "args": { ... }}
@FENCE@

Available tools (executed by the client; the user confirms each write). They
cover EVERY designer tab — process/overview, diagram, forms, roles, search
items, tools and versions. All writes land in the PENDING design version;
deploying (or cancelling) it is the USER's action in the UI — never yours.

Overview / process:
- get_process_config {processId} — full config (engine + app sections)
- create_process {processName, processId?, processDescription?, iconPath?, canBeLaunched?, launchRoleId?} — starts a NEW PROCESS PROJECT: the process is scaffolded with a minimal engine design (start -> end). From then on, ALL your changes to it (tasks, edges, forms, roles, tools, labels...) accumulate in its SINGLE pending version — the user reviews it in the Design versions tab and deploys (or cancels) it. Keep building on the pending version; never duplicate changes.
- update_process {processId, ...process fields}
- set_process_icon {processId, iconKey} — the process menu icon (key from the icon catalog, e.g. "bank", "e-hospital")
- rename_process {processId, newProcessId} — change the PROCESS ID itself. Only allowed while the project has NEVER been deployed (the server rejects deployed processes). Once deployed, the id is immutable.
- delete_process {processId} — delete a process in which NO instance was ever executed (a brand-new project: undeployed, or deployed but never launched). The server rejects processes with executed instances. Only call it when the user explicitly asks to delete the project.
- set_ai_params {processId, model, embeddingModel?, processDesc?}
- add_template {processId, templateId, name, templateType, allowedGroups?, allowedSteps?, jsonTemplate}

Diagram (tasks + topology):
- add_task {processId, name, group, description?, after_task?, type? (HUMAN default | START | END)} — add a task; HUMAN needs the group (its role), START/END don't. after_task optionally wires an edge.
- add_edge {processId, from, to, condition? (default "always"), fork? (bool, parallel branch), join_from? ([task...]), join? ("and"|"or", default "and")} — connect two tasks. For a PARALLEL JOIN (a step waiting for converging branches) pass join_from:[branch1,branch2] + to (instead of from).
- update_task {processId, task, group?, description?} — change a HUMAN task's responsible role group and/or description.
- delete_task {processId, task} — remove a HUMAN/SYSTEM task and every topology reference to it (start/end are protected).
- delete_edge {processId, from?, to} — remove an edge (from optional for join edges).
- rename_task {processId, from, to} — rename a task the durable way: engine task_id + topology, the matching form step and role task references, all in one pending version. Use this (NOT add_form) when the user wants to change a step/task name.
- add_tool_task {processId, name, tool (mcp.gmail.send|mcp.telegram.send|mcp.whatsapp.send|mcp.http.request), tool_config (object of the tool's config fields), tool_inputs (object: tool parameter <- $-prefixed context field), after_task?} — a SYSTEM task that EXECUTES the tool automatically (zero LLM tokens). Config fields per tool: telegram {bot_token, chat_id}; gmail {from, gateway, api_key}; whatsapp {access_token, phone_number_id, api_base?}; http {method, url, headers? (JSON string)}. If the user has not given the credentials, add the task anyway and tell them to complete the configuration in the Tools tab.
- add_system_task {processId, name, description?, group?, after_task?, url?, method?, headers?} — a plain SYSTEM task (HTTP executor). Use the suggested url/method/headers when the user gives them; omit them when the user wants to configure it later (the task is created unconfigured and must be completed in the Tools tab before deploying).
- set_task_config {processId, task, url, method?, headers? (object)} — "configure later": set/update the HTTP config of an existing system task.

Forms:
- add_form {processId, stepId, launchable?, formInput?, fields:[{field_row (int), field_tab, field_type, field_label, field_name, field_required (bool), field_edit (bool), field_header_display (bool), field_minLength?, field_maxLength?, field_pattern?, field_options? (dropdown: ["a","b"]), field_source? (external), field_source_type? ("Internal-API"|"Internal-API-Config"), field_multple_choice? (sic), field_unique?, user_can_add?, field_template? ("json list": [{"fieldName","fieldLabel","fieldType","fieldRegex?"}])}]}
  field_name MUST be $-prefixed (e.g. "$amount"). formInput = initial step context object (e.g. {"assignee":"Managers"}); its keys must match $ field names; use null when there is none. Never put $-names as values.
  EXACT add_form example:
{"name":"add_form","args":{"processId":"p","stepId":"Review","launchable":false,"formInput":null,"fields":[{"field_row":1,"field_tab":"Decision","field_type":"dropdown","field_label":"Approval","field_name":"$approval","field_required":true,"field_edit":true,"field_header_display":false,"field_options":["Yes","No"]},{"field_row":2,"field_tab":"Decision","field_type":"date","field_label":"Date","field_name":"$decided_at","field_required":false,"field_edit":true,"field_header_display":false}]}}
- update_form_step {processId, stepId, newStepId?, launchable?, formInput?, fields?} — edit an EXISTING form step (fields replace the step's fields when given; same field spec as add_form)
- get_form_steps {processId} — the process' current form steps
- sync_forms {processId} — create form steps for every human task that has none yet

Roles:
- add_role {processId, roleId (Keycloak group name), roleName, canLaunch?, canEditInitialForm?, applyDeptFilter?, docRoles?, viewAccessRoles?, roleTasks? (array of {taskName, actions:{view?, complete?, comment?, attachment?}})} — upsert a role and its per-task permissions
- sync_roles {processId} — re-derive roles from the engine design's task groups

Search items (label catalog):
- add_search_item {processId, roleId ("app" = process-level catalog, recommended), fieldName, fieldType (menu_item|tab|field_label|hint), fieldLabelEn, fieldLabelFr, fieldLabelAr} — localized LABELS ONLY the app resolves for menu items (fieldName = menu key), form tab titles, field labels (fieldName = $-prefixed field name) and UI status hints (fieldType = hint, fieldName = hint key). Values are never touched. Same fieldName replaces (edit).
- delete_search_item {processId, roleId, fieldName} — remove a label from the catalog

Versions:
- save_design {processId, yaml} — save the workflow engine design as a NEW version (engine section ONLY: description, tasks[], topology[], start_task_id?). Tasks: task_id, type (START|HUMAN|SYSTEM|END), description, group (HUMAN), input_schema/output_schema (JSON strings). Topology edges: {from,to,condition?} or fork/join {from,fork:true} / {join_from:[...]}. Saving never affects running instances.
- list_design_versions {processId} — saved versions with status.
- get_design_version {processId, version} — the full saved design of one version.

DEPLOYMENT IS OUT OF YOUR SCOPE: deploying a pending version — or cancelling it —
is the USER'S decision, made in the UI (Design versions tab). NEVER suggest or
call a deploy/discard tool. After any change set, tell the user: the changes are
in the pending version; they can review and deploy (or cancel) it themselves.

Form types (fieldType) allowed by the backend: text, dropdown (needs fieldOptions), checkbox, date, external (needs fieldSource + fieldSourceType Internal-API|Internal-API-Config), "json list" (needs fieldTemplate: [{fieldName, fieldLabel, fieldType, fieldRegex?}]). When converting pasted JSON into a form: map keys to $-prefixed field names, infer types (string→text, number→text, boolean→checkbox, date-like→date, array of objects→"json list" with a template from the item keys, fixed small option set→dropdown), group into tabs by nesting.

Design rules you must respect (the server re-validates and rejects violations):
- one START, at least one END; unique task_id; HUMAN tasks need a group that
  matches a configured role; topology edges only reference defined tasks;
  fork branches need a join; input/output schemas must be valid JSON.
- form step_id must match the step used by the corresponding engine task;
  every $-prefixed form field needs a search item with en/fr/ar labels.
- A newly created project is NOT visible to the engine (unlaunchable) until
  the user deploys its pending version in the UI.
- delete_process works only when no instance was ever executed in the
  process; rename_process only while the project has never been deployed.
- Never invent process data.

Keep answers concise, plain text, at most a short explanation plus at most
one tool block. If a tool result reports an error, fix the request and call
again. NEVER claim a change was made unless you emitted the tool block AND a
tool result reported success — if you did not call a tool, say you are ready
to apply it and emit the call. A tool block must be ONE complete, valid
JSON object (never truncated); the client executes it only after the user
confirms it. Always respond in: ` + lang + `.`
	return strings.ReplaceAll(prompt, "@FENCE@", fence)
}

// ---------------------------------------------------------------------------
// POST /api/management/design/{processId}/sync-roles  (Designer)
//
// Derives the process roles from the ENGINE design: one role per group of
// HUMAN tasks, with the standard action groups. Replaces the manual
// setparams/add-roles-*.sh scripts. After syncing, the designer can change
// every permission per role (POST /api/management/add-role/{processId}
// upserts by role_id).
// ---------------------------------------------------------------------------

func (d *DesignService) syncRoles(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("processId")
	def, err := d.reg.GetDefinition(r.Context(), processID)
	if err != nil || def == nil || len(def.TaskList) == 0 {
		writeErr(w, http.StatusNotFound, "No engine definition found for process "+processID)
		return
	}
	if _, ok := d.store.Get(processID); !ok {
		writeErr(w, http.StatusNotFound, "Process not found in the management store: "+processID)
		return
	}

	// Group HUMAN tasks by group (order preserved).
	var groupOrder []string
	groupTasks := map[string][]*domain.Task{}
	for _, t := range def.TaskList {
		if t.Type != domain.TaskTypeHuman || t.Group == "" {
			continue
		}
		if _, seen := groupTasks[t.Group]; !seen {
			groupOrder = append(groupOrder, t.Group)
		}
		groupTasks[t.Group] = append(groupTasks[t.Group], t)
	}
	if len(groupOrder) == 0 {
		writeErr(w, http.StatusBadRequest, "The engine definition has no HUMAN tasks with a group")
		return
	}

	// First HUMAN task in BFS order from the start → its group may launch
	// and edit the initial form.
	firstHumanGroup := d.firstHumanGroup(def)

	synced := []string{}
	for _, g := range groupOrder {
		launch := g == firstHumanGroup
		tasks := []RoleTaskDTO{}
		for _, t := range groupTasks[g] {
			actions := []TaskActionDTO{
				{ActionGroup: "attachment", ActionItems: standardActionItems([]string{"read", "write", "update"})},
				{ActionGroup: "comment", ActionItems: standardActionItems([]string{"read", "write", "update", "delete"})},
				{ActionGroup: "action", ActionItems: standardActionItems([]string{"skip", "Complete", "Claim", "Release"})},
			}
			tasks = append(tasks, RoleTaskDTO{
				TaskName:             t.ID,
				CanEditInitialForm:   boolPtr(launch),
				RequiresDecisionForm: boolPtr(true),
				InitialTask:          boolPtr(false),
				TaskActions:          actions,
			})
		}
		docRoles := []DocRoleDTO{}
		for _, other := range groupOrder {
			perms := []string{"read", "write", "view", "share"}
			if other == g {
				perms = append(perms, "delete")
			}
			docRoles = append(docRoles, DocRoleDTO{RoleID: other, RoleName: other, DocPermissions: standardDocPerms(perms)})
		}
		viewAccess := []string{}
		for _, other := range groupOrder {
			if other != g {
				viewAccess = append(viewAccess, other)
			}
		}

		_, err := d.store.mutateApp(processID, func(app *AppConfig) error {
			rc := roleConfigFromDTO(RoleDTO{
				RoleName:           g,
				RoleID:             g,
				ApplyDeptFilter:    true,
				RoleCanLaunch:      boolPtr(launch),
				CanEditInitialForm: boolPtr(launch),
				DocRoles:           docRoles,
				RoleTasks:          tasks,
				ViewAccessRoles:    viewAccess,
			})
			found := false
			for i := range app.Roles {
				if app.Roles[i].RoleID == g {
					app.Roles[i] = rc
					found = true
					break
				}
			}
			if !found {
				app.Roles = append(app.Roles, rc)
			}
			return nil
		})
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "sync role "+g+": "+err.Error())
			return
		}
		synced = append(synced, g)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message":     "Roles synced from the engine design",
		"processId":   processID,
		"roles":       synced,
		"launchGroup": firstHumanGroup,
	})
}

// firstHumanGroup returns the group of the first HUMAN task reachable from
// the start (BFS over the topology), or "" if none.
func (d *DesignService) firstHumanGroup(def *domain.ProcessDefinition) string {
	seen := map[string]bool{}
	queue := []string{def.StartTaskID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		if t, ok := def.Tasks[id]; ok && t.Type == domain.TaskTypeHuman && t.Group != "" {
			return t.Group
		}
		for _, e := range def.Topology {
			if e.From == id && e.To != "" {
				queue = append(queue, e.To)
			}
		}
	}
	return ""
}

func standardActionItems(names []string) []ActionItemDTO {
	items := make([]ActionItemDTO, 0, len(names))
	for _, n := range names {
		items = append(items, ActionItemDTO{ActionName: n, ActionValue: boolPtr(true)})
	}
	return items
}

func standardDocPerms(ids []string) []DocPermissionDTO {
	out := make([]DocPermissionDTO, 0, len(ids))
	for _, id := range ids {
		out = append(out, DocPermissionDTO{ID: id, Permission: true})
	}
	return out
}

// ---------------------------------------------------------------------------
// POST /api/management/design/{processId}/sync-forms  (Designer)
//
// Creates a form step for every HUMAN task that has none (step_id = task id).
// Fields are derived from the task's input_schema (string→text,
// number→text, boolean→checkbox, enum→dropdown). Existing steps are never
// touched, so re-syncing is idempotent.
// ---------------------------------------------------------------------------

func (d *DesignService) syncForms(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("processId")
	def, err := d.reg.GetDefinition(r.Context(), processID)
	if err != nil || def == nil || len(def.TaskList) == 0 {
		writeErr(w, http.StatusNotFound, "No engine definition found for process "+processID)
		return
	}
	if _, ok := d.store.Get(processID); !ok {
		writeErr(w, http.StatusNotFound, "Process not found in the management store: "+processID)
		return
	}
	firstHuman := d.firstHumanTaskID(def)

	anyLaunchable := false
	for _, st := range func() (out []StepConfig) {
		if p, ok := d.store.Get(processID); ok {
			out = p.App.Forms.Steps
		}
		return out
	}() {
		if st.Launchable != nil && *st.Launchable {
			anyLaunchable = true
		}
	}
	var created []string
	_, err = d.store.mutateApp(processID, func(app *AppConfig) error {
		existing := map[string]bool{}
		for _, st := range app.Forms.Steps {
			existing[st.StepID] = true
		}
		for _, t := range def.TaskList {
			if t.Type != domain.TaskTypeHuman || existing[t.ID] {
				continue
			}
			existing[t.ID] = true // defensive: never create the same step twice
			row := 1
			fields := make([]FieldConfig, 0, 8)
			for name, f := range schemaFields(t.InputSchema) {
				fields = append(fields, FieldConfig{
					FieldName:     name,
					FieldLabel:    name,
					FieldType:     f,
					FieldTab:      "General",
					FieldRow:      intPtr(row),
					FieldRequired: boolPtr(true),
				})
				row++
			}
			// Only make the first HUMAN task launchable when the process has
			// no initial step yet — marking a new step launchable next to an
			// existing one with a different structure breaks the initial-step
			// consistency rule and blocks all later form edits.
			launchable := t.ID == firstHuman && !anyLaunchable
			app.Forms.Steps = append(app.Forms.Steps, StepConfig{
				StepID:     t.ID,
				Launchable: boolPtr(launchable),
				Fields:     fields,
			})
			created = append(created, t.ID)
		}
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "sync forms: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message":   "Form steps synced from the engine design",
		"processId": processID,
		"created":   created,
	})
}

// firstHumanTaskID returns the ID of the first HUMAN task reachable from
// the start (BFS), or "".
func (d *DesignService) firstHumanTaskID(def *domain.ProcessDefinition) string {
	seen := map[string]bool{}
	queue := []string{def.StartTaskID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		if t, ok := def.Tasks[id]; ok && t.Type == domain.TaskTypeHuman {
			return t.ID
		}
		for _, e := range def.Topology {
			if e.From == id && e.To != "" {
				queue = append(queue, e.To)
			}
		}
	}
	return ""
}

// schemaFields extracts {name: field_type} from a JSON-schema-style
// input_schema ({"type":"object","properties":{...}}).
func schemaFields(raw json.RawMessage) map[string]string {
	out := map[string]string{}
	if len(raw) == 0 {
		return out
	}
	var schema struct {
		Properties map[string]struct {
			Type        string   `json:"type"`
			Enum        []string `json:"enum"`
			Description string   `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil || schema.Properties == nil {
		return out
	}
	for name, p := range schema.Properties {
		switch {
		case len(p.Enum) > 0:
			out[name] = "dropdown"
		case p.Type == "boolean":
			out[name] = "checkbox"
		case p.Type == "number" || p.Type == "integer":
			out[name] = "text"
		default:
			out[name] = "text"
		}
	}
	return out
}

func intPtr(i int) *int { return &i }

// ---------------------------------------------------------------------------
// Permission enforcement
//
// The role configuration (role_tasks → task_actions → action_items) is the
// source of truth for what a group may DO on a task. CheckTaskAction is
// called by the HTTP layer before a task action executes:
//
//   - the caller's groups are matched against the process roles;
//   - if a role declares the task (exact id or underscore form) and the
//     requested action is present, the action must be enabled (true);
//   - an explicit false (or a missing action in a declared task) denies;
//   - roles that don't declare the task impose no restriction (the engine's
//     group check remains the baseline).
// ---------------------------------------------------------------------------

// CheckTaskAction reports whether ANY of the caller's groups is allowed to
// perform `action` (e.g. "Complete") on `taskName` of `processID`.
func (s *Store) CheckTaskAction(processID, taskName, action string, groups []string) error {
	return s.CheckTaskActionGroup(processID, taskName, "action", action, groups)
}

// CheckTaskActionGroup is the group-aware permission check: "action" covers
// Complete/Claim/Release/skip, "comment" covers read/write/update/delete on
// task comments, and "attachment" covers read/write/update/delete on task
// attachments. Same semantics: a role that declares the task must explicitly
// allow the action; roles that don't declare the task impose no restriction
// (the engine group check stays the baseline).
// CheckTaskActionGroup enforces the designer-configured permission for one
// (group, action) pair on a task, using the LIVE (working) app config.
func (s *Store) CheckTaskActionGroup(processID, taskName, group, action string, groups []string) error {
	return s.CheckTaskActionGroupVersion(processID, taskName, group, action, groups, 0)
}

// CheckTaskActionGroupVersion is the version-aware variant: version > 0
// checks the roles of the DEPLOYED design version the running instance was
// launched with, so an instance keeps the permissions it started with even
// after newer versions are deployed. version 0 = live config.
func (s *Store) CheckTaskActionGroupVersion(processID, taskName, group, action string, groups []string, version int) error {
	app := s.appForVersion(processID, version)
	if app == nil {
		return nil // no app config → engine group check is the baseline
	}
	declared := false
	for _, g := range groups {
		role := app.roleByID(strings.TrimPrefix(g, "/"))
		if role == nil {
			continue
		}
		for _, rt := range role.RoleTasks {
			if taskNameDiffers(rt.TaskName, taskName) {
				continue
			}
			declared = true
			for _, ag := range rt.ActionGroups {
				if ag.ActionGroup != group {
					continue
				}
				for _, it := range ag.Actions {
					if it.ActionName == action && it.ActionValue {
						return nil // allowed by this role
					}
					// explicit false on a declared task → deny (checked
					// after all groups so a second role may still grant)
				}
			}
		}
	}
	if declared {
		return fmt.Errorf("permission denied: role does not allow %s %q on task %q", group, action, taskName)
	}
	return nil
}
