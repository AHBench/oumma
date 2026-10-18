package processconfig

import "testing"

func TestSemverRules(t *testing.T) {
	base := `{"process_id":"p","start_task_id":"A","tasks":[{"task_id":"A"},{"task_id":"B"}],"topology":[{"from":"A","to":"B"}],"app":{"roles":[{"role_id":"R1"}],"forms":{"steps":[{"step_id":"S1"}]}}}`

	// first deploy
	if got := computeDesignSemver("", nil, []byte(base)); got != "1.0.0" {
		t.Fatalf("first deploy: got %s", got)
	}
	// no change → keep current
	if got := computeDesignSemver("1.2.3", []byte(base), []byte(base)); got != "1.2.3" {
		t.Fatalf("no change: got %s", got)
	}
	// patch: permission toggle
	np := `{"process_id":"p","start_task_id":"A","tasks":[{"task_id":"A"},{"task_id":"B"}],"topology":[{"from":"A","to":"B"}],"app":{"roles":[{"role_id":"R1","perms":{"x":true}}],"forms":{"steps":[{"step_id":"S1"}]}}}`
	if got := computeDesignSemver("1.2.3", []byte(base), []byte(np)); got != "1.2.4" {
		t.Fatalf("patch: got %s", got)
	}
	// minor: new role
	nm := `{"process_id":"p","start_task_id":"A","tasks":[{"task_id":"A"},{"task_id":"B"}],"topology":[{"from":"A","to":"B"}],"app":{"roles":[{"role_id":"R1"},{"role_id":"R2"}],"forms":{"steps":[{"step_id":"S1"}]}}}`
	if got := computeDesignSemver("1.2.3", []byte(base), []byte(nm)); got != "1.3.0" {
		t.Fatalf("minor role: got %s", got)
	}
	// minor: step rename
	ns := `{"process_id":"p","start_task_id":"A","tasks":[{"task_id":"A"},{"task_id":"B"}],"topology":[{"from":"A","to":"B"}],"app":{"roles":[{"role_id":"R1"}],"forms":{"steps":[{"step_id":"S1v2"}]}}}`
	if got := computeDesignSemver("1.2.3", []byte(base), []byte(ns)); got != "1.3.0" {
		t.Fatalf("minor step: got %s", got)
	}
	// major: new task
	nj := `{"process_id":"p","start_task_id":"A","tasks":[{"task_id":"A"},{"task_id":"B"},{"task_id":"C"}],"topology":[{"from":"A","to":"B"},{"from":"B","to":"C"}],"app":{"roles":[{"role_id":"R1"}],"forms":{"steps":[{"step_id":"S1"}]}}}`
	if got := computeDesignSemver("1.2.3", []byte(base), []byte(nj)); got != "2.0.0" {
		t.Fatalf("major: got %s", got)
	}
}

func TestSearchItemLabelEditIsMinor(t *testing.T) {
	base := []byte(`{"app":{"search_items":[
		{"field_name":"client","field_type":"field_label","field_label_en":"Client","field_label_fr":"Client","field_label_ar":"العميل"}
	]}}`)
	// label-only edit -> minor bump
	edited := []byte(`{"app":{"search_items":[
		{"field_name":"client","field_type":"field_label","field_label_en":"Client name","field_label_fr":"Client","field_label_ar":"العميل"}
	]}}`)
	if got := computeDesignSemver("1.2.3", base, edited); got != "1.3.0" {
		t.Errorf("label edit: got %s, want 1.3.0", got)
	}
	// identical doc -> keeps semver
	if got := computeDesignSemver("1.2.3", base, base); got != "1.2.3" {
		t.Errorf("unchanged doc: got %s, want 1.2.3", got)
	}
	// item added (set changes) -> patch, not minor
	added := []byte(`{"app":{"search_items":[
		{"field_name":"client","field_type":"field_label","field_label_en":"Client","field_label_fr":"Client","field_label_ar":"العميل"},
		{"field_name":"fraude","field_type":"field_label","field_label_en":"Fraud","field_label_fr":"Fraude","field_label_ar":"احتيال"}
	]}}`)
	if got := computeDesignSemver("1.2.3", base, added); got != "1.2.4" {
		t.Errorf("item added: got %s, want 1.2.4", got)
	}
	// item removed -> patch
	if got := computeDesignSemver("1.2.3", added, base); got != "1.2.4" {
		t.Errorf("item removed: got %s, want 1.2.4", got)
	}
}
