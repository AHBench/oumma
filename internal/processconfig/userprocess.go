package processconfig

import (
	"strconv"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	"oumma/internal/handler"
)

// RegisterRoutes mounts the ported UserProcessController endpoints
// (Java @Path("/api/user-processes")).
func (s *Store) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/user-processes/my-accessible-processes", s.handleMyAccessibleProcesses)
	mux.HandleFunc("GET /api/user-processes/task-groups/{processId}", s.handleTaskGroupByProcess)
	mux.HandleFunc("GET /api/user-processes/task-groups", s.handleAllTaskGroups)
	mux.HandleFunc("GET /api/user-processes/forms/{processId}", s.handleProcessForms)
	mux.HandleFunc("GET /api/user-processes/forms/{processId}/{stepId}", s.handleProcessStep)
	mux.HandleFunc("GET /api/user-processes/ai-params/{processId}", s.handleAIParams)
	mux.HandleFunc("GET /api/user-processes/search-items/{processId}/{roleId}", s.handleSearchItems)
	mux.HandleFunc("GET /api/user-processes/authorised-search/{processId}/{roleId}", s.handleAuthorisedSearch)
	mux.HandleFunc("GET /api/user-processes/view-access-roles/{processId}/{roleId}", s.handleViewAccessRoles)
	mux.HandleFunc("GET /api/user-processes/templates/{processId}", s.handleGetTemplates)
	mux.HandleFunc("POST /api/user-processes/templates", s.handlePostTemplates)
	mux.HandleFunc("GET /api/user-processes/extract-all", s.handleExtractAll)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func (s *Store) claims(r *http.Request) (*handler.AuthClaims, bool) {
	return handler.ClaimsFromContext(r.Context())
}

func (s *Store) isAdmin(groups []string) bool {
	for _, g := range groups {
		g = strings.TrimPrefix(g, "/")
		if g == "Designer" {
			return true
		}
	}
	return false
}

// userHasRole reports whether any of the caller's groups matches roleID.
func (s *Store) userHasRole(groups []string, roleID string) bool {
	for _, g := range groups {
		if groupMatchesRole(g, roleID) {
			return true
		}
	}
	return false
}

// checkUserAccessToProcess ports the Java helper: admin bypass, otherwise
// any process role matching one of the user's groups grants access.
func (s *Store) checkUserAccess(processID string, groups []string) bool {
	if s.isAdmin(groups) {
		return true
	}
	p, ok := s.Get(processID)
	if !ok {
		return false
	}
	if len(groups) == 0 {
		return false
	}
	for _, rc := range p.App.Roles {
		if s.userHasRole(groups, rc.RoleID) {
			return true
		}
	}
	return false
}

// requireUser2 returns the caller's claims or emits the Java-style 401.
func (s *Store) requireUser2(w http.ResponseWriter, r *http.Request) (*handler.AuthClaims, bool) {
	c, ok := s.claims(r)
	if !ok || c.Username == "" {
		writeErr(w, http.StatusUnauthorized, "Authentication required")
		return nil, false
	}
	return c, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ---------------------------------------------------------------------------
// GET /my-accessible-processes
// ---------------------------------------------------------------------------

func (s *Store) handleMyAccessibleProcesses(w http.ResponseWriter, r *http.Request) {
	c, ok := s.claims(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "Authentication required")
		return
	}
	groups := c.Groups
	if len(groups) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{}})
		return
	}

	var accessible []*ProcessDTO
	for _, p := range s.All() {
		admin := s.isAdmin(groups)
		var matched []RoleConfig
		if admin {
			matched = p.App.Roles
		} else {
			for _, rc := range p.App.Roles {
				if s.userHasRole(groups, rc.RoleID) {
					matched = append(matched, rc)
				}
			}
			if len(matched) == 0 {
				continue
			}
		}

		dto := p.toDTO()
		// doc_roles is always the full role list of the process with all
		// permissions granted (Java controller behaviour).
		fullDocRoles := p.allRolesAsDocRoles()
		roles := make([]RoleDTO, 0, len(matched))
		for _, rc := range matched {
			rd := roleToDTO(rc)
			rd.DocRoles = fullDocRoles
			roles = append(roles, rd)
		}
		dto.Roles = roles
		accessible = append(accessible, dto)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": accessible})
}

// ---------------------------------------------------------------------------
// task groups
// ---------------------------------------------------------------------------

func (s *Store) handleTaskGroupByProcess(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireUser2(w, r)
	if !ok {
		return
	}
	processID := r.PathValue("processId")
	if !s.checkUserAccess(processID, c.Groups) {
		writeErr(w, http.StatusForbidden, "Access denied to process: "+processID)
		return
	}
	p, ok := s.Get(processID)
	if !ok || p.toTaskGroupDTO() == nil {
		writeErr(w, http.StatusNotFound, "Task group not found for process: "+processID)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": []any{p.toTaskGroupDTO()}})
}

func (s *Store) handleAllTaskGroups(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireUser2(w, r)
	if !ok {
		return
	}
	out := make([]any, 0)
	for _, p := range s.All() {
		if !s.isAdmin(c.Groups) && !s.checkUserAccess(p.ID, c.Groups) {
			continue
		}
		if tg := p.toTaskGroupDTO(); tg != nil {
			out = append(out, tg)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// requireUser2 is the real 401-emitting variant of the username check.

// ---------------------------------------------------------------------------
// forms
// ---------------------------------------------------------------------------

func (s *Store) handleProcessForms(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireUser2(w, r)
	if !ok {
		return
	}
	processID := r.PathValue("processId")
	if !s.checkUserAccess(processID, c.Groups) {
		writeErr(w, http.StatusForbidden, "Access denied to process: "+processID)
		return
	}
	p, ok := s.Get(processID)
	if !ok || len(p.App.Forms.Steps) == 0 {
		writeErr(w, http.StatusNotFound, "No forms found for process: "+processID)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": []any{p.toFormDTO()}})
}

// stepToDTO builds the step DTO from a StepConfig (any design version).
func stepToDTO(st StepConfig) *ProcessStepDTO {
	launchable := st.Launchable
	if launchable == nil {
		launchable = boolPtr(true)
	}
	sd := ProcessStepDTO{
		StepID:     st.StepID,
		Launchable: launchable,
		FormInput:  st.FormInput,
		FormFields: []FormFieldDTO{},
	}
	for _, f := range st.Fields {
		sd.FormFields = append(sd.FormFields, fieldToDTO(f))
	}
	return &sd
}

func (s *Store) handleProcessStep(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireUser2(w, r)
	if !ok {
		return
	}
	processID := r.PathValue("processId")
	stepID := r.PathValue("stepId")
	if !s.checkUserAccess(processID, c.Groups) {
		writeErr(w, http.StatusForbidden, "Access denied to process: "+processID)
		return
	}
	// Optional ?version=N: serve the step form as it was in a deployed
	// design version (running instances of that version render their own
	// form structure, not the latest one). 0/absent = live config.
	version, _ := strconv.Atoi(r.URL.Query().Get("version"))
	if version > 0 {
		if st := s.StepForVersion(processID, stepID, version); st != nil {
			writeJSON(w, http.StatusOK, map[string]any{"data": stepToDTO(*st)})
			return
		}
		writeErr(w, http.StatusNotFound, "Step not found for process: "+processID+", step: "+stepID+" (version "+r.URL.Query().Get("version")+")")
		return
	}
	p, ok := s.Get(processID)
	if !ok {
		writeErr(w, http.StatusNotFound, "Step not found for process: "+processID+", step: "+stepID)
		return
	}
	step := p.stepDTO(stepID)
	if step == nil {
		writeErr(w, http.StatusNotFound, "Step not found for process: "+processID+", step: "+stepID)
		return
	}
	if fid := s.formIDFor(processID, stepID); fid != "" {
		step.FormID = fid
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": step})
}

// ---------------------------------------------------------------------------
// ai params
// ---------------------------------------------------------------------------

func (s *Store) handleAIParams(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireUser2(w, r)
	if !ok {
		return
	}
	processID := r.PathValue("processId")
	if !s.checkUserAccess(processID, c.Groups) {
		writeErr(w, http.StatusForbidden, "Access denied to process: "+processID)
		return
	}
	p, ok := s.Get(processID)
	if !ok {
		writeErr(w, http.StatusNotFound, "No AI parameters found for process: "+processID)
		return
	}
	ai := p.toAIParamsDTO()
	if ai == nil {
		writeErr(w, http.StatusNotFound, "No AI parameters found for process: "+processID)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": []any{ai}})
}

// ---------------------------------------------------------------------------
// search items / authorised search / view access roles
//
// All three accept a comma-separated role list, dedupe in first-seen order,
// return 404 when no requested role exists and 403 when none is authorized,
// and answer with a bare JSON array (no {data} wrapper) — Java behaviour.
// ---------------------------------------------------------------------------

type roleAccessOutcome int

const (
	roleNotFound roleAccessOutcome = iota
	roleDenied
	roleOK
)

// evaluateRoleAccess walks the comma-separated role ids exactly like the
// Java controller: foundAny / authorizedAny flags.
func (s *Store) evaluateRoleAccess(processID string, roleIDs string, groups []string) (*AppProcess, []string, roleAccessOutcome) {
	p, ok := s.Get(processID)
	if !ok {
		return nil, nil, roleNotFound
	}
	ids := strings.Split(roleIDs, ",")
	found := make([]string, 0, len(ids))
	authorized := false
	foundAny := false
	for _, raw := range ids {
		rid := strings.TrimSpace(raw)
		if p.roleByID(rid) == nil {
			continue
		}
		foundAny = true
		if s.isAdmin(groups) || s.userHasRole(groups, rid) {
			authorized = true
			found = append(found, rid)
		}
	}
	if !foundAny {
		return p, nil, roleNotFound
	}
	if !authorized {
		return p, nil, roleDenied
	}
	return p, found, roleOK
}

func (s *Store) handleSearchItems(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireUser2(w, r)
	if !ok {
		return
	}
	p, roleIDs, outcome := s.evaluateRoleAccess(r.PathValue("processId"), r.PathValue("roleId"), c.Groups)
	if outcome == roleNotFound {
		writeErr(w, http.StatusNotFound, "Role not found")
		return
	}
	if outcome == roleDenied {
		writeErr(w, http.StatusForbidden, "Access denied to requested roles")
		return
	}
	seen := map[string]struct{}{}
	out := make([]SearchItemDTO, 0)
	// Process-level catalog first (menu items, tabs, fields, field labels);
	// role-level items follow and override on name collision.
	for _, si := range p.App.SearchItems {
		if _, dup := seen[si.FieldName]; dup {
			continue
		}
		seen[si.FieldName] = struct{}{}
		out = append(out, SearchItemDTO{
			FieldName:    si.FieldName,
			FieldType:    si.FieldType,
			FieldLabelEN: si.FieldLabelEN,
			FieldLabelFR: si.FieldLabelFR,
			FieldLabelAR: si.FieldLabelAR,
		})
	}
	for _, rid := range roleIDs {
		rc := p.roleByID(rid)
		for _, si := range rc.SearchItems {
			if _, dup := seen[si.FieldName]; dup {
				continue
			}
			seen[si.FieldName] = struct{}{}
			out = append(out, SearchItemDTO{
				FieldName:    si.FieldName,
				FieldType:    si.FieldType,
				FieldLabelEN: si.FieldLabelEN,
				FieldLabelFR: si.FieldLabelFR,
				FieldLabelAR: si.FieldLabelAR,
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Store) handleAuthorisedSearch(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireUser2(w, r)
	if !ok {
		return
	}
	p, roleIDs, outcome := s.evaluateRoleAccess(r.PathValue("processId"), r.PathValue("roleId"), c.Groups)
	if outcome == roleNotFound {
		writeErr(w, http.StatusNotFound, "Role not found")
		return
	}
	if outcome == roleDenied {
		writeErr(w, http.StatusForbidden, "Access denied to requested roles")
		return
	}
	seen := map[string]struct{}{}
	out := make([]AuthorisedSearchInRoleDTO, 0)
	for _, rid := range roleIDs {
		rc := p.roleByID(rid)
		for _, a := range rc.AuthorisedSearch {
			if _, dup := seen[a.RoleID]; dup {
				continue
			}
			seen[a.RoleID] = struct{}{}
			out = append(out, AuthorisedSearchInRoleDTO{
				RoleID:          a.RoleID,
				RoleDescription: a.RoleDescription,
				RoleName:        a.RoleName,
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Store) handleViewAccessRoles(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireUser2(w, r)
	if !ok {
		return
	}
	_, roleIDs, outcome := s.evaluateRoleAccess(r.PathValue("processId"), r.PathValue("roleId"), c.Groups)
	if outcome == roleNotFound {
		writeErr(w, http.StatusNotFound, "Role not found")
		return
	}
	if outcome == roleDenied {
		writeErr(w, http.StatusForbidden, "Access denied to requested roles")
		return
	}
	seen := map[string]struct{}{}
	out := make([]string, 0)
	for _, rid := range roleIDs {
		for _, v := range s.getRoleForView(r, rid) {
			if _, dup := seen[v]; dup {
				continue
			}
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// getRoleForView re-fetches the role's view_access_roles list.
func (s *Store) getRoleForView(r *http.Request, rid string) []string {
	p, ok := s.Get(r.PathValue("processId"))
	if !ok {
		return nil
	}
	rc := p.roleByID(rid)
	if rc == nil {
		return nil
	}
	return rc.ViewAccessRoles
}

// ---------------------------------------------------------------------------
// templates
// ---------------------------------------------------------------------------

func (s *Store) templateAllowedGroups(tpl TemplateConfig, groups []string, admin bool, requested []string) bool {
	if len(tpl.AllowedGroups) == 0 || admin {
		return true
	}
	for _, g := range tpl.AllowedGroups {
		gt := strings.TrimSpace(g)
		for _, ug := range groups {
			ugT := strings.TrimPrefix(ug, "/")
			if ug == gt || ugT == gt || strings.SplitN(ugT, "_", 2)[0] == gt {
				return true
			}
		}
		for _, req := range requested {
			if strings.EqualFold(strings.TrimSpace(req), gt) {
				return true
			}
		}
	}
	return false
}

func (s *Store) templateAllowedSteps(tpl TemplateConfig, admin bool, requested []string) bool {
	if len(tpl.AllowedSteps) == 0 || admin {
		return true
	}
	for _, req := range requested {
		req = strings.TrimSpace(req)
		for _, ts := range tpl.AllowedSteps {
			if strings.EqualFold(strings.TrimSpace(ts), req) {
				return true
			}
		}
	}
	return false
}

func (s *Store) handleGetTemplates(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireUser2(w, r)
	if !ok {
		return
	}
	processID := r.PathValue("processId")
	if !s.checkUserAccess(processID, c.Groups) {
		writeErr(w, http.StatusForbidden, "Access denied to process: "+processID)
		return
	}
	var requestedGroups []string
	if q := r.URL.Query().Get("allowed_groups"); q != "" {
		for _, g := range strings.Split(q, ",") {
			requestedGroups = append(requestedGroups, g)
		}
	}
	s.writeTemplates(w, processID, c.Groups, requestedGroups, nil)
}

func (s *Store) handlePostTemplates(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProcessID      string   `json:"processId"`
		AllowedGroups  []string `json:"allowedGroups"`
		AllowedSteps   []struct {
			StepID string `json:"stepId"`
		} `json:"allowedSteps"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if strings.TrimSpace(req.ProcessID) == "" {
		writeErr(w, http.StatusBadRequest, "Process ID is required")
		return
	}
	var stepIDs []string
	for _, s2 := range req.AllowedSteps {
		stepIDs = append(stepIDs, s2.StepID)
	}
	c, ok := s.requireUser2(w, r)
	if !ok {
		return
	}
	processID := strings.TrimSpace(req.ProcessID)
	if !s.checkUserAccess(processID, c.Groups) {
		writeErr(w, http.StatusForbidden, "Access denied to process: "+processID)
		return
	}
	s.writeTemplates(w, processID, c.Groups, req.AllowedGroups, stepIDs)
}

func (s *Store) writeTemplates(w http.ResponseWriter, processID string, groups []string, reqGroups, reqSteps []string) {
	p, ok := s.Get(processID)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"templates": []any{}})
		return
	}
	admin := s.isAdmin(groups)
	items := make([]TemplateItemDTO, 0, len(p.App.Templates))
	for _, t := range p.App.Templates {
		if !s.templateAllowedGroups(t, groups, admin, reqGroups) {
			continue
		}
		if !s.templateAllowedSteps(t, admin, reqSteps) {
			continue
		}
		items = append(items, templateToItem(processID, t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": items})
}

// ---------------------------------------------------------------------------
// extract-all — processes "owned" by the caller's JWT subject.
// The session claims do not carry the sub, so it is read straight from the
// (already validated) Bearer token payload.
// ---------------------------------------------------------------------------

func (s *Store) handleExtractAll(w http.ResponseWriter, r *http.Request) {
	sub := jwtSub(r)
	out := make([]*ProcessDTO, 0)
	for _, p := range s.All() {
		if p.App.UserID == sub {
			out = append(out, p.toDTO())
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// templates request DTO (shared with the management port)
// ---------------------------------------------------------------------------

// TemplateRequest is the Java ProcessTemplateRequestDTO wire shape.
type TemplateRequest struct {
	ProcessID    string   `json:"processId"`
	TemplateID   string   `json:"templateId"`
	TemplateName string   `json:"templateName"`
	JSONTemplate any      `json:"jsonTemplate"`
	TemplateType string   `json:"templateType"`
	Filters      any      `json:"filters"`
	AllowedGroups []string `json:"allowedGroups"`
	AllowedSteps []struct {
		StepID string `json:"stepId"`
	} `json:"allowedSteps"`
}

func (t TemplateRequest) stepIDs() []string {
	out := make([]string, 0, len(t.AllowedSteps))
	for _, s := range t.AllowedSteps {
		out = append(out, s.StepID)
	}
	return out
}

func jwtSub(r *http.Request) string {
	hdr := r.Header.Get("Authorization")
	tok := strings.TrimPrefix(hdr, "Bearer ")
	parts := strings.SplitN(tok, ".", 3)
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		Sub string `json:"sub"`
	}
	_ = json.Unmarshal(payload, &c)
	return c.Sub
}

