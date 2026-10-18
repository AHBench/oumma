package forms

import "context"

// FormIDForStep returns the form_id of the newest saved step matching
// process+step. The form_id is the identifier generated when a form is saved
// or updated in oper-data (one uuid per step, shared across its versions).
func (s *Service) FormIDForStep(ctx context.Context, processID, stepID string) (string, bool) {
	var formID string
	err := s.db.QueryRowContext(ctx,
		`SELECT st.form_id
		   FROM oper_process_forms pf
		   JOIN oper_steps st ON st.process_form_id = pf.id
		  WHERE pf.process_id = $1 AND st.step_id = $2 AND st.form_id IS NOT NULL AND st.form_id <> ''
		  ORDER BY st.version DESC
		  LIMIT 1`, processID, stepID,
	).Scan(&formID)
	if err != nil {
		return "", false
	}
	return formID, true
}

// ValidFormID reports whether formID belongs to a saved step of processID —
// used to reject launches carrying a form_id the process does not own.
func (s *Service) ValidFormID(ctx context.Context, processID, formID string) bool {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1
		   FROM oper_process_forms pf
		   JOIN oper_steps st ON st.process_form_id = pf.id
		  WHERE pf.process_id = $1 AND st.form_id = $2
		  LIMIT 1`, processID, formID,
	).Scan(&one)
	return err == nil
}
