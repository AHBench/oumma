package processconfig

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"oumma/internal/repository"
)

const formsRollbackProcYAML = `process_id: formsrb
start_task_id: start
end_task_ids: [end]
tasks:
  - task_id: start
    type: START
  - task_id: step1
    type: HUMAN
  - task_id: end
    type: END
topology:
  - from: start
    to: step1
  - from: step1
    to: end
app:
  name: FormsRB
  forms:
    steps:
      - step_id: s1
        launchable: true
        fields:
          - field_name: f1
            field_label: F1
            field_type: text
`

// deployReq runs the real deploy handler against the service.
func deployReq(t *testing.T, d *DesignService, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/management/design/deploy", bytes.NewBufferString(body))
	d.deployDesign(w, r)
	return w
}

func liveFormFields(t *testing.T, s *Store, processID string) []string {
	t.Helper()
	p, ok := s.Get(processID)
	if !ok {
		t.Fatalf("process %s not found", processID)
	}
	var out []string
	for _, st := range p.App.Forms.Steps {
		out = append(out, st.StepID+":")
		for _, f := range st.Fields {
			out = append(out, f.FieldName)
		}
	}
	return out
}

// Deploying a version must roll the live form CONFIGURATION back to that
// version's snapshot (form instances live per instance and are untouched).
func TestDeployRollsBackFormConfiguration(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "formsrb.yaml"), []byte(formsRollbackProcYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	repo := repository.NewInMemoryStore()
	d := NewDesignService(store, repo, nil, nil, slog.New(slog.DiscardHandler))
	ctx := t.Context()

	// 1. Snapshot the current design (forms: s1[f1]) and deploy it.
	d.takeSnapshot(ctx, "formsrb", "tester", 1)
	if w := deployReq(t, d, `{"processId":"formsrb"}`); w.Code != 200 {
		t.Fatalf("first deploy: %d %s", w.Code, w.Body.String())
	}

	// 2. Edit the live form: add a field to the deployed version.
	if _, err := store.mutateApp("formsrb", func(app *AppConfig) error {
		app.Forms.Steps[0].Fields = append(app.Forms.Steps[0].Fields, FieldConfig{
			FieldName: "f2", FieldLabel: "F2", FieldType: "text",
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := liveFormFields(t, store, "formsrb"); len(got) != 3 || got[1] != "f1" || got[2] != "f2" {
		t.Fatalf("after live edit, expected s1:[f1 f2], got %v", got)
	}

	// 3. Roll back to the deployed version 1 — the form edit must revert.
	if w := deployReq(t, d, `{"processId":"formsrb","version":1}`); w.Code != 200 {
		t.Fatalf("rollback deploy: %d %s", w.Code, w.Body.String())
	}
	if got := liveFormFields(t, store, "formsrb"); len(got) != 2 || got[1] != "f1" {
		t.Fatalf("rollback must restore the version's forms, got %v", got)
	}
}

// Deploying the PENDING version captures the CURRENT live forms (the deploy
// re-snapshots first), so a forward deploy does not lose fresh form edits.
func TestDeployPendingKeepsCurrentForms(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "formsrb.yaml"), []byte(formsRollbackProcYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	repo := repository.NewInMemoryStore()
	d := NewDesignService(store, repo, nil, nil, slog.New(slog.DiscardHandler))
	ctx := t.Context()

	// First deploy (forms: s1[f1]).
	d.takeSnapshot(ctx, "formsrb", "tester", 1)
	if w := deployReq(t, d, `{"processId":"formsrb"}`); w.Code != 200 {
		t.Fatalf("first deploy: %d %s", w.Code, w.Body.String())
	}
	// Edit forms, then deploy the pending version — the edit must survive.
	if _, err := store.mutateApp("formsrb", func(app *AppConfig) error {
		app.Forms.Steps[0].Fields = append(app.Forms.Steps[0].Fields, FieldConfig{
			FieldName: "f2", FieldLabel: "F2", FieldType: "text",
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if w := deployReq(t, d, `{"processId":"formsrb"}`); w.Code != 200 {
		t.Fatalf("second deploy: %d %s", w.Code, w.Body.String())
	}
	if got := liveFormFields(t, store, "formsrb"); len(got) != 3 || got[1] != "f1" || got[2] != "f2" {
		t.Fatalf("forward deploy must keep the current forms, got %v", got)
	}
}
