package processconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"oumma/internal/auth"
)

// KeycloakAdmin is implemented by internal/auth.Service for the two
// Keycloak user-management endpoints.
type KeycloakAdmin interface {
	CreateUser(ctx context.Context, req auth.CreateUserRequest) (string, error)
	ConfirmEmail(ctx context.Context, userID string) error
}

// RegisterManagementRoutes mounts the ported ManagementController endpoints
// (Java @Path("/api/management") @RolesAllowed("/ProcessAdmin")).
func (s *Store) RegisterManagementRoutes(mux *http.ServeMux, kc KeycloakAdmin) {
	mux.HandleFunc("GET /api/management/extract-all", s.mgmtRequireAdmin(s.mgmtExtractAll))
	mux.HandleFunc("GET /api/management/extract-by-id/{processId}", s.mgmtRequireAdmin(s.mgmtExtractByID))
	mux.HandleFunc("GET /api/management/extract-by-role/{roleId}", s.mgmtRequireAdmin(s.mgmtExtractByRole))
	mux.HandleFunc("GET /api/management/search", s.mgmtRequireAdmin(s.mgmtSearch))
	mux.HandleFunc("GET /api/management/templates/{processId}", s.mgmtRequireAdmin(s.mgmtGetTemplates))
	mux.HandleFunc("POST /api/management/insert-process", s.mgmtRequireAdmin(s.mgmtInsertProcess))
	mux.HandleFunc("PUT /api/management/update-process/{processId}", s.mgmtRequireAdmin(s.mgmtUpdateProcess))
	mux.HandleFunc("POST /api/management/process/{processId}/icon", s.mgmtRequireAdmin(s.mgmtSetProcessIcon))
	mux.HandleFunc("DELETE /api/management/delete-process/{processId}", s.mgmtRequireAdmin(s.mgmtDeleteProcess))
	mux.HandleFunc("POST /api/management/add-role/{processId}", s.mgmtRequireAdmin(s.mgmtAddRole))
	mux.HandleFunc("POST /api/management/add-action-group/{processId}/{roleId}/{taskId}", s.mgmtRequireAdmin(s.mgmtAddActionGroup))
	mux.HandleFunc("POST /api/management/create-task-group", s.mgmtRequireAdmin(s.mgmtCreateTaskGroup))
	mux.HandleFunc("POST /api/management/add-task-items/{groupId}", s.mgmtRequireAdmin(s.mgmtAddTaskItems))
	mux.HandleFunc("DELETE /api/management/delete-task-group/{groupId}", s.mgmtRequireAdmin(s.mgmtDeleteTaskGroup))
	mux.HandleFunc("POST /api/management/add-forms", s.mgmtRequireAdmin(s.mgmtAddForms))
	mux.HandleFunc("PUT /api/management/form-step/{processId}/{stepId}", s.mgmtRequireAdmin(s.mgmtUpdateFormStep))
	mux.HandleFunc("POST /api/management/ai-params", s.mgmtRequireAdmin(s.mgmtAIParams))
	mux.HandleFunc("POST /api/management/add-search-item/{processId}/{roleId}", s.mgmtRequireAdmin(s.mgmtAddSearchItem))
	mux.HandleFunc("GET /api/management/search-items/{processId}", s.mgmtRequireAdmin(s.mgmtListSearchItems))
	mux.HandleFunc("POST /api/management/search-item/{processId}", s.mgmtRequireAdmin(s.mgmtUpsertSearchItem))
	mux.HandleFunc("DELETE /api/management/search-item/{processId}/{fieldName}", s.mgmtRequireAdmin(s.mgmtDeleteSearchItem))
	mux.HandleFunc("POST /api/management/add-authorised-search/{processId}/{roleId}", s.mgmtRequireAdmin(s.mgmtAddAuthorisedSearch))
	mux.HandleFunc("POST /api/management/add-view-access-roles/{processId}/{roleId}", s.mgmtRequireAdmin(s.mgmtAddViewAccessRoles))
	mux.HandleFunc("POST /api/management/add-template", s.mgmtRequireAdmin(s.mgmtAddTemplate))
	mux.HandleFunc("POST /api/management/create-user", s.mgmtRequireAdmin(s.mgmtCreateUser(kc)))
	mux.HandleFunc("POST /api/management/confirm-email/{userId}", s.mgmtRequireAdmin(s.mgmtConfirmEmail(kc)))
}

// mgmtRequireAdmin mirrors the Java class-level @RolesAllowed("/ProcessAdmin")
// (now the Designer group in oumma).
func (s *Store) mgmtRequireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := s.claims(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "Authentication required")
			return
		}
		if !s.isAdmin(c.Groups) {
			writeErr(w, http.StatusForbidden, "Access denied. Designer role required.")
			return
		}
		next(w, r)
	}
}

// mutateApp runs fn on a copy of the process's AppConfig and persists the
// result into the process YAML file. The process must already be in the store.
func (s *Store) mutateApp(processID string, fn func(app *AppConfig) error) (*AppProcess, error) {
	err := s.Update(processID, func(doc map[string]any) (map[string]any, error) {
		app, err := appFromMap(doc)
		if err != nil {
			return nil, err
		}
		if app == nil {
			app = &AppConfig{}
		}
		if err := fn(app); err != nil {
			return nil, err
		}
		m, err := appAsMap(app)
		if err != nil {
			return nil, err
		}
		doc["app"] = m
		return doc, nil
	})
	if err != nil {
		return nil, err
	}
	p, ok := s.Get(processID)
	if !ok {
		return nil, fmt.Errorf("process %s not found after update", processID)
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// reads
// ---------------------------------------------------------------------------

func (s *Store) mgmtExtractAll(w http.ResponseWriter, r *http.Request) {
	sub := jwtSub(r)
	out := make([]*ProcessDTO, 0)
	for _, p := range s.All() {
		if p.App.UserID == sub {
			out = append(out, p.toDTO())
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Store) mgmtExtractByID(w http.ResponseWriter, r *http.Request) {
	// The route is already gated by mgmtRequireAdmin (Designer role);
	// every designer may read any process's config. (The legacy owner check
	// p.App.UserID == sub broke the design view for every designer who is
	// not the user recorded in the YAML.)
	p, ok := s.Get(r.PathValue("processId"))
	if !ok {
		writeErr(w, http.StatusNotFound, "Process not found")
		return
	}
	writeJSON(w, http.StatusOK, p.toDTO())
}

func (s *Store) mgmtExtractByRole(w http.ResponseWriter, r *http.Request) {
	roleID := r.PathValue("roleId")
	out := make([]*ProcessDTO, 0)
	for _, p := range s.All() {
		if p.roleByID(roleID) != nil {
			out = append(out, p.toDTO())
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Store) mgmtSearch(w http.ResponseWriter, r *http.Request) {
	pattern := strings.TrimSpace(r.URL.Query().Get("name"))
	if pattern == "" {
		writeErr(w, http.StatusBadRequest, "Name pattern is required")
		return
	}
	lp := strings.ToLower(pattern)
	out := make([]*ProcessDTO, 0)
	for _, p := range s.All() {
		if strings.Contains(strings.ToLower(p.App.Name), lp) {
			out = append(out, p.toDTO())
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Store) mgmtGetTemplates(w http.ResponseWriter, r *http.Request) {
	p, ok := s.Get(r.PathValue("processId"))
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"templates": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": p.toTemplateItems()})
}

// ---------------------------------------------------------------------------
// process CRUD
// ---------------------------------------------------------------------------

type CreateProcessRequest struct {
	ProcessID          string   `json:"process_id"`
	ProcessName        string   `json:"process_name"`
	ProcessDescription string   `json:"process_description"`
	ProcessLink        string   `json:"process_link"`
	ProcessServiceName string   `json:"process_service_name"`
	ProcessPodName     string   `json:"process_pod_name"`
	ProcessIconPath    string   `json:"process_icon_path"`
	Company            string   `json:"company"`
	ProcessLaunchRoleID string  `json:"process_launch_role_id"`
	SharedProcessIDs   []string `json:"shared_process_ids"`
}

// generateProcessId ports the Java helper (name slug + millisecond timestamp).
func generateProcessID(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == ' ' {
			b.WriteRune(r)
		}
	}
	slug := strings.Join(strings.Fields(b.String()), "-")
	return slug + "-" + fmt.Sprintf("%d", time.Now().UnixMilli())
}

func (s *Store) mgmtInsertProcess(w http.ResponseWriter, r *http.Request) {
	var req CreateProcessRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if strings.TrimSpace(req.ProcessName) == "" {
		writeErr(w, http.StatusBadRequest, "Process name is required")
		return
	}
	processID := strings.TrimSpace(req.ProcessID)
	if processID == "" {
		processID = generateProcessID(req.ProcessName)
	}

	c, _ := s.claims(r)
	sub := jwtSub(r)

	_, existed := s.Get(processID)
	err := s.Update(processID, func(doc map[string]any) (map[string]any, error) {
		app, err := appFromMap(doc)
		if err != nil {
			return nil, err
		}
		if app == nil {
			app = &AppConfig{}
		}
		if existed {
			// Java clears nested relationships when re-inserting an existing
			// process; keep process-level fields only.
			app.Roles = nil
			app.Forms = FormsConfig{}
			app.AI = nil
			app.Templates = nil
			app.TaskGroups = nil
		}
		app.Name = req.ProcessName
		app.Link = req.ProcessLink
		app.ServiceName = req.ProcessServiceName
		app.PodName = req.ProcessPodName
		app.IconPath = req.ProcessIconPath
		app.Company = req.Company
		app.LaunchRoleID = req.ProcessLaunchRoleID
		app.SharedProcessIDs = req.SharedProcessIDs
		app.UserID = sub
		app.Username = c.Username
		if app.CanBeLaunched == nil {
			app.CanBeLaunched = boolPtr(true)
		}
		m, err := appAsMap(app)
		if err != nil {
			return nil, err
		}
		doc["app"] = m
		doc["process_id"] = processID
		if req.ProcessDescription != "" {
			doc["description"] = req.ProcessDescription
		}
		// New process = a new PROJECT: scaffold a minimal valid engine
		// design (START -> END) so the designer chat can build on it —
		// every subsequent prompt accumulates in the same pending version
		// until the user deploys it.
		if !existed {
			if _, hasTasks := doc["tasks"]; !hasTasks {
				doc["start_task_id"] = "start"
				doc["tasks"] = map[string]any{
					"start": map[string]any{"task_id": "start", "id": "start", "type": "START", "description": "Start"},
					"end":   map[string]any{"task_id": "end", "id": "end", "type": "END", "description": "End"},
				}
				doc["topology"] = []any{
					map[string]any{"from": "start", "to": "end", "condition": "always"},
				}
			}
		}
		return doc, nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Failed to create process: "+err.Error())
		return
	}
	p, _ := s.Get(processID)
	writeJSON(w, http.StatusCreated, p.toDTO())
}

// mgmtUpdateProcess ports the Java behaviour verbatim (it literally prefixes
// "Updated: " to the description and rewrites the link and first role name).
func (s *Store) mgmtUpdateProcess(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("processId")
	p, ok := s.Get(processID)
	if !ok {
		writeErr(w, http.StatusNotFound, "Process not found")
		return
	}
	var in struct {
		Name               string   `json:"name"`
		ProcessName        string   `json:"processName"`
		Description        string   `json:"description"`
		ProcessDescription string   `json:"processDescription"`
		Link               string   `json:"link"`
		Company            string   `json:"company"`
		ServiceName        string   `json:"serviceName"`
		PodName            string   `json:"podName"`
		CanBeLaunched      *bool    `json:"canBeLaunched"`
		LaunchRoleID       string   `json:"processLaunchRoleId"`
		SharedProcessIDs   []string `json:"sharedProcessIds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	_, err := s.mutateApp(processID, func(app *AppConfig) error {
		if in.ProcessName != "" {
			app.Name = in.ProcessName
		}
		if in.Name != "" {
			app.Name = in.Name
		}
		if in.ProcessDescription != "" {
			app.ProcessDescription = in.ProcessDescription
		}
		if in.Link != "" {
			app.Link = in.Link
		}
		if in.Company != "" {
			app.Company = in.Company
		}
		if in.ServiceName != "" {
			app.ServiceName = in.ServiceName
		}
		if in.PodName != "" {
			app.PodName = in.PodName
		}
		if in.CanBeLaunched != nil {
			app.CanBeLaunched = in.CanBeLaunched
		}
		if in.LaunchRoleID != "" {
			app.LaunchRoleID = in.LaunchRoleID
		}
		if in.SharedProcessIDs != nil {
			app.SharedProcessIDs = in.SharedProcessIDs
		}
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Failed to update process: "+err.Error())
		return
	}
	// The user-facing description lives in the engine section (top-level),
	// not the app block.
	if in.Description != "" {
		if err := s.Update(processID, func(doc map[string]any) (map[string]any, error) {
			doc["description"] = in.Description
			return doc, nil
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, "Failed to update description: "+err.Error())
			return
		}
	}
	p, _ = s.Get(processID)
	writeJSON(w, http.StatusOK, p.toDTO())
}

// mgmtSetProcessIcon sets the process's menu icon (a key from the app's
// built-in icon catalog; "" restores the default).
func (s *Store) mgmtSetProcessIcon(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("processId")
	var in struct {
		IconKey string `json:"icon_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	key := strings.TrimSpace(in.IconKey)
	if key != "" && len(key) > 32 {
		writeErr(w, http.StatusBadRequest, "icon_key must be a catalog key (max 32 chars)")
		return
	}
	for _, r := range key {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			writeErr(w, http.StatusBadRequest, "icon_key may only contain lowercase letters, digits, '-' and '_'")
			return
		}
	}
	if _, err := s.mutateApp(processID, func(app *AppConfig) error {
		app.IconKey = key
		return nil
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "Failed to set process icon: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"processId": processID, "icon_key": key})
}

func (s *Store) mgmtDeleteProcess(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("processId")
	_, ok := s.Get(processID)
	if !ok {
		writeErr(w, http.StatusNotFound, "Process not found")
		return
	}
	// Remove the app configuration; the engine workflow (if any) is kept so
	// the engine is never left with a broken definition.
	err := s.Update(processID, func(doc map[string]any) (map[string]any, error) {
		delete(doc, "app")
		return doc, nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Failed to delete process: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Process deleted successfully"})
}

// ---------------------------------------------------------------------------
// roles
// ---------------------------------------------------------------------------

type AddRoleRequest struct {
	RoleName           string         `json:"role_name"`
	RoleID             string         `json:"role_id"`
	ApplyDeptFilter    bool           `json:"apply_dept_filter"`
	RoleCanLaunch      *bool          `json:"role_can_launch"`
	CanEditInitialForm *bool          `json:"can_edit_initial_form"`
	DocRoles           []DocRoleDTO   `json:"doc_roles"`
	RoleTasks          []RoleTaskDTO  `json:"role_tasks"`
}

func roleConfigFromDTO(rd RoleDTO) RoleConfig {
	rc := RoleConfig{
		RoleID:             rd.RoleID,
		RoleName:           rd.RoleName,
		CanLaunch:          rd.RoleCanLaunch,
		CanEditInitialForm: rd.CanEditInitialForm != nil && *rd.CanEditInitialForm,
		ApplyDeptFilter:    rd.ApplyDeptFilter,
		ViewAccessRoles:    rd.ViewAccessRoles,
	}
	for _, dr := range rd.DocRoles {
		perms := make([]string, 0, len(dr.DocPermissions))
		for _, p := range dr.DocPermissions {
			perms = append(perms, p.ID)
		}
		rc.DocRoles = append(rc.DocRoles, DocRoleConfig{RoleID: dr.RoleID, RoleName: dr.RoleName, Permissions: perms})
	}
	for _, t := range rd.RoleTasks {
		tcfg := RoleTaskConfig{TaskName: t.TaskName}
		if t.CanEditInitialForm != nil {
			tcfg.CanEditInitialForm = *t.CanEditInitialForm
		}
		if t.InitialTask != nil {
			tcfg.InitialTask = *t.InitialTask
		}
		tcfg.RequiresDecisionForm = t.RequiresDecisionForm
		for _, ag := range t.TaskActions {
			g := ActionGroupConf{ActionGroup: ag.ActionGroup}
			for _, ai := range ag.ActionItems {
				v := false
				if ai.ActionValue != nil {
					v = *ai.ActionValue
				}
				g.Actions = append(g.Actions, ActionItemCfg{ActionName: ai.ActionName, ActionValue: v})
			}
			tcfg.ActionGroups = append(tcfg.ActionGroups, g)
		}
		rc.RoleTasks = append(rc.RoleTasks, tcfg)
	}
	return rc
}

func (s *Store) mgmtAddRole(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("processId")
	var req AddRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RoleName == "" || req.RoleID == "" {
		writeErr(w, http.StatusBadRequest, "Role name and role ID are required")
		return
	}
	p, ok := s.Get(processID)
	if !ok {
		writeErr(w, http.StatusNotFound, "Process not found")
		return
	}

	existing := p.roleByID(req.RoleID)
	_, err := s.mutateApp(processID, func(app *AppConfig) error {
		rc := roleConfigFromDTO(RoleDTO{
			RoleName:           req.RoleName,
			RoleID:             req.RoleID,
			ApplyDeptFilter:    req.ApplyDeptFilter,
			RoleCanLaunch:      req.RoleCanLaunch,
			CanEditInitialForm: req.CanEditInitialForm,
			DocRoles:           req.DocRoles,
			RoleTasks:          req.RoleTasks,
		})
		if existing == nil && rc.CanLaunch == nil {
			rc.CanLaunch = boolPtr(false)
		}
		if existing != nil {
			// replace in place (position preserved)
			for i := range app.Roles {
				if app.Roles[i].RoleID == req.RoleID {
					app.Roles[i] = rc
					break
				}
			}
		} else {
			app.Roles = append(app.Roles, rc)
		}
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Internal server error: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"message":   fmt.Sprintf("Role %s added successfully", req.RoleID),
		"processId": processID,
		"roleId":    req.RoleID,
		"roleName":  req.RoleName,
	})
}

func (s *Store) mgmtAddActionGroup(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("processId")
	roleID := r.PathValue("roleId")
	taskID := r.PathValue("taskId")
	var req TaskActionDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	p, ok := s.Get(processID)
	if !ok {
		writeErr(w, http.StatusNotFound, "Process not found")
		return
	}
	if p.roleByID(roleID) == nil {
		writeErr(w, http.StatusNotFound, "Role not found")
		return
	}
	found := false
	_, err := s.mutateApp(processID, func(app *AppConfig) error {
		for i := range app.Roles {
			if app.Roles[i].RoleID != roleID {
				continue
			}
			for j := range app.Roles[i].RoleTasks {
				if taskNameDiffers(app.Roles[i].RoleTasks[j].TaskName, taskID) {
					continue
				}
				found = true
				g := ActionGroupConf{ActionGroup: req.ActionGroup}
				for _, ai := range req.ActionItems {
					v := false
					if ai.ActionValue != nil {
						v = *ai.ActionValue
					}
					g.Actions = append(g.Actions, ActionItemCfg{ActionName: ai.ActionName, ActionValue: v})
				}
				app.Roles[i].RoleTasks[j].ActionGroups = append(app.Roles[i].RoleTasks[j].ActionGroups, g)
			}
		}
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Internal server error: "+err.Error())
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "Task not found")
		return
	}
	p2, _ := s.Get(processID)
	writeJSON(w, http.StatusOK, p2.toDTO())
}

// ---------------------------------------------------------------------------
// task groups
// ---------------------------------------------------------------------------

type TaskItemCreate struct {
	Order           *int   `json:"order"`
	ItemName        string `json:"item_name"`
	ItemDescription string `json:"item_description"`
	URLActive       *bool  `json:"url_active"`
	Detailed        *bool  `json:"detailed"`
	Atomic          *bool  `json:"atomic"`
	Format          string `json:"format"`
	FormatString    string `json:"format_string"`
	ItemColumnName  string `json:"item_column_name"`
}

func (t TaskItemCreate) toConfig() (TaskItemConfig, error) {
	if strings.TrimSpace(t.ItemName) == "" {
		return TaskItemConfig{}, fmt.Errorf("Item name is required")
	}
	if strings.TrimSpace(t.ItemColumnName) == "" {
		return TaskItemConfig{}, fmt.Errorf("Item column name is required")
	}
	if t.Order == nil {
		return TaskItemConfig{}, fmt.Errorf("Item order is required")
	}
	c := TaskItemConfig{
		Order:          *t.Order,
		ItemName:       t.ItemName,
		ItemDescription: t.ItemDescription,
		Atomic:         boolPtr(true),
		Format:         "String",
		FormatString:   "None",
		ItemColumnName: t.ItemColumnName,
	}
	if t.URLActive != nil {
		c.URLActive = *t.URLActive
	}
	if t.Detailed != nil {
		c.Detailed = *t.Detailed
	}
	if t.Atomic != nil {
		c.Atomic = t.Atomic
	}
	if t.Format != "" {
		c.Format = t.Format
	}
	if t.FormatString != "" {
		c.FormatString = t.FormatString
	}
	return c, nil
}

func (s *Store) mgmtCreateTaskGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GroupID   string           `json:"group_id"`
		TaskItems []TaskItemCreate `json:"task_items"`
		TaskView  []TaskItemCreate `json:"task_view"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.GroupID) == "" {
		writeErr(w, http.StatusBadRequest, "Group ID is required")
		return
	}
	groupID := strings.TrimSpace(req.GroupID)
	items, err := toConfigs(req.TaskItems)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Failed to create task group: "+err.Error())
		return
	}
	views, err := toConfigs(req.TaskView)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Failed to create task group: "+err.Error())
		return
	}
	_, err = s.mutateApp(groupID, func(app *AppConfig) error {
		app.TaskGroups = &TaskGroupsConfig{TaskItems: items, TaskView: views}
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Failed to create task group: "+err.Error())
		return
	}
	p, _ := s.Get(groupID)
	tg := p.toTaskGroupDTO()
	writeJSON(w, http.StatusCreated, tg)
}

func toConfigs(in []TaskItemCreate) ([]TaskItemConfig, error) {
	out := make([]TaskItemConfig, 0, len(in))
	for _, t := range in {
		c, err := t.toConfig()
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (s *Store) mgmtAddTaskItems(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("groupId")
	p, ok := s.Get(groupID)
	if !ok || p.App.TaskGroups == nil {
		writeErr(w, http.StatusNotFound, "TaskGroup not found")
		return
	}
	var req []TaskItemCreate
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req) == 0 {
		writeErr(w, http.StatusBadRequest, "Task items list cannot be empty")
		return
	}
	items, err := toConfigs(req)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Failed to add task items: "+err.Error())
		return
	}
	_, err = s.mutateApp(groupID, func(app *AppConfig) error {
		app.TaskGroups.TaskItems = append(app.TaskGroups.TaskItems, items...)
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Failed to add task items: "+err.Error())
		return
	}
	p2, _ := s.Get(groupID)
	writeJSON(w, http.StatusOK, p2.toTaskGroupDTO())
}

func (s *Store) mgmtDeleteTaskGroup(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("groupId")
	p, ok := s.Get(groupID)
	if !ok || p.App.TaskGroups == nil {
		writeErr(w, http.StatusNotFound, "TaskGroup not found")
		return
	}
	_, err := s.mutateApp(groupID, func(app *AppConfig) error {
		app.TaskGroups = nil
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Failed to delete task group: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "TaskGroup deleted successfully"})
}

// ---------------------------------------------------------------------------
// forms
// ---------------------------------------------------------------------------

type ProcessFormRequest struct {
	Data []ProcessFormDTO `json:"data"`
}

func (s *Store) mgmtAddForms(w http.ResponseWriter, r *http.Request) {
	var req ProcessFormRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Data) == 0 {
		writeErr(w, http.StatusBadRequest, "Process form data is required")
		return
	}
	processed := make([]string, 0, len(req.Data))
	for _, pf := range req.Data {
		processID := strings.TrimSpace(pf.ProcessID)
		if processID == "" {
			writeErr(w, http.StatusBadRequest, "Process ID is required for all processes")
			return
		}
		if _, ok := s.Get(processID); !ok {
			writeErr(w, http.StatusNotFound, "Process not found: "+processID)
			return
		}
		steps := make([]StepConfig, 0, len(pf.Steps))
		for _, st := range pf.Steps {
			if strings.TrimSpace(st.StepID) == "" {
				writeErr(w, http.StatusBadRequest, "Step ID is required for all steps")
				return
			}
			fields := make([]FieldConfig, 0, len(st.FormFields))
			for _, f := range st.FormFields {
				if err := validateFormField(f); err != nil {
					writeErr(w, http.StatusBadRequest, err.Error())
					return
				}
				fields = append(fields, fieldConfigFromDTO(f))
			}
			steps = append(steps, StepConfig{
				StepID:     st.StepID,
				Launchable: st.Launchable,
				FormInput:  st.FormInput,
				Fields:     fields,
			})
		}
		pid := processID
		if _, err := s.mutateApp(pid, func(app *AppConfig) error {
			// Upsert: a step with an existing step_id is replaced, so the
			// designer can toggle the "initial" (launchable) flag in place.
			kept := app.Forms.Steps[:0]
			for _, ex := range app.Forms.Steps {
				replaced := false
				for _, ns := range steps {
					if ex.StepID == ns.StepID {
						replaced = true
						break
					}
				}
				if !replaced {
					kept = append(kept, ex)
				}
			}
			merged := append(kept, steps...)
			// Reject only violations this change introduces — a pre-existing
			// inconsistency must not block every subsequent edit.
			if before := checkInitialStepsConsistent(app.Forms.Steps); before == nil {
				if err := checkInitialStepsConsistent(merged); err != nil {
					return err
				}
			}
			app.Forms.Steps = merged
			return nil
		}); err != nil {
			if err.Error() == "initial steps must share the same form structure" {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeErr(w, http.StatusInternalServerError, "Failed to add forms: "+err.Error())
			return
		}
		processed = append(processed, processID)
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"message":             "Forms added successfully to processes",
		"processed_processes": processed,
	})
}

// validateFormField ports the Java "external" field validation (the checks
// that guard the data-source wiring).
func validateFormField(f FormFieldDTO) error {
	if strings.TrimSpace(f.FieldName) == "" {
		return fmt.Errorf("Field name is required for all form fields")
	}
	if strings.TrimSpace(f.FieldType) == "" {
		return fmt.Errorf("Field type is required for all form fields")
	}
	if !strings.EqualFold(strings.TrimSpace(f.FieldType), "external") {
		return nil
	}
	srcType := strings.TrimSpace(strVal(f.FieldSourceType))
	if srcType == "" {
		return fmt.Errorf("fieldSourceType is required when fieldType is external")
	}
	valid := map[string]bool{"Database": true, "RestAPI": true, "Json": true, "TextFile": true, "Internal-API": true, "Internal-API-Config": true}
	if !valid[normaliseType(srcType)] {
		return fmt.Errorf("fieldSourceType must be one of: Database, RestAPI, Json, TextFile, Internal-API, Internal-API-Config")
	}
	switch normaliseType(srcType) {
	case "Database":
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(strVal(f.FieldSource))), "jdbc:") {
			return fmt.Errorf("fieldSource is required and must be a valid jdbc access url when type is Database")
		}
		if strVal(f.FieldDBQuery) == "" {
			return fmt.Errorf("fieldDbQuery is required when type is Database")
		}
		if strVal(f.FieldDatabaseUser) == "" {
			return fmt.Errorf("fieldDatabaseUser is required when type is Database")
		}
		if strVal(f.FieldDatabasePasswd) == "" {
			return fmt.Errorf("fieldDatabasePassword is required when type is Database")
		}
	case "RestAPI":
		if strVal(f.FieldSource) == "" {
			return fmt.Errorf("fieldSource is required when type is RestAPI")
		}
		if strVal(f.APIKey) == "" {
			return fmt.Errorf("apiKey is required when type is RestAPI")
		}
	case "Json":
		src := strVal(f.FieldSource)
		if src == "" {
			return fmt.Errorf("fieldSource is required when type is Json")
		}
		if !json.Valid([]byte(src)) {
			return fmt.Errorf("fieldSource must be a valid JSON string when type is Json")
		}
	case "Internal-API":
		src := strVal(f.FieldSource)
		if src == "" {
			return fmt.Errorf("fieldSource is required when type is Internal-API")
		}
		if strings.HasPrefix(strings.ToLower(src), "http://") || strings.HasPrefix(strings.ToLower(src), "https://") {
			return fmt.Errorf("fieldSource must be a relative URL when type is Internal-API")
		}
	case "Internal-API-Config":
		tbl := strVal(f.FieldNameTable)
		if tbl == "" {
			return fmt.Errorf("fieldTableName is required when type is Internal-API-Config")
		}
		if !strings.HasPrefix(tbl, "params_") {
			return fmt.Errorf("fieldTableName must start with 'params_' when type is Internal-API-Config")
		}
		if len(f.FieldTableFieldNames) == 0 {
			return fmt.Errorf("fieldTableFieldNames is required and must be a non-empty list when type is Internal-API-Config")
		}
	}
	if boolVal(f.FieldMultpleChoice) && !strings.EqualFold(srcType, "Internal-API-Config") {
		return fmt.Errorf("fieldMultpleChoice can only be true when fieldSourceType is Internal-API-Config")
	}
	return nil
}

func normaliseType(t string) string {
	switch strings.ToLower(t) {
	case "database":
		return "Database"
	case "restapi":
		return "RestAPI"
	case "json":
		return "Json"
	case "textfile":
		return "TextFile"
	case "internal-api":
		return "Internal-API"
	case "internal-api-config":
		return "Internal-API-Config"
	}
	return t
}

func strVal(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

func boolVal(p *bool) bool { return p != nil && *p }

func fieldConfigFromDTO(f FormFieldDTO) FieldConfig {
	return FieldConfig{
		FieldName:            f.FieldName,
		FieldLabel:           f.FieldLabel,
		FieldType:            f.FieldType,
		FieldTab:             f.FieldTab,
		FieldRow:             f.FieldRow,
		FieldRequired:        f.FieldRequired,
		FieldEdit:            f.FieldEdit,
		FieldHeaderDisplay:   f.FieldHeaderDisplay,
		FieldMinLength:       f.FieldMinLength,
		FieldMaxLength:       f.FieldMaxLength,
		FieldPattern:         strVal(f.FieldPattern),
		FieldOptions:         f.FieldOptions,
		FieldSource:          strVal(f.FieldSource),
		FieldSourceType:      strVal(f.FieldSourceType),
		FieldDBQuery:         strVal(f.FieldDBQuery),
		FieldDatabaseUser:    strVal(f.FieldDatabaseUser),
		FieldDatabasePasswd:  strVal(f.FieldDatabasePasswd),
		APIKey:               strVal(f.APIKey),
		FieldNameTable:       strVal(f.FieldNameTable),
		FieldTableFieldNames: f.FieldTableFieldNames,
		UserCanAdd:           f.UserCanAdd,
		FieldMultpleChoice:   f.FieldMultpleChoice,
		FieldUnique:          f.FieldUnique,
		FieldTemplate:        f.FieldTemplate,
	}
}

// ---------------------------------------------------------------------------
// ai params / search items / view access / templates
// ---------------------------------------------------------------------------

func (s *Store) mgmtAIParams(w http.ResponseWriter, r *http.Request) {
	var req BriefAIParamsDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if strings.TrimSpace(req.ProcessID) == "" {
		writeErr(w, http.StatusBadRequest, "Process ID is required")
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeErr(w, http.StatusBadRequest, "Model is required")
		return
	}
	if strings.TrimSpace(req.EmbeddingModel) == "" {
		writeErr(w, http.StatusBadRequest, "Embedding model is required")
		return
	}
	processID := strings.TrimSpace(req.ProcessID)
	p, ok := s.Get(processID)
	if !ok {
		writeErr(w, http.StatusNotFound, "Process not found: "+processID)
		return
	}
	for _, pr := range req.Prompts {
		if strings.TrimSpace(pr.Type) == "" {
			writeErr(w, http.StatusBadRequest, "Prompt type is required for all prompts")
			return
		}
		if strings.TrimSpace(pr.Name) == "" {
			writeErr(w, http.StatusBadRequest, "Prompt name is required for all prompts")
			return
		}
		if len(pr.Prompts) == 0 {
			writeErr(w, http.StatusBadRequest, "At least one child prompt is required for each AI prompt")
			return
		}
		for _, c := range pr.Prompts {
			if strings.TrimSpace(c.Role) == "" {
				writeErr(w, http.StatusBadRequest, "Prompt role is required for all child prompts")
				return
			}
			if strings.TrimSpace(c.Content) == "" {
				writeErr(w, http.StatusBadRequest, "Prompt content is required for all child prompts")
				return
			}
		}
	}
	wasUpdate := p.App.AI != nil
	cfg := &AIConfig{
		Model:          req.Model,
		EmbeddingModel: req.EmbeddingModel,
		ProcessDesc:    req.ProcessDesc,
	}
	for _, pr := range req.Prompts {
		pc := AIPromptConfig{Type: pr.Type, Name: pr.Name}
		for _, c := range pr.Prompts {
			pc.Prompts = append(pc.Prompts, PromptCfg{Role: c.Role, Content: c.Content})
		}
		cfg.Prompts = append(cfg.Prompts, pc)
	}
	_, err := s.mutateApp(processID, func(app *AppConfig) error {
		app.AI = cfg
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Failed to process AI parameters: "+err.Error())
		return
	}
	p2, _ := s.Get(processID)
	dto := p2.toAIParamsDTO()
	if wasUpdate {
		writeJSON(w, http.StatusOK, dto)
	} else {
		writeJSON(w, http.StatusCreated, dto)
	}
}

func (s *Store) mgmtAddSearchItem(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("processId")
	roleID := r.PathValue("roleId")
	var req SearchItemDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
		strings.TrimSpace(req.FieldName) == "" || strings.TrimSpace(req.FieldType) == "" {
		writeErr(w, http.StatusBadRequest, "Search item field name and type are required")
		return
	}
	p, ok := s.Get(processID)
	if !ok {
		writeErr(w, http.StatusNotFound, "Process not found")
		return
	}
	if p.roleByID(roleID) == nil {
		writeErr(w, http.StatusNotFound, "Role not found")
		return
	}
	name := strings.TrimSpace(req.FieldName)
	updated := false
	_, err := s.mutateApp(processID, func(app *AppConfig) error {
		for i := range app.Roles {
			if app.Roles[i].RoleID != roleID {
				continue
			}
			for j := range app.Roles[i].SearchItems {
				if strings.EqualFold(app.Roles[i].SearchItems[j].FieldName, name) {
					app.Roles[i].SearchItems[j].FieldType = strings.TrimSpace(req.FieldType)
					app.Roles[i].SearchItems[j].FieldLabelEN = req.FieldLabelEN
					app.Roles[i].SearchItems[j].FieldLabelFR = req.FieldLabelFR
					app.Roles[i].SearchItems[j].FieldLabelAR = req.FieldLabelAR
					updated = true
					return nil
				}
			}
			app.Roles[i].SearchItems = append(app.Roles[i].SearchItems, SearchItemConfig{
				FieldName:    name,
				FieldType:    strings.TrimSpace(req.FieldType),
				FieldLabelEN: req.FieldLabelEN,
				FieldLabelFR: req.FieldLabelFR,
				FieldLabelAR: req.FieldLabelAR,
			})
			return nil
		}
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Internal server error: "+err.Error())
		return
	}
	if updated {
		writeJSON(w, http.StatusOK, map[string]string{"message": "Search item updated successfully"})
	} else {
		writeJSON(w, http.StatusCreated, map[string]string{"message": "Search item added successfully"})
	}
}

func (s *Store) mgmtAddAuthorisedSearch(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("processId")
	roleID := r.PathValue("roleId")
	var req AuthorisedSearchInRoleDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.RoleID) == "" {
		writeErr(w, http.StatusBadRequest, "Authorised search roleId is required")
		return
	}
	p, ok := s.Get(processID)
	if !ok {
		writeErr(w, http.StatusNotFound, "Process not found")
		return
	}
	if p.roleByID(roleID) == nil {
		writeErr(w, http.StatusNotFound, "Role not found")
		return
	}
	_, err := s.mutateApp(processID, func(app *AppConfig) error {
		for i := range app.Roles {
			if app.Roles[i].RoleID == roleID {
				app.Roles[i].AuthorisedSearch = append(app.Roles[i].AuthorisedSearch, AuthorisedSearchItem{
					RoleID:          req.RoleID,
					RoleName:        req.RoleName,
					RoleDescription: req.RoleDescription,
				})
			}
		}
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Internal server error: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"message": "Authorised search role added successfully"})
}

func (s *Store) mgmtAddViewAccessRoles(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("processId")
	roleID := r.PathValue("roleId")
	var req []string
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		req = nil
	}
	p, ok := s.Get(processID)
	if !ok {
		writeErr(w, http.StatusNotFound, "Process not found")
		return
	}
	if p.roleByID(roleID) == nil {
		writeErr(w, http.StatusNotFound, "Role not found")
		return
	}
	_, err := s.mutateApp(processID, func(app *AppConfig) error {
		for i := range app.Roles {
			if app.Roles[i].RoleID == roleID {
				app.Roles[i].ViewAccessRoles = req
			}
		}
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Internal server error: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"message": "View access roles added successfully"})
}

func (s *Store) mgmtAddTemplate(w http.ResponseWriter, r *http.Request) {
	var req TemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "Request body is required")
		return
	}
	processID := strings.TrimSpace(req.ProcessID)
	if processID == "" {
		writeErr(w, http.StatusBadRequest, "Process ID is required")
		return
	}
	if strings.TrimSpace(req.TemplateID) == "" {
		writeErr(w, http.StatusBadRequest, "Template ID is required")
		return
	}
	if strings.TrimSpace(req.TemplateName) == "" {
		writeErr(w, http.StatusBadRequest, "Template name is required")
		return
	}
	if req.JSONTemplate == nil {
		writeErr(w, http.StatusBadRequest, "jsonTemplate is required")
		return
	}
	if strings.TrimSpace(req.TemplateType) == "" {
		writeErr(w, http.StatusBadRequest, "template_type is required")
		return
	}
	if _, ok := s.Get(processID); !ok {
		writeErr(w, http.StatusNotFound, "Process not found: "+processID)
		return
	}
	tpl := TemplateConfig{
		TemplateID:    strings.TrimSpace(req.TemplateID),
		Name:          strings.TrimSpace(req.TemplateName),
		Type:          req.TemplateType,
		JSONTemplate:  req.JSONTemplate,
		Filters:       req.Filters,
		AllowedGroups: req.AllowedGroups,
		AllowedSteps:  req.stepIDs(),
	}
	_, err := s.mutateApp(processID, func(app *AppConfig) error {
		for i := range app.Templates {
			if app.Templates[i].TemplateID == tpl.TemplateID {
				app.Templates[i] = tpl
				return nil
			}
		}
		app.Templates = append(app.Templates, tpl)
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Failed to save process template: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, templateToItem(processID, tpl))
}

// ---------------------------------------------------------------------------
// Keycloak user management (delegated to internal/auth)
// ---------------------------------------------------------------------------

func (s *Store) mgmtCreateUser(kc KeycloakAdmin) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req auth.CreateUserRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Username) == "" {
			writeErr(w, http.StatusBadRequest, "Username is required")
			return
		}
		if _, err := kc.CreateUser(r.Context(), req); err != nil {
			writeErr(w, http.StatusInternalServerError, "Failed to create user: "+err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"message": "User created successfully in Keycloak"})
	}
}

func (s *Store) mgmtConfirmEmail(kc KeycloakAdmin) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.PathValue("userId")
		if strings.TrimSpace(userID) == "" {
			writeErr(w, http.StatusBadRequest, "User ID is required")
			return
		}
		if err := kc.ConfirmEmail(r.Context(), userID); err != nil {
			writeErr(w, http.StatusInternalServerError, "Failed to confirm email: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "User email confirmed successfully in Keycloak"})
	}
}

// taskNameDiffers compares a role task name against a requested task id,
// tolerating the legacy underscore form for old configurations.
func taskNameDiffers(roleTaskName, requested string) bool {
	if roleTaskName == requested {
		return false
	}
	return strings.ReplaceAll(roleTaskName, "_", " ") != strings.ReplaceAll(requested, "_", " ")
}

// checkInitialStepsConsistent enforces the rule that all initial
// (launchable) steps of a process share the same form structure: the forms
// are "replicated with the same parameters", so two initial steps with
// different field structures are not allowed.
func checkInitialStepsConsistent(steps []StepConfig) error {
	var ref []string
	for _, st := range steps {
		active := st.Launchable != nil && *st.Launchable
		if !active {
			continue
		}
		names := make([]string, 0, len(st.Fields))
		for _, f := range st.Fields {
			names = append(names, f.FieldName)
		}
		sort.Strings(names)
		if ref == nil {
			ref = names
			continue
		}
		if !equalStringSlices(ref, names) {
			return errors.New("initial steps must share the same form structure")
		}
	}
	return nil
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// mgmtUpdateFormStep handles PUT /api/management/form-step/{processId}/{stepId}.
// Updates an existing form step in place: rename (new_step_id) and/or field,
// form_input and launchable changes. This is what makes step names
// editable from the designer (add-forms alone would only create duplicates).
func (s *Store) mgmtUpdateFormStep(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("processId")
	stepID := r.PathValue("stepId")
	var req struct {
		NewStepID  *string        `json:"new_step_id"`
		Launchable *bool          `json:"launchable"`
		FormInput  any            `json:"form_input"`
		FormFields []*FormFieldDTO `json:"form_fields"`
		HasFields  bool           `json:"has_fields"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if _, ok := s.Get(processID); !ok {
		writeErr(w, http.StatusNotFound, "Process not found: "+processID)
		return
	}
	_, err := s.mutateApp(processID, func(app *AppConfig) error {
		beforeSteps := make([]StepConfig, len(app.Forms.Steps))
		copy(beforeSteps, app.Forms.Steps)
		idx := -1
		for i, st := range app.Forms.Steps {
			if st.StepID == stepID {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("step %q not found in process %s", stepID, processID)
		}
		st := &app.Forms.Steps[idx]
		if req.NewStepID != nil && strings.TrimSpace(*req.NewStepID) != "" && strings.TrimSpace(*req.NewStepID) != st.StepID {
			for _, other := range app.Forms.Steps {
				if other.StepID == strings.TrimSpace(*req.NewStepID) {
					return fmt.Errorf("a step named %q already exists", strings.TrimSpace(*req.NewStepID))
				}
			}
			st.StepID = strings.TrimSpace(*req.NewStepID)
		}
		if req.Launchable != nil {
			st.Launchable = req.Launchable
		}
		if req.FormInput != nil {
			st.FormInput = req.FormInput
		}
		if req.HasFields {
			fields := make([]FieldConfig, 0, len(req.FormFields))
			for _, f := range req.FormFields {
				if err := validateFormField(*f); err != nil {
					return err
				}
				fields = append(fields, fieldConfigFromDTO(*f))
			}
			st.Fields = fields
		}
		// Reject only violations this change introduces — a pre-existing
		// inconsistency must not block every subsequent edit.
		if before := checkInitialStepsConsistent(beforeSteps); before == nil {
			return checkInitialStepsConsistent(app.Forms.Steps)
		}
		return nil
	})
	if err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "initial steps") {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"process_id": processID, "step_id": stepID, "updated": true})
}

// ── Process-level search-item catalog (designer) ────────────────────────────
// The catalog holds localized (EN/FR/AR) labels for everything the app
// renders on behalf of a process: menu items, form tab titles, process-data
// fields and form field labels. field_type is one of: menu_item | field |
// tab | field_label. The app resolves labels through this catalog first.

func (s *Store) mgmtListSearchItems(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("processId")
	p, ok := s.Get(processID)
	if !ok {
		writeErr(w, http.StatusNotFound, "Process not found")
		return
	}
	out := make([]SearchItemDTO, 0, len(p.App.SearchItems))
	for _, si := range p.App.SearchItems {
		out = append(out, SearchItemDTO{
			FieldName:    si.FieldName,
			FieldType:    si.FieldType,
			FieldLabelEN: si.FieldLabelEN,
			FieldLabelFR: si.FieldLabelFR,
			FieldLabelAR: si.FieldLabelAR,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Store) mgmtUpsertSearchItem(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("processId")
	var req SearchItemDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
		strings.TrimSpace(req.FieldName) == "" || strings.TrimSpace(req.FieldType) == "" {
		writeErr(w, http.StatusBadRequest, "field_name and field_type are required")
		return
	}
	if !isValidSearchKind(strings.TrimSpace(req.FieldType)) {
		writeErr(w, http.StatusBadRequest,
			"field_type must be one of: menu_item, tab, field_label, hint")
		return
	}
	if _, ok := s.Get(processID); !ok {
		writeErr(w, http.StatusNotFound, "Process not found: "+processID)
		return
	}
	name := strings.TrimSpace(req.FieldName)
	kind := strings.TrimSpace(req.FieldType)
	updated := false
	_, err := s.mutateApp(processID, func(app *AppConfig) error {
		for j := range app.SearchItems {
			if strings.EqualFold(app.SearchItems[j].FieldName, name) {
				app.SearchItems[j].FieldType = kind
				app.SearchItems[j].FieldLabelEN = req.FieldLabelEN
				app.SearchItems[j].FieldLabelFR = req.FieldLabelFR
				app.SearchItems[j].FieldLabelAR = req.FieldLabelAR
				updated = true
				return nil
			}
		}
		app.SearchItems = append(app.SearchItems, SearchItemConfig{
			FieldName:    name,
			FieldType:    kind,
			FieldLabelEN: req.FieldLabelEN,
			FieldLabelFR: req.FieldLabelFR,
			FieldLabelAR: req.FieldLabelAR,
		})
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Internal server error: "+err.Error())
		return
	}
	if updated {
		writeJSON(w, http.StatusOK, map[string]string{"message": "Search item updated successfully"})
	} else {
		writeJSON(w, http.StatusCreated, map[string]string{"message": "Search item added successfully"})
	}
}

func (s *Store) mgmtDeleteSearchItem(w http.ResponseWriter, r *http.Request) {
	processID := r.PathValue("processId")
	name := r.PathValue("fieldName")
	if _, ok := s.Get(processID); !ok {
		writeErr(w, http.StatusNotFound, "Process not found: "+processID)
		return
	}
	removed := false
	_, err := s.mutateApp(processID, func(app *AppConfig) error {
		kept := app.SearchItems[:0]
		for _, si := range app.SearchItems {
			if strings.EqualFold(si.FieldName, name) {
				removed = true
				continue
			}
			kept = append(kept, si)
		}
		app.SearchItems = kept
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Internal server error: "+err.Error())
		return
	}
	if !removed {
		writeErr(w, http.StatusNotFound, "Search item not found: "+name)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Search item deleted"})
}
