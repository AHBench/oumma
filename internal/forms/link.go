package forms

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// LinkInstance appends instanceID to the process_instance_id JSON list of
// the form's launchable step (fallback: every step carrying that form_id).
// This is the form↔instance link the old Java form service wrote at launch
// time; without it, launched forms never match activeform=true and the
// My-Dossiers grid stays empty.
//
// No-op (nil error) when the form_id is unknown — launches with a stale or
// external form id must not fail because of the link.
func (s *Service) LinkInstance(ctx context.Context, formID, instanceID string) error {
	formID = strings.TrimSpace(formID)
	instanceID = strings.TrimSpace(instanceID)
	if formID == "" || instanceID == "" {
		return nil
	}

	// Collect the current lists per matching step (launchable preferred).
	type stepRow struct {
		id      int64
		current []string
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, launchable, process_instance_id
		   FROM oper_steps
		  WHERE form_id = $1
		  ORDER BY launchable DESC, id`, formID)
	if err != nil {
		return fmt.Errorf("link instance: query steps: %w", err)
	}
	defer rows.Close()

	var stepRows []stepRow
	launchableSeen := false
	for rows.Next() {
		var (
			id       int64
			launch   bool
			procInst sql.NullString
		)
		if err := rows.Scan(&id, &launch, &procInst); err != nil {
			return fmt.Errorf("link instance: scan step: %w", err)
		}
		if !launch && launchableSeen {
			continue // a launchable step was found — skip the rest
		}
		if launch {
			launchableSeen = true
		}
		stepRows = append(stepRows, stepRow{id: id, current: parseInstanceIDs(procInst.String)})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("link instance: rows: %w", err)
	}
	if len(stepRows) == 0 {
		return nil // unknown form_id — nothing to link
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("link instance: begin tx: %w", err)
	}
	defer tx.Rollback()
	for _, sr := range stepRows {
		updated := append([]string{}, sr.current...)
		found := false
		for _, id := range updated {
			if id == instanceID {
				found = true
				break
			}
		}
		if !found {
			updated = append(updated, instanceID)
		}
		b, err := json.Marshal(updated)
		if err != nil {
			return fmt.Errorf("link instance: marshal ids: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE oper_steps SET process_instance_id = $2 WHERE id = $1`,
			sr.id, string(b)); err != nil {
			return fmt.Errorf("link instance: update step %d: %w", sr.id, err)
		}
	}
	return tx.Commit()
}
