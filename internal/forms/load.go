package forms

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
)

// loadForms loads process forms (with steps and form fields) matching the
// given WHERE clause over the pf/st alias pair.
//
//	where: e.g. " WHERE pf.process_id = $1 AND st.step_id = $2" ("" for all)
func (s *Service) loadForms(ctx context.Context, where string, args ...any) ([]*ProcessForm, error) {
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
	for id, pf := range byID {
		_ = id
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

// loadSteps loads steps for the given process form ids.
func (s *Service) loadSteps(ctx context.Context, formIDs []int64) ([]*Step, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, process_form_id, form_id, step_id, version, process_instance_id,
		        step_name, step_description, launchable, form_input, created_at
		   FROM oper_steps WHERE process_form_id = ANY($1)
		  ORDER BY id`, formIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Step
	for rows.Next() {
		st := &Step{}
		var (
			formID        *string
			stepID        *string
			version       *string
			procInstRaw   sql.NullString
			stepName      *string
			stepDesc      *string
			formInput     sql.NullString
			createdAt     sql.NullInt64
		)
		if err := rows.Scan(&st.ID, &st.ProcessFormID, &formID, &stepID, &version, &procInstRaw,
			&stepName, &stepDesc, &st.Launchable, &formInput, &createdAt); err != nil {
			return nil, err
		}
		st.FormID, st.StepID, st.Version = formID, stepID, version
		st.StepName, st.StepDescription = stepName, stepDesc
		st.FormInput = formInput.String
		if formInput.Valid && st.FormInput == "" {
			st.FormInput = "{}"
		}
		if createdAt.Valid {
			v := InstantMillis(createdAt.Int64)
			st.CreatedAt = &v
		}
		if procInstRaw.Valid && procInstRaw.String != "" {
			st.ProcessInstanceRaw = procInstRaw.String
			st.ProcessInstanceIDs = parseInstanceIDs(procInstRaw.String)
		} else {
			st.ProcessInstanceIDs = []string{}
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// loadFields loads form fields for the given steps.
func (s *Service) loadFields(ctx context.Context, steps []*Step) (map[int64][]*FormField, error) {
	out := map[int64][]*FormField{}
	if len(steps) == 0 {
		return out, nil
	}
	ids := make([]int64, 0, len(steps))
	for _, st := range steps {
		ids = append(ids, st.ID)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, steps_id, field_row, field_tab, field_type, field_label, field_name,
		        field_value, field_required, field_unique, field_min_length,
		        field_header_display, field_edit, field_template, field_pattern,
		        field_source, field_source_type, field_table_name, user_can_add,
		        field_multple_choice, field_db_query, field_database_user,
		        field_database_password, api_key
		   FROM oper_form_fields WHERE steps_id = ANY($1)
		  ORDER BY id`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		f := &FormField{}
		var (
			stepID             int64
			fieldTab           *string
			fieldType          *string
			fieldLabel         *string
			fieldName          *string
			fieldValue         sql.NullString
			fieldMinLength     sql.NullInt32
			fieldHeaderDisplay sql.NullBool
			fieldEdit          sql.NullBool
			fieldTemplate      sql.NullString
			fieldPattern       *string
			fieldSource        *string
			fieldSourceType    *string
			fieldTableName     *string
			fieldDBQuery       *string
			fieldDBUser        *string
			fieldDBPass        *string
			apiKey             *string
		)
		if err := rows.Scan(&f.ID, &stepID, &f.FieldRow, &fieldTab, &fieldType, &fieldLabel,
			&fieldName, &fieldValue, &f.FieldRequired, &f.FieldUnique, &fieldMinLength,
			&fieldHeaderDisplay, &fieldEdit, &fieldTemplate, &fieldPattern,
			&fieldSource, &fieldSourceType, &fieldTableName, &f.UserCanAdd,
			&f.FieldMultpleChoice, &fieldDBQuery, &fieldDBUser, &fieldDBPass, &apiKey); err != nil {
			return nil, err
		}
		f.FieldTab, f.FieldType, f.FieldLabel, f.FieldName = fieldTab, fieldType, fieldLabel, fieldName
		if minValue := fieldMinLength.Int32; fieldMinLength.Valid {
			v := int(minValue)
			f.FieldMinLength = &v
		}
		f.FieldHeaderDisplay, f.FieldEdit = nullBool(fieldHeaderDisplay), nullBool(fieldEdit)
		f.FieldPattern, f.FieldSource, f.FieldSourceType, f.FieldTableName = fieldPattern, fieldSource, fieldSourceType, fieldTableName
		f.FieldDBQuery, f.FieldDatabaseUser, f.FieldDatabasePassword, f.APIKey = fieldDBQuery, fieldDBUser, fieldDBPass, apiKey
		if fieldValue.Valid && fieldValue.String != "" {
			var v any
			if err := json.Unmarshal([]byte(fieldValue.String), &v); err == nil {
				f.FieldValue = v
			} else {
				f.FieldValue = fieldValue.String
			}
		}
		if fieldTemplate.Valid && fieldTemplate.String != "" {
			var v any
			if err := json.Unmarshal([]byte(fieldTemplate.String), &v); err == nil {
				f.FieldTemplate = v
			} else {
				f.FieldTemplate = fieldTemplate.String
			}
		}
		f.FieldOptions, f.FieldTableFieldNames = []string{}, []string{}
		out[stepID] = append(out[stepID], f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Element collections (stored as JSON text arrays).
	if err := s.loadStringList(ctx, out,
		"SELECT step_field_id, value FROM oper_form_field_options WHERE step_field_id = ANY($1) ORDER BY ordinal",
		func(f *FormField, v string) { f.FieldOptions = append(f.FieldOptions, v) }); err != nil {
		return nil, err
	}
	if err := s.loadStringList(ctx, out,
		"SELECT step_field_id, value FROM oper_form_field_table_field_names WHERE step_field_id = ANY($1) ORDER BY ordinal",
		func(f *FormField, v string) { f.FieldTableFieldNames = append(f.FieldTableFieldNames, v) }); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) loadStringList(ctx context.Context, fields map[int64][]*FormField,
	query string, add func(*FormField, string)) error {
	ids := make([]int64, 0)
	for _, list := range fields {
		for _, f := range list {
			ids = append(ids, f.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, query, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := map[int64]*FormField{}
	for _, list := range fields {
		for _, f := range list {
			byID[f.ID] = f
		}
	}
	for rows.Next() {
		var fid int64
		var v string
		if err := rows.Scan(&fid, &v); err != nil {
			return err
		}
		if f := byID[fid]; f != nil {
			add(f, v)
		}
	}
	return rows.Err()
}

func nullBool(b sql.NullBool) *bool {
	if !b.Valid {
		return nil
	}
	v := b.Bool
	return &v
}

// parseInstanceIDs mirrors Steps.getProcessInstanceIds(): JSON array, legacy
// plain string, or the raw value on parse failure.
func parseInstanceIDs(raw string) []string {
	out := []string{}
	if strings.TrimSpace(raw) == "" {
		return out
	}
	var arr []any
	if err := json.Unmarshal([]byte(raw), &arr); err == nil {
		for _, v := range arr {
			if s, ok := v.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	var str string
	if err := json.Unmarshal([]byte(raw), &str); err == nil && strings.TrimSpace(str) != "" {
		return []string{str}
	}
	return []string{raw}
}

// sanitizeProcessForms mirrors UserController.sanitizeProcessForms: blanks
// sensitive form-field properties before serialization.
func sanitizeProcessForms(forms []*ProcessForm) {
	for _, pf := range forms {
		for _, st := range pf.Steps {
			for _, f := range st.FormFields {
				f.FieldDBQuery = nil
				f.FieldDatabaseUser = nil
				f.FieldDatabasePassword = nil
				f.APIKey = nil
			}
		}
	}
}

// reduceToLatestVersions keeps only the latest version per form_id/step_id
// pair (mirrors UserController.reduceToLatestVersions).
func (pf *ProcessForm) reduceToLatestVersions() {
	if len(pf.Steps) == 0 {
		return
	}
	type key struct{ form, step string }
	latest := map[key]*Step{}
	var order []*Step
	for _, st := range pf.Steps {
		k := key{strv(st.FormID), strv(st.StepID)}
		cur, ok := latest[k]
		if !ok || parseVersion(strv(st.Version)) > parseVersion(strv(cur.Version)) {
			if !ok {
				order = append(order, st)
			} else {
				for i, o := range order {
					if o == cur {
						order[i] = st
						break
					}
				}
			}
			latest[k] = st
		}
	}
	pf.Steps = order
}

// reduceStepToLatest keeps only the latest-version step for the given stepId
// (mirrors the ManagementController/UserController inline max-version logic).
func (pf *ProcessForm) reduceStepToLatest(targetStepID string) {
	if len(pf.Steps) == 0 {
		return
	}
	maxV := -1
	found := false
	for _, st := range pf.Steps {
		if strv(st.StepID) == targetStepID {
			v := parseVersion(strv(st.Version))
			if !found || v > maxV {
				maxV = v
				found = true
			}
		}
	}
	if !found {
		return
	}
	kept := make([]*Step, 0, len(pf.Steps))
	for _, st := range pf.Steps {
		if strv(st.StepID) == targetStepID && parseVersion(strv(st.Version)) != maxV {
			continue
		}
		kept = append(kept, st)
	}
	pf.Steps = kept
}

// processDepartmentsList mirrors UserController.processDepartmentsList.
func processDepartmentsList(departments []string) []string {
	out := []string{}
	for _, d := range departments {
		parts := strings.Split(d, ",")
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// extractDepartmentsFromFormInput mirrors
// UserController.extractDepartmentsFromFormInput: the formInput JSON
// "department" node as an array, a string, or an object with a "codes" array.
func extractDepartmentsFromFormInput(formInput string) []string {
	out := []string{}
	if strings.TrimSpace(formInput) == "" {
		return out
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(formInput), &root); err != nil {
		return out
	}
	dept, ok := root["department"]
	if !ok || dept == nil {
		return out
	}
	switch v := dept.(type) {
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
	case string:
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	case map[string]any:
		if codes, ok := v["codes"].([]any); ok {
			for _, item := range codes {
				if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// applyDepartmentFilter mirrors UserController.applyDepartmentFilter.
func applyDepartmentFilter(forms []*ProcessForm, departments []string) []*ProcessForm {
	if len(departments) == 0 {
		return forms
	}
	out := make([]*ProcessForm, 0, len(forms))
	for _, pf := range forms {
		if len(pf.Steps) == 0 {
			continue
		}
		keep := false
		for _, st := range pf.Steps {
			for _, d := range extractDepartmentsFromFormInput(st.FormInput) {
				for _, want := range departments {
					if d == want {
						keep = true
					}
				}
			}
			if keep {
				break
			}
		}
		if keep {
			out = append(out, pf)
		}
	}
	return out
}

// applyActiveFilter mirrors UserController.applyActiveFilter.
func applyActiveFilter(forms []*ProcessForm, activeForm *bool) []*ProcessForm {
	if activeForm == nil {
		return forms
	}
	out := make([]*ProcessForm, 0, len(forms))
	for _, pf := range forms {
		anyActive := false
		for _, st := range pf.Steps {
			if st.HasProcessInstanceIDs() {
				anyActive = true
				break
			}
		}
		if *activeForm == anyActive {
			out = append(out, pf)
		}
	}
	return out
}

// normalizeGroup mirrors UserController.normalizeGroup.
func normalizeGroup(group string) string {
	g := strings.TrimSpace(group)
	g = strings.TrimPrefix(g, "/")
	return strings.ToLower(g)
}

// isDesigner grants access to all groups.
func isDesigner(groups []string) bool {
	for _, g := range groups {
		if g == "Designer" || g == "/Designer" {
			return true
		}
	}
	return false
}

// loadFormsByID loads the given process forms (with steps/fields), in the
// given order.
func (s *Service) loadFormsByID(ctx context.Context, ids []int64) ([]*ProcessForm, error) {
	if len(ids) == 0 {
		return []*ProcessForm{}, nil
	}
	forms, err := s.loadForms(ctx, ` WHERE pf.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	byID := map[int64]*ProcessForm{}
	for _, pf := range forms {
		byID[pf.ID] = pf
	}
	out := make([]*ProcessForm, 0, len(ids))
	for _, id := range ids {
		if pf := byID[id]; pf != nil {
			out = append(out, pf)
		}
	}
	return out, nil
}

// fieldStringValue renders a field value the way Java's Object.toString()
// does for the supported JSON types.
func fieldStringValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
}
