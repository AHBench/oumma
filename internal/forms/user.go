package forms

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

var errSQLNoRows = sql.ErrNoRows

// RegisterRoutes mounts the oper-data-index endpoints under prefix
// (e.g. "/oper-api"), mirroring the Java @Path resources /user/forms,
// /management and /health.
func (s *Service) RegisterRoutes(mux *http.ServeMux, prefix string) {
	p := strings.TrimSuffix(prefix, "/")

	mux.HandleFunc("GET "+p+"/user/forms", s.handleUserForms)
	mux.HandleFunc("GET "+p+"/user/forms/searchSavedForms", s.handleSearchSavedForms)
	mux.HandleFunc("GET "+p+"/user/forms/searchform", s.handleSearchSavedForms)
	mux.HandleFunc("GET "+p+"/user/forms/getform", s.handleGetForm)
	mux.HandleFunc("GET "+p+"/user/forms/formversions", s.handleFormVersions)

	mux.HandleFunc("GET "+p+"/management/auth-test", s.handleAuthTest)
	mux.HandleFunc("POST "+p+"/management/processform", s.handleCreateProcessForm)
	mux.HandleFunc("PUT "+p+"/management/processform", s.handleUpdateProcessForm)
	mux.HandleFunc("GET "+p+"/management/processform", s.handleGetAllProcessForms)
	mux.HandleFunc("DELETE "+p+"/management/processform", s.handleDeleteProcessForm)
	mux.HandleFunc("DELETE "+p+"/management/processform/steps", s.handleDeleteSteps)

	mux.HandleFunc("GET "+p+"/health", s.handleHealth)
	mux.HandleFunc("GET "+p+"/health/live", s.handleHealthLive)
	mux.HandleFunc("GET "+p+"/health/ready", s.handleHealthReady)
	mux.HandleFunc("GET "+p+"/health/started", s.handleHealthStarted)
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func writeText(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg))
}

func writeErr(w http.ResponseWriter, e *apiErr) {
	if e == nil {
		return
	}
	writeText(w, e.status, e.msg)
}

func writeInternal(w http.ResponseWriter, err error) {
	writeText(w, http.StatusInternalServerError, err.Error())
}

func boolParam(values map[string][]string, key string) *bool {
	v, ok := values[key]
	if !ok || len(v) == 0 {
		return nil
	}
	b, err := strconv.ParseBool(v[0])
	if err != nil {
		return nil
	}
	return &b
}

func intParam(values map[string][]string, key string) int {
	if v, ok := values[key]; ok && len(v) > 0 {
		if n, err := strconv.Atoi(v[0]); err == nil {
			return n
		}
	}
	return 0
}

// handleUserForms mirrors UserController.getUserForms (GET /user/forms).
func (s *Service) handleUserForms(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	forms, err := s.userForms(r.Context(),
		q.Get("process_id"), q.Get("form_id"), q.Get("step_id"), q.Get("version"),
		boolParam(q, "activeform"), processDepartmentsList(q["departments"]))
	if err != nil {
		s.log.Error("load forms failed", "error", err)
		writeInternal(w, err)
		return
	}
	sanitizeProcessForms(forms)
	writeJSON(w, http.StatusOK, forms)
}

// userForms is the core of GET /user/forms, shared with the in-process LLM
// tool. Mirrors UserController.getUserForms.
func (s *Service) userForms(ctx context.Context, processID, formID, stepID, version string,
	activeForm *bool, departments []string) ([]*ProcessForm, error) {
	var result []*ProcessForm
	var err error

	// If form_id is provided, prioritize it.
	if strings.TrimSpace(formID) != "" {
		if strings.TrimSpace(stepID) != "" {
			if strings.TrimSpace(version) != "" {
				result, err = s.loadForms(ctx, ` WHERE st.form_id = $1 AND st.step_id = $2 AND st.version = $3`,
					formID, stepID, version)
			} else {
				// Latest version for this form_id and step_id (parent form).
				result, err = s.loadFormsByLatestStep(ctx, formID, stepID)
			}
		} else {
			// All steps with this form_id, newest version first.
			result, err = s.loadForms(ctx, ` WHERE st.form_id = $1`, formID)
		}
		if err == nil {
			result = applyActiveFilter(result, activeForm)
			result = applyDepartmentFilter(result, departments)
		}
		return result, err
	}

	if strings.TrimSpace(processID) != "" {
		if strings.TrimSpace(stepID) != "" {
			if strings.TrimSpace(version) != "" {
				result, err = s.loadForms(ctx, ` WHERE pf.process_id = $1 AND st.step_id = $2 AND st.version = $3`,
					processID, stepID, version)
				if err == nil {
					result = applyDepartmentFilter(result, departments)
				}
				return result, err
			}
			// Latest version for this specific process and step combination.
			result, err = s.loadForms(ctx, ` WHERE pf.process_id = $1 AND st.step_id = $2`, processID, stepID)
			if err == nil {
				for _, pf := range result {
					pf.reduceStepToLatest(stepID)
				}
				result = applyActiveFilter(result, activeForm)
				result = applyDepartmentFilter(result, departments)
			}
			return result, err
		}
		// All steps for this process with latest versions per form_id/step_id.
		result, err = s.loadForms(ctx, ` WHERE pf.process_id = $1`, processID)
		if err == nil {
			for _, pf := range result {
				pf.reduceToLatestVersions()
			}
			result = applyActiveFilter(result, activeForm)
			result = applyDepartmentFilter(result, departments)
		}
		return result, err
	}

	// No specific criteria: all forms.
	result, err = s.loadForms(ctx, "")
	if err == nil {
		result = applyActiveFilter(result, activeForm)
		result = applyDepartmentFilter(result, departments)
	}
	return result, err
}

// loadFormsByLatestStep returns the single parent form of the
// highest-version step matching form_id+step_id (Java firstResult semantics).
func (s *Service) loadFormsByLatestStep(ctx context.Context, formID, stepID string) ([]*ProcessForm, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`SELECT pf.id
		   FROM oper_process_forms pf JOIN oper_steps st ON st.process_form_id = pf.id
		  WHERE st.form_id = $1 AND st.step_id = $2
		  ORDER BY st.version DESC
		  LIMIT 1`, formID, stepID).Scan(&id)
	if err == errNoRows() {
		return []*ProcessForm{}, nil
	}
	if err != nil {
		return nil, err
	}
	return s.loadFormsByID(ctx, []int64{id})
}

func errNoRows() error { return errSQLNoRows }

func (s *Service) finishForms(w http.ResponseWriter, forms []*ProcessForm, err error) {
	if err != nil {
		s.log.Error("load forms failed", "error", err)
		writeInternal(w, err)
		return
	}
	sanitizeProcessForms(forms)
	writeJSON(w, http.StatusOK, forms)
}

// searchSavedForms mirrors UserController.searchSavedForms.
func (s *Service) searchSavedForms(ctx context.Context,
	query string, hasQuery bool,
	fieldName, fieldValue, processID, stepID string,
	limit int, activeForm *bool, departments, groups []string) ([]*ProcessForm, *apiErr) {

	// Group check (only when the caller requests specific groups).
	if len(groups) > 0 {
		_, userGroups, ok := s.claims(ctx)
		if !ok {
			return nil, textErr(http.StatusUnauthorized, "Invalid or expired token")
		}
		// Java: groups are truncated at the first "_" before comparison.
		prefixGroups := make([]string, 0, len(userGroups))
		for _, g := range userGroups {
			if i := strings.Index(g, "_"); i >= 0 {
				prefixGroups = append(prefixGroups, g[:i])
			} else {
				prefixGroups = append(prefixGroups, g)
			}
		}
		normRequested := make([]string, 0, len(groups))
		for _, g := range groups {
			if g = normalizeGroup(g); g != "" {
				normRequested = append(normRequested, g)
			}
		}
		normUser := make([]string, 0, len(prefixGroups))
		for _, g := range prefixGroups {
			if g = normalizeGroup(g); g != "" {
				normUser = append(normUser, g)
			}
		}
		intersects := false
		for _, a := range normUser {
			for _, b := range normRequested {
				if a == b {
					intersects = true
				}
			}
		}
		if !intersects && !isDesigner(userGroups) {
			return nil, textErr(http.StatusUnauthorized, "User not authorized for requested groups")
		}
	}

	// Fetch candidate forms, newest step version first.
	var candidates []*ProcessForm
	var err error
	if limit > 0 {
		// GROUP BY (not SELECT DISTINCT) so ORDER BY can use MAX(s.version):
		// Postgres forbids ordering a DISTINCT query by a non-selected column.
		q := `SELECT pf.id
		       FROM oper_process_forms pf JOIN oper_steps s ON s.process_form_id = pf.id`
		args := []any{}
		if strings.TrimSpace(processID) != "" {
			q += ` WHERE pf.process_id = $1`
			args = append(args, processID)
		}
		q += ` GROUP BY pf.id ORDER BY MAX(s.version) DESC LIMIT $` + strconv.Itoa(len(args)+1)
		args = append(args, limit)

		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, textErr(http.StatusInternalServerError, err.Error())
		}
		var orderedIDs []int64
		for rows.Next() && len(orderedIDs) < limit {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, textErr(http.StatusInternalServerError, err.Error())
			}
			orderedIDs = append(orderedIDs, id)
		}
		rows.Close()
		if len(orderedIDs) == 0 {
			candidates = []*ProcessForm{}
		} else {
			candidates, err = s.loadFormsByID(ctx, orderedIDs)
		}
	} else if strings.TrimSpace(processID) != "" {
		candidates, err = s.loadForms(ctx, ` WHERE pf.process_id = $1`, processID)
	} else {
		candidates, err = s.loadForms(ctx, "")
	}
	if err != nil {
		return nil, textErr(http.StatusInternalServerError, err.Error())
	}

	afterDept := applyDepartmentFilter(candidates, departments)

	// Filter by step_id if provided.
	if strings.TrimSpace(stepID) != "" {
		target := strings.TrimSpace(stepID)
		filtered := make([]*ProcessForm, 0, len(afterDept))
		for _, pf := range afterDept {
			keep := make([]*Step, 0, len(pf.Steps))
			found := false
			for _, st := range pf.Steps {
				if strv(st.StepID) == target {
					keep = append(keep, st)
					found = true
				}
			}
			if found {
				pf.Steps = keep
				filtered = append(filtered, pf)
			}
		}
		afterDept = filtered
	}

	// Reduce each ProcessForm to its latest version per form_id/step_id.
	for _, pf := range afterDept {
		pf.reduceToLatestVersions()
		if pf.CreatedAt == nil && len(pf.Steps) > 0 {
			var min *int64
			for _, st := range pf.Steps {
				if st.CreatedAt != nil {
					v := int64(*st.CreatedAt)
					if min == nil || v < *min {
						min = &v
					}
				}
			}
			if min != nil {
				v := InstantMillis(*min)
				pf.CreatedAt = &v
			}
		}
	}

	// activeForm default false → only forms with at least one step having a
	// processInstanceId. activeform=true → no restriction.
	activeParam := activeForm
	if activeParam == nil {
		f := false
		activeParam = &f
	}
	if !*activeParam {
		filtered := make([]*ProcessForm, 0, len(afterDept))
		for _, pf := range afterDept {
			for _, st := range pf.Steps {
				if st.HasProcessInstanceIDs() {
					filtered = append(filtered, pf)
					break
				}
			}
		}
		afterDept = filtered
	}

	// field_name / field_value filter.
	targetFieldName := strings.TrimSpace(fieldName)
	targetFieldValue := strings.TrimSpace(fieldValue)

	if targetFieldName != "" {
		targetNameNorm := strings.ToLower(targetFieldName)
		targetValNorm := strings.ToLower(targetFieldValue)
		if targetValNorm == "" {
			targetValNorm = strings.ToLower(strings.TrimSpace(query))
		}
		filtered := make([]*ProcessForm, 0, len(afterDept))
		for _, pf := range afterDept {
			if formMatchesField(pf, targetFieldName, targetNameNorm, targetValNorm) {
				filtered = append(filtered, pf)
			}
		}
		afterDept = filtered
	} else {
		// Standard token query matching.
		// Java: an explicitly empty query= param returns an empty list.
		if hasQuery && query == "" {
			return []*ProcessForm{}, nil
		}
		if strings.TrimSpace(query) != "" {
			var tokens []string
			for _, tok := range strings.Fields(strings.TrimSpace(query)) {
				tokens = append(tokens, strings.ToLower(tok))
			}
			filtered := make([]*ProcessForm, 0, len(afterDept))
			for _, pf := range afterDept {
				if formMatchesAllTokens(pf, tokens) {
					filtered = append(filtered, pf)
				}
			}
			afterDept = filtered
		}
	}

	return afterDept, nil
}

func formMatchesField(pf *ProcessForm, targetFieldName, targetNameNorm, targetValNorm string) bool {
	for _, st := range pf.Steps {
		inFields := false
		for _, f := range st.FormFields {
			if f.FieldName == nil || strings.ToLower(*f.FieldName) != targetNameNorm {
				continue
			}
			if targetValNorm == "" {
				inFields = true
				break
			}
			if f.FieldValue == nil {
				continue
			}
			if strings.Contains(strings.ToLower(fieldStringValue(f.FieldValue)), targetValNorm) {
				inFields = true
				break
			}
		}
		if inFields {
			return true
		}
		// Check the formInput JSON string.
		if strings.TrimSpace(st.FormInput) != "" {
			var root map[string]any
			if err := json.Unmarshal([]byte(st.FormInput), &root); err == nil {
				if _, has := root[targetFieldName]; has {
					if targetValNorm == "" {
						return true
					}
					if v, ok := root[targetFieldName]; ok && v != nil {
						if strings.Contains(strings.ToLower(fieldStringValue(v)), targetValNorm) {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

func formMatchesAllTokens(pf *ProcessForm, tokens []string) bool {
	for _, token := range tokens {
		found := false
		for _, st := range pf.Steps {
			inFields := false
			for _, f := range st.FormFields {
				if f.FieldValue != nil &&
					strings.Contains(strings.ToLower(fieldStringValue(f.FieldValue)), token) {
					inFields = true
					break
				}
			}
			if !inFields && st.FormInput != "" &&
				strings.Contains(strings.ToLower(st.FormInput), token) {
				inFields = true
			}
			if inFields {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (s *Service) handleSearchSavedForms(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	hasQuery := false
	if v, ok := q["query"]; ok && len(v) > 0 {
		hasQuery = true
	}
	forms, e := s.searchSavedForms(r.Context(),
		q.Get("query"), hasQuery,
		q.Get("field_name"), q.Get("field_value"),
		q.Get("process_id"), q.Get("step_id"),
		intParam(q, "limit"), boolParam(q, "activeform"),
		processDepartmentsList(q["departments"]), q["groups"])
	if e != nil {
		writeErr(w, e)
		return
	}
	sanitizeProcessForms(forms)
	writeJSON(w, http.StatusOK, forms)
}

// handleGetForm mirrors UserController.findLatestProcessFormByFormId.
func (s *Service) handleGetForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	formID := r.URL.Query().Get("form_id")
	if strings.TrimSpace(formID) == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("null"))
		return
	}

	var ids []int64
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT pf.id
		   FROM oper_process_forms pf JOIN oper_steps st ON st.process_form_id = pf.id
		  WHERE st.form_id = $1`, formID)
	if err != nil {
		writeInternal(w, err)
		return
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			writeInternal(w, err)
			return
		}
		ids = append(ids, id)
	}
	rows.Close()

	forms, err := s.loadFormsByID(ctx, ids)
	if err != nil {
		writeInternal(w, err)
		return
	}

	// Pick the form with the highest version among steps matching formId.
	var result *ProcessForm
	best := -1
	for _, pf := range forms {
		v := 0
		for _, st := range pf.Steps {
			if strv(st.FormID) == formID {
				if pv := parseVersion(strv(st.Version)); pv > v {
					v = pv
				}
			}
		}
		if v > best {
			best = v
			result = pf
		}
	}
	// Keep only the latest version for this formId.
	if result != nil {
		maxV := -1
		for _, st := range result.Steps {
			if strv(st.FormID) == formID {
				if pv := parseVersion(strv(st.Version)); pv > maxV {
					maxV = pv
				}
			}
		}
		if maxV >= 0 {
			kept := make([]*Step, 0, len(result.Steps))
			for _, st := range result.Steps {
				if strv(st.FormID) == formID && parseVersion(strv(st.Version)) == maxV {
					kept = append(kept, st)
				}
			}
			result.Steps = kept
		}
	}
	sanitizeProcessForms([]*ProcessForm{result})
	if result == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("null"))
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleFormVersions mirrors UserController.getUserFormsAllVersions.
func (s *Service) handleFormVersions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	processID := q.Get("process_id")
	formIDs := processDepartmentsList(q["form_id"])
	stepID := q.Get("step_id")
	version := q.Get("version")
	detailed := q.Get("detailed")
	departments := processDepartmentsList(q["departments"])

	latestOnly := strings.EqualFold(strings.TrimSpace(detailed), "no")

	var result []*ProcessForm
	var err error

	if len(formIDs) > 0 {
		if strings.TrimSpace(stepID) != "" {
			if strings.TrimSpace(version) != "" {
				result, err = s.loadFormsByFormIDs(ctx,
					` WHERE st.form_id = ANY($1) AND st.step_id = $2 AND st.version = $3`,
					[]any{formIDs, stepID, version})
			} else {
				result, err = s.loadFormsByFormIDs(ctx,
					` WHERE st.form_id = ANY($1) AND st.step_id = $2`, []any{formIDs, stepID})
				if err == nil && latestOnly {
					for _, pf := range result {
						pf.reduceToLatestVersions()
					}
				}
			}
		} else {
			result, err = s.loadFormsByFormIDs(ctx, ` WHERE st.form_id = ANY($1)`, []any{formIDs})
			if err == nil && latestOnly {
				for _, pf := range result {
					pf.reduceToLatestVersions()
				}
			}
		}
	} else if strings.TrimSpace(processID) != "" {
		if strings.TrimSpace(stepID) != "" {
			if strings.TrimSpace(version) != "" {
				result, err = s.loadForms(ctx,
					` WHERE pf.process_id = $1 AND st.step_id = $2 AND st.version = $3`,
					processID, stepID, version)
			} else {
				result, err = s.loadForms(ctx, ` WHERE pf.process_id = $1 AND st.step_id = $2`, processID, stepID)
				if err == nil && latestOnly {
					for _, pf := range result {
						pf.reduceToLatestVersions()
					}
				}
			}
		} else {
			result, err = s.loadForms(ctx, ` WHERE pf.process_id = $1`, processID)
			if err == nil && latestOnly {
				for _, pf := range result {
					pf.reduceToLatestVersions()
				}
			}
		}
	} else {
		result, err = s.loadForms(ctx, "")
		if err == nil && latestOnly {
			for _, pf := range result {
				pf.reduceToLatestVersions()
			}
		}
	}

	if err == nil {
		result = applyDepartmentFilter(result, departments)
	}
	s.finishForms(w, result, err)
}

// loadFormsByFormIDs loads forms filtered by a WHERE clause whose first
// parameter is the list of form ids (ANY($1)).
func (s *Service) loadFormsByFormIDs(ctx context.Context, where string, args []any) ([]*ProcessForm, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT pf.id, pf.process_id, pf.created_at
		   FROM oper_process_forms pf
		   LEFT JOIN oper_steps st ON st.process_form_id = pf.id
		   `+where+`
		  ORDER BY pf.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var order []int64
	byID := map[int64]*ProcessForm{}
	for rows.Next() {
		var (
			id        int64
			processID string
			createdAt sql.NullInt64
		)
		if err := rows.Scan(&id, &processID, &createdAt); err != nil {
			return nil, err
		}
		pf := &ProcessForm{ID: id, ProcessID: processID, Steps: []*Step{}}
		if createdAt.Valid {
			v := InstantMillis(createdAt.Int64)
			pf.CreatedAt = &v
		}
		byID[id] = pf
		order = append(order, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(order) == 0 {
		return []*ProcessForm{}, nil
	}
	steps, err := s.loadSteps(ctx, order)
	if err != nil {
		return nil, err
	}
	fields, err := s.loadFields(ctx, steps)
	if err != nil {
		return nil, err
	}
	for _, pf := range byID {
		for _, st := range steps {
			if st.ProcessFormID == pf.ID {
				st.FormFields = fields[st.ID]
				if st.FormFields == nil {
					st.FormFields = []*FormField{}
				}
				if st.ProcessInstanceIDs == nil {
					st.ProcessInstanceIDs = []string{}
				}
				pf.Steps = append(pf.Steps, st)
			}
		}
	}
	out := make([]*ProcessForm, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out, nil
}
