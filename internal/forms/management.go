package forms

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// randomUUID returns a random RFC 4122 version 4 UUID string.
func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// handleAuthTest mirrors ManagementController.testAuth.
func (s *Service) handleAuthTest(w http.ResponseWriter, r *http.Request) {
	username, _, ok := s.claims(r.Context())
	if !ok {
		writeText(w, http.StatusForbidden, "Invalid or expired token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message":       "Authentication successful",
		"hasValidToken": true,
		"username":      username,
	})
}

// handleGetAllProcessForms mirrors ManagementController.getAllProcessForms.
// (The Java "version" filter references a field that does not exist on
// ProcessForm and was never functional; it is ignored here as well.)
func (s *Service) handleGetAllProcessForms(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.claims(r.Context()); !ok {
		writeText(w, http.StatusForbidden, "Invalid or expired token")
		return
	}
	processID := r.URL.Query().Get("processId")
	var (
		forms []*ProcessForm
		err   error
	)
	if strings.TrimSpace(processID) != "" {
		forms, err = s.loadForms(r.Context(), ` WHERE pf.process_id = $1`, processID)
	} else {
		forms, err = s.loadForms(r.Context(), "")
	}
	if err != nil {
		writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, forms)
}

// requestFieldValue mirrors ManagementController.getFieldValue: the explicit
// field_value if present, otherwise the form_input[<field_name>] text.
func requestFieldValue(f *FormFieldRequest, step *StepRequest) *string {
	if f.FieldValue != nil {
		str := fieldStringValue(f.FieldValue)
		if str != "" {
			return &str
		}
	}
	if step != nil && step.FormInput != nil && f.FieldName != nil {
		root := inputAsObject(step.FormInput)
		if v, ok := root[*f.FieldName]; ok && v != nil {
			if s, ok := v.(string); ok && s != "" {
				return &s
			}
		}
	}
	return nil
}

// inputAsObject normalizes form_input (JSON string or object) to a map.
func inputAsObject(v any) map[string]any {
	switch t := v.(type) {
	case string:
		var m map[string]any
		if err := json.Unmarshal([]byte(t), &m); err == nil {
			return m
		}
		return nil
	case map[string]any:
		return t
	}
	return nil
}

// handleCreateProcessForm mirrors ManagementController.createProcessForm.
func (s *Service) handleCreateProcessForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, _, ok := s.claims(ctx); !ok {
		writeText(w, http.StatusForbidden, "Invalid or expired token")
		return
	}
	var req ProcessFormRequest
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeText(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if strings.TrimSpace(req.ProcessID) == "" {
		writeText(w, http.StatusBadRequest, "process_id is required")
		return
	}

	// field_unique precheck: an active form with the same unique field
	// value in this process/step short-circuits the creation.
	for _, step := range req.Steps {
		for _, f := range step.FormFields {
			if !f.FieldUnique {
				continue
			}
			val := requestFieldValue(f, step)
			existing, _ := s.searchSavedForms(ctx, "", false,
				strv(f.FieldName), strv(val), req.ProcessID, step.StepID,
				0, boolPtr(true), nil, nil)
			if len(existing) > 0 {
				var foundFormID string
				for _, pf := range existing {
					for _, st := range pf.Steps {
						if fid := strv(st.FormID); fid != "" {
							foundFormID = fid
							break
						}
					}
					if foundFormID != "" {
						break
					}
				}
				if foundFormID != "" {
					writeJSON(w, http.StatusOK, map[string]any{
						"formexists": "form already exists",
						"form_id":    foundFormID,
					})
					return
				}
			}
		}
	}

	created, err := s.createProcessForm(ctx, &req)
	if err != nil {
		s.log.Error("create process form failed", "error", err)
		writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// SaveFormTrace persists a form result as an operational trace (used by the
// engine's completion path to record decision-form outcomes in the oper
// store). Same persistence as createProcessForm.
func (s *Service) SaveFormTrace(ctx context.Context, req *ProcessFormRequest) (*ProcessForm, error) {
	if _, _, ok := s.claims(ctx); !ok {
		return nil, textErr(403, "Invalid or expired token")
	}
	return s.createProcessForm(ctx, req)
}

// createProcessForm persists a new process form with its steps/fields.
func (s *Service) createProcessForm(ctx context.Context, req *ProcessFormRequest) (*ProcessForm, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := nowMillis()

	var formID int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO oper_process_forms (process_id, created_at) VALUES ($1, $2) RETURNING id`,
		req.ProcessID, now).Scan(&formID)
	if err != nil {
		return nil, err
	}

	for _, stepReq := range req.Steps {
		procInstJSON, err := marshalInstanceIDs(stepReq.ProcessInstanceID)
		if err != nil {
			return nil, fmt.Errorf("process step %s: %w", stepReq.StepID, err)
		}
		formInput, err := marshalFormInput(stepReq.FormInput)
		if err != nil {
			return nil, fmt.Errorf("process step %s: %w", stepReq.StepID, err)
		}
		var stepID int64
		err = tx.QueryRowContext(ctx,
			`INSERT INTO oper_steps
				(process_form_id, form_id, step_id, version, process_instance_id,
				 step_name, step_description, launchable, form_input, created_at)
			 VALUES ($1, $2, $3, '1', $4, $5, $6, $7, $8, $9) RETURNING id`,
			formID, randomUUID(), stepReq.StepID, procInstJSON,
			stepReq.StepName, stepReq.StepDescription, stepReq.Launchable,
			formInput, now).Scan(&stepID)
		if err != nil {
			return nil, err
		}
		if err := s.insertFormFieldsTx(ctx, tx, stepID, stepReq.FormFields); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	forms, err := s.loadFormsByID(ctx, []int64{formID})
	if err != nil {
		return nil, err
	}
	if len(forms) == 0 {
		return nil, fmt.Errorf("created form not found")
	}
	return forms[0], nil
}

func marshalInstanceIDs(ids []string) (*string, error) {
	if ids == nil {
		return nil, nil
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	s := string(b)
	return &s, nil
}

func marshalFormInput(v any) (string, error) {
	if v == nil {
		return "{}", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// insertFormFieldsTx persists form field rows (and element collections).
func (s *Service) insertFormFieldsTx(ctx context.Context, tx *sql.Tx, stepID int64, fields []*FormFieldRequest) error {
	for _, f := range fields {
		fieldValue := jsonOrNull(f.FieldValue)
		fieldTemplate := jsonOrNull(f.FieldTemplate)
		fieldEdit := true
		if f.FieldEdit != nil {
			fieldEdit = *f.FieldEdit
		}
		var fid int64
		err := tx.QueryRowContext(ctx,
			`INSERT INTO oper_form_fields
				(steps_id, field_row, field_tab, field_type, field_label, field_name,
				 field_value, field_required, field_unique, field_min_length,
				 field_header_display, field_edit, field_template, field_pattern,
				 field_source, field_source_type, field_table_name, user_can_add,
				 field_multple_choice, field_db_query, field_database_user,
				 field_database_password, api_key)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)
			 RETURNING id`,
			stepID, f.FieldRow, f.FieldTab, f.FieldType, f.FieldLabel, f.FieldName,
			fieldValue, f.FieldRequired, f.FieldUnique, f.FieldMinLength,
			f.FieldHeaderDisplay, fieldEdit, fieldTemplate, f.FieldPattern,
			f.FieldSource, f.FieldSourceType, f.FieldTableName, f.UserCanAdd,
			f.FieldMultpleChoice, f.FieldDBQuery, f.FieldDatabaseUser,
			f.FieldDatabasePassword, f.APIKey).Scan(&fid)
		if err != nil {
			return err
		}
		if err := insertStringListTx(ctx, tx,
			"INSERT INTO oper_form_field_options (step_field_id, ordinal, value) VALUES ($1, $2, $3)",
			fid, f.FieldOptions); err != nil {
			return err
		}
		if err := insertStringListTx(ctx, tx,
			"INSERT INTO oper_form_field_table_field_names (step_field_id, ordinal, value) VALUES ($1, $2, $3)",
			fid, f.FieldTableFieldNames); err != nil {
			return err
		}
	}
	return nil
}

func insertStringListTx(ctx context.Context, tx *sql.Tx, query string, fieldID int64, values []string) error {
	for i, v := range values {
		if _, err := tx.ExecContext(ctx, query, fieldID, i, v); err != nil {
			return err
		}
	}
	return nil
}

func jsonOrNull(v any) *string {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	s := string(b)
	return &s
}

func boolPtr(b bool) *bool { return &b }

func (s *Service) deleteStepRows(ctx context.Context, tx *sql.Tx, stepIDs []int64) error {
	if len(stepIDs) == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM oper_form_field_options WHERE step_field_id IN (SELECT id FROM oper_form_fields WHERE steps_id = ANY($1))`, stepIDs); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM oper_form_field_table_field_names WHERE step_field_id IN (SELECT id FROM oper_form_fields WHERE steps_id = ANY($1))`, stepIDs); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM oper_form_fields WHERE steps_id = ANY($1)`, stepIDs); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM oper_steps WHERE id = ANY($1)`, stepIDs)
	return err
}

// insertDeletedAudit persists a ProcessFormDeleted audit row.
func (s *Service) insertDeletedAudit(ctx context.Context, tx *sql.Tx, st *Step, processID, reason string, at int64, by string) error {
	fieldsJSON := jsonOrNull(nil)
	if len(st.FormFields) > 0 {
		if b, err := json.Marshal(st.FormFields); err == nil {
			t := string(b)
			fieldsJSON = &t
		}
	}
	procInst := (*string)(nil)
	if st.ProcessInstanceRaw != "" {
		procInst = &st.ProcessInstanceRaw
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO oper_process_form_deleted
			(process_id, form_id, step_id, version, process_instance_id, step_name,
			 step_description, launchable, form_input, form_fields, deleted,
			 deleted_reason, deleted_at, deleted_by)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,TRUE,$11,$12,$13)`,
		processID, st.FormID, st.StepID, st.Version, procInst, st.StepName,
		st.StepDescription, st.Launchable, strPtr(st.FormInput), fieldsJSON,
		strPtr(reason), at, by)
	return err
}

// getNextVersion mirrors ManagementController.getNextVersion.
func (s *Service) getNextVersion(ctx context.Context, formID, stepID string) (string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT st.version
		   FROM oper_steps st WHERE st.form_id = $1 AND st.step_id = $2`, formID, stepID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	max := 0
	seen := false
	for rows.Next() {
		var v *string
		if err := rows.Scan(&v); err != nil {
			return "", err
		}
		pv := parseVersion(strv(v))
		if !seen || pv > max {
			max = pv
			seen = true
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if !seen {
		return "1", nil
	}
	return fmt.Sprintf("%d", max+1), nil
}

// validateFormField mirrors ManagementController.validateFormField
// (field_name/field_type required; json/json list template validation).
func validateFormField(f *FormFieldRequest) *apiErr {
	if f == nil {
		return textErr(http.StatusBadRequest, "Form field cannot be null")
	}
	if strings.TrimSpace(strv(f.FieldName)) == "" {
		return textErr(http.StatusBadRequest, "field_name is required for all form fields")
	}
	if strings.TrimSpace(strv(f.FieldType)) == "" {
		return textErr(http.StatusBadRequest, "field_type is required for all form fields")
	}
	fieldTypeNorm := strings.ToLower(strings.TrimSpace(strv(f.FieldType)))
	if fieldTypeNorm == "json list" || fieldTypeNorm == "json_list" || fieldTypeNorm == "json" {
		if f.FieldTemplate == nil {
			return nil
		}
		var node any
		switch t := f.FieldTemplate.(type) {
		case string:
			if err := json.Unmarshal([]byte(t), &node); err != nil {
				return textErr(http.StatusBadRequest, "Invalid field_template JSON format: " + err.Error())
			}
		default:
			node = t
		}

		validateItems := func(items []any) *apiErr {
			for _, item := range items {
				obj, ok := item.(map[string]any)
				if !ok {
					return textErr(http.StatusBadRequest, "Each item in field_template must be a JSON object")
				}
				fn, _ := obj["field_name"].(string)
				ft, _ := obj["field_type"].(string)
				if strings.TrimSpace(fn) == "" {
					return textErr(http.StatusBadRequest, "field_name is required for each item in field_template")
				}
				if strings.TrimSpace(ft) == "" {
					return textErr(http.StatusBadRequest, "field_type is required for each item in field_template")
				}
				if _, has := obj["field_label"]; has && obj["field_label"] == nil {
					return textErr(http.StatusBadRequest, "field_label cannot be null when specified in field_template")
				}
			}
			return nil
		}

		if fieldTypeNorm == "json list" || fieldTypeNorm == "json_list" {
			items, ok := node.([]any)
			if !ok {
				return textErr(http.StatusBadRequest, "field_template must be a JSON array of template items when field_type is 'json list'")
			}
			return validateItems(items)
		}
		// "json"
		if items, ok := node.([]any); ok {
			return validateItems(items)
		}
		if _, ok := node.(map[string]any); !ok {
			return textErr(http.StatusBadRequest, "field_template must be a JSON object or array when field_type is 'json'")
		}
	}
	return nil
}

// validateExternalFieldInput mirrors ManagementController.validateExternalFieldInput.
func validateExternalFieldInput(f *FormFieldRequest, step *StepRequest) *apiErr {
	if f == nil || !strings.EqualFold(strv(f.FieldType), "external") {
		return nil
	}
	if f.FieldName == nil || step == nil || step.FormInput == nil {
		return nil
	}
	root := inputAsObject(step.FormInput)
	if root == nil {
		if _, isString := step.FormInput.(string); isString {
			return textErr(http.StatusBadRequest, "Invalid form_input JSON format")
		}
		return nil
	}
	v, has := root[strv(f.FieldName)]
	if !has || v == nil {
		return nil
	}
	if f.FieldMultpleChoice {
		items, ok := v.([]any)
		if !ok {
			return textErr(http.StatusBadRequest, fmt.Sprintf(
				"form_input value for field '%s' must be a list of strings when field_multple_choice is true", *f.FieldName))
		}
		for _, item := range items {
			if _, ok := item.(string); !ok {
				return textErr(http.StatusBadRequest, fmt.Sprintf(
					"Every item in form_input value for field '%s' must be a string when field_multple_choice is true", *f.FieldName))
			}
		}
		return nil
	}
	if _, ok := v.(string); !ok {
		return textErr(http.StatusBadRequest, fmt.Sprintf(
			"form_input value for field '%s' must be a single string when field_multple_choice is false", *f.FieldName))
	}
	return nil
}

// handleUpdateProcessForm mirrors ManagementController.updateProcessForm.
func (s *Service) handleUpdateProcessForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, _, ok := s.claims(ctx); !ok {
		writeText(w, http.StatusForbidden, "Invalid or expired token")
		return
	}
	var req UpdateProcessFormRequest
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeText(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if strings.TrimSpace(req.ProcessID) == "" {
		writeText(w, http.StatusBadRequest, "process_id is required")
		return
	}
	if strings.TrimSpace(req.FormID) == "" {
		writeText(w, http.StatusBadRequest, "form_id is required")
		return
	}
	if req.Step == nil || strings.TrimSpace(req.Step.StepID) == "" {
		writeText(w, http.StatusBadRequest, "step with step_id is required")
		return
	}

	// Find the step with the exact form_id and step_id (any process form).
	all, err := s.loadForms(ctx, "")
	if err != nil {
		writeInternal(w, err)
		return
	}
	var targetPF *ProcessForm
	var targetStep *Step
	outer:
	for _, pf := range all {
		for _, st := range pf.Steps {
			if strv(st.FormID) == req.FormID && strv(st.StepID) == req.Step.StepID {
				targetPF, targetStep = pf, st
				break outer
			}
		}
	}
	if targetPF == nil || targetStep == nil {
		writeText(w, http.StatusNotFound,
			"No step found with form_id: "+req.FormID+", step_id: "+req.Step.StepID)
		return
	}

	// Verify unique fields were not changed.
	if uniqueFieldChanged(targetStep, &req) {
		writeText(w, http.StatusForbidden, "User doesn't have the authority to change these fields")
		return
	}

	// source="process": override the latest matching step in place.
	source := strings.TrimSpace(strv(req.Source))
	if source == "" {
		source = "user"
	}
	if strings.EqualFold(source, "process") {
		s.overrideStepInPlace(w, ctx, targetPF, &req)
		return
	}

	nextVersion, err := s.getNextVersion(ctx, req.FormID, req.Step.StepID)
	if err != nil {
		writeInternal(w, err)
		return
	}

	for _, f := range req.Step.FormFields {
		if e := validateFormField(f); e != nil {
			writeErr(w, e)
			return
		}
		if e := validateExternalFieldInput(f, req.Step); e != nil {
			writeErr(w, e)
			return
		}
	}

	procInstJSON, err := marshalInstanceIDs(req.Step.ProcessInstanceID)
	if err != nil {
		writeText(w, http.StatusBadRequest, "Error processing form_input: "+err.Error())
		return
	}
	formInput, err := marshalFormInput(req.Step.FormInput)
	if err != nil {
		writeText(w, http.StatusBadRequest, "Error processing form_input: "+err.Error())
		return
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		writeInternal(w, err)
		return
	}
	defer tx.Rollback()
	var newStepID int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO oper_steps
			(process_form_id, form_id, step_id, version, process_instance_id,
			 step_name, step_description, launchable, form_input, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
		targetPF.ID, req.FormID, req.Step.StepID, nextVersion, procInstJSON,
		req.Step.StepName, req.Step.StepDescription, req.Step.Launchable,
		formInput, nowMillis()).Scan(&newStepID)
	if err != nil {
		writeInternal(w, err)
		return
	}
	if err := s.insertFormFieldsTx(ctx, tx, newStepID, req.Step.FormFields); err != nil {
		writeInternal(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message":     "Step updated successfully",
		"process_id":  req.ProcessID,
		"form_id":     req.FormID,
		"new_version": nextVersion,
	})
}

// uniqueFieldChanged mirrors the PUT unique-field authority checks.
func uniqueFieldChanged(targetStep *Step, req *UpdateProcessFormRequest) bool {
	// Existing unique fields must not be removed, renamed, or changed.
	for _, existing := range targetStep.FormFields {
		if !existing.FieldUnique {
			continue
		}
		fname := strv(existing.FieldName)
		existingVal := stepFieldValue(existing, targetStep.FormInput)

		var incoming *FormFieldRequest
		for _, dto := range req.Step.FormFields {
			if fname == strv(dto.FieldName) {
				incoming = dto
				break
			}
		}
		if incoming == nil {
			return true
		}
		incomingVal := requestFieldValue(incoming, req.Step)
		if normalizeVal(existingVal) != normalizeVal(incomingVal) {
			return true
		}
	}
	// Incoming unique fields must already exist as unique fields.
	for _, dto := range req.Step.FormFields {
		if !dto.FieldUnique {
			continue
		}
		fname := strv(dto.FieldName)
		var existing *FormField
		for _, f := range targetStep.FormFields {
			if fname == strv(f.FieldName) {
				existing = f
				break
			}
		}
		if existing == nil || !existing.FieldUnique {
			return true
		}
	}
	return false
}

// stepFieldValue mirrors ManagementController.getStepFieldValue.
func stepFieldValue(field *FormField, formInputJSON string) *string {
	if field.FieldValue != nil {
		str := fieldStringValue(field.FieldValue)
		if str != "" {
			return &str
		}
	}
	if strings.TrimSpace(formInputJSON) != "" && field.FieldName != nil {
		var root map[string]any
		if err := json.Unmarshal([]byte(formInputJSON), &root); err == nil {
			if v, ok := root[strv(field.FieldName)]; ok && v != nil {
				if s, ok := v.(string); ok && s != "" {
					return &s
				}
			}
		}
	}
	return nil
}

func normalizeVal(v *string) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(*v)
}

// overrideStepInPlace mirrors ManagementController.overrideStepInPlace.
func (s *Service) overrideStepInPlace(w http.ResponseWriter, ctx context.Context, target *ProcessForm, req *UpdateProcessFormRequest) {
	var overrideStep *Step
	for _, st := range target.Steps {
		if strv(st.FormID) == req.FormID && strv(st.StepID) == req.Step.StepID &&
			(overrideStep == nil || parseVersion(strv(st.Version)) > parseVersion(strv(overrideStep.Version))) {
			overrideStep = st
		}
	}
	if overrideStep == nil {
		writeText(w, http.StatusNotFound,
			"No step found with form_id: "+req.FormID+", step_id: "+req.Step.StepID)
		return
	}

	for _, f := range req.Step.FormFields {
		if e := validateFormField(f); e != nil {
			writeErr(w, e)
			return
		}
		if e := validateExternalFieldInput(f, req.Step); e != nil {
			writeErr(w, e)
			return
		}
	}

	procInstJSON, err := marshalInstanceIDs(req.Step.ProcessInstanceID)
	if err != nil {
		writeText(w, http.StatusBadRequest, "Error processing form_input: "+err.Error())
		return
	}
	formInput, err := marshalFormInput(req.Step.FormInput)
	if err != nil {
		writeText(w, http.StatusBadRequest, "Error processing form_input: "+err.Error())
		return
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		writeInternal(w, err)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE oper_steps
		    SET step_name = $2, step_description = $3, launchable = $4,
		        process_instance_id = $5, form_input = $6
		  WHERE id = $1`,
		overrideStep.ID, req.Step.StepName, req.Step.StepDescription, req.Step.Launchable,
		procInstJSON, strPtr(formInput)); err != nil {
		writeInternal(w, err)
		return
	}
	// Replace the step's form fields (orphanRemoval semantics).
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM oper_form_field_options WHERE step_field_id IN (SELECT id FROM oper_form_fields WHERE steps_id = $1)`,
		overrideStep.ID); err != nil {
		writeInternal(w, err)
		return
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM oper_form_field_table_field_names WHERE step_field_id IN (SELECT id FROM oper_form_fields WHERE steps_id = $1)`,
		overrideStep.ID); err != nil {
		writeInternal(w, err)
		return
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM oper_form_fields WHERE steps_id = $1`, overrideStep.ID); err != nil {
		writeInternal(w, err)
		return
	}
	if err := s.insertFormFieldsTx(ctx, tx, overrideStep.ID, req.Step.FormFields); err != nil {
		writeInternal(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message":    "Step updated successfully",
		"process_id": req.ProcessID,
		"form_id":    req.FormID,
		"version":    strv(overrideStep.Version),
	})
}

// handleDeleteSteps mirrors ManagementController.deleteSteps.
func (s *Service) handleDeleteSteps(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	username, _, ok := s.claims(ctx)
	if !ok {
		writeText(w, http.StatusForbidden, "Invalid or expired token")
		return
	}
	q := r.URL.Query()
	processID := q.Get("process_id")
	formID := q.Get("form_id")
	stepID := q.Get("step_id")
	if strings.TrimSpace(processID) == "" {
		writeText(w, http.StatusBadRequest, "process_id is required")
		return
	}
	if strings.TrimSpace(formID) == "" {
		writeText(w, http.StatusBadRequest, "form_id is required")
		return
	}
	if strings.TrimSpace(stepID) == "" {
		writeText(w, http.StatusBadRequest, "step_id is required")
		return
	}

	// ProcessForm must exist for this process.
	var hasForm bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM oper_process_forms WHERE process_id = $1)`, processID).
		Scan(&hasForm)
	if err != nil {
		writeInternal(w, err)
		return
	}
	if !hasForm {
		writeText(w, http.StatusNotFound, "ProcessForm with process_id "+processID+" not found")
		return
	}

	steps, err := s.stepsByProcessAndForm(ctx, processID, formID)
	if err != nil {
		writeInternal(w, err)
		return
	}
	if len(steps) == 0 {
		writeText(w, http.StatusNotFound, "No steps found with form_id "+formID+" for process_id "+processID)
		return
	}
	for _, st := range steps {
		if st.HasProcessInstanceIDs() {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"message":    "Cannot delete steps because at least one matching step is attached to an active process",
				"process_id": processID,
				"form_id":    formID,
				"step_id":    stepID,
			})
			return
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		writeInternal(w, err)
		return
	}
	defer tx.Rollback()
	at := nowMillis()
	ids := make([]int64, 0, len(steps))
	for _, st := range steps {
		if err := s.insertDeletedAudit(ctx, tx, st, processID, "Step deleted via API", at, username); err != nil {
			writeInternal(w, err)
			return
		}
		ids = append(ids, st.ID)
	}
	if err := s.deleteStepRows(ctx, tx, ids); err != nil {
		writeInternal(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message":       "Steps deleted successfully",
		"process_id":    processID,
		"form_id":       formID,
		"step_id":       stepID,
		"deleted_count": len(ids),
	})
}

// stepsByProcessAndForm loads steps belonging to the process with the given
// form_id (Java: processForm.processId = ? and formId = ?).
func (s *Service) stepsByProcessAndForm(ctx context.Context, processID, formID string) ([]*Step, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT st.id, st.process_form_id, st.form_id, st.step_id, st.version,
		        st.process_instance_id, st.step_name, st.step_description,
		        st.launchable, st.form_input, st.created_at
		   FROM oper_steps st JOIN oper_process_forms pf ON pf.id = st.process_form_id
		  WHERE pf.process_id = $1 AND st.form_id = $2
		  ORDER BY st.id`, processID, formID)
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
		fields, err := s.loadFields(ctx, []*Step{st})
		if err != nil {
			return nil, err
		}
		st.FormFields = fields[st.ID]
		if st.FormFields == nil {
			st.FormFields = []*FormField{}
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// handleDeleteProcessForm mirrors ManagementController.deleteProcessForm.
func (s *Service) handleDeleteProcessForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	username, _, ok := s.claims(ctx)
	if !ok {
		writeText(w, http.StatusForbidden, "Invalid or expired token")
		return
	}
	var req ProcessFormRequest
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeText(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if strings.TrimSpace(req.ProcessID) == "" {
		writeText(w, http.StatusBadRequest, "process_id is required")
		return
	}
	if len(req.Steps) == 0 {
		writeText(w, http.StatusBadRequest, "At least one step is required to delete")
		return
	}

	var hasForm bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM oper_process_forms WHERE process_id = $1)`, req.ProcessID).
		Scan(&hasForm)
	if err != nil {
		writeInternal(w, err)
		return
	}
	if !hasForm {
		writeText(w, http.StatusNotFound, "Process form not found with process_id: "+req.ProcessID)
		return
	}

	var matchedStepIDs, requestedDeleteIDs, actuallyDeletedIDs []int64
	blockedNames := []string{}
	type audit struct {
		step   *Step
		reason string
	}
	var audits []audit

	for _, stepReq := range req.Steps {
		if strings.TrimSpace(stepReq.StepID) == "" || strv(stepReq.FormID) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"message":    "Each step must include step_id and form_id",
				"process_id": req.ProcessID,
			})
			return
		}
		matching, err := s.stepsByProcessAndForm(ctx, req.ProcessID, strv(stepReq.FormID))
		if err != nil {
			writeInternal(w, err)
			return
		}
		if len(matching) == 0 {
			continue
		}
		for _, st := range matching {
			matchedStepIDs = append(matchedStepIDs, st.ID)
		}
		hasActive := false
		for _, st := range matching {
			if st.HasProcessInstanceIDs() {
				hasActive = true
				break
			}
		}
		if hasActive {
			blockedNames = append(blockedNames, stepReq.StepID)
			continue
		}
		for _, st := range matching {
			audits = append(audits, audit{step: st, reason: strv(stepReq.DeletedReason)})
			requestedDeleteIDs = append(requestedDeleteIDs, st.ID)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		writeInternal(w, err)
		return
	}
	defer tx.Rollback()
	at := nowMillis()
	for _, a := range audits {
		if err := s.insertDeletedAudit(ctx, tx, a.step, req.ProcessID, a.reason, at, username); err != nil {
			writeInternal(w, err)
			return
		}
	}
	if len(requestedDeleteIDs) > 0 {
		if err := s.deleteStepRows(ctx, tx, requestedDeleteIDs); err != nil {
			writeInternal(w, err)
			return
		}
		actuallyDeletedIDs = requestedDeleteIDs
	}
	if err := tx.Commit(); err != nil {
		writeInternal(w, err)
		return
	}

	var responseDeletedAt any
	if len(audits) > 0 {
		responseDeletedAt = at
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message":                    "Process form deletion attempted",
		"process_id":                 req.ProcessID,
		"matched_step_ids":           matchedStepIDs,
		"requested_delete_step_ids":  requestedDeleteIDs,
		"actually_deleted_step_ids":  actuallyDeletedIDs,
		"deleted_steps_count":        len(actuallyDeletedIDs),
		"blocked_step_ids":           blockedNames,
		"deleted_at":                 responseDeletedAt,
	})
}
