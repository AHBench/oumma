package processconfig

import "testing"

func TestTaskRenamePair(t *testing.T) {
	old := []byte(`{"tasks":[{"task_id":"A"},{"task_id":"B"}]}`)
	// 1:1 swap → rename
	f, to, ok := taskRenamePair(old, []byte(`{"tasks":[{"task_id":"A"},{"task_id":"B2"}]}`))
	if !ok || f != "B" || to != "B2" {
		t.Fatalf("swap: got (%q,%q,%v)", f, to, ok)
	}
	// pure addition → not a rename
	if _, _, ok := taskRenamePair(old, []byte(`{"tasks":[{"task_id":"A"},{"task_id":"B"},{"task_id":"C"}]}`)); ok {
		t.Fatal("addition must not be a rename")
	}
	// pure removal → not a rename
	if _, _, ok := taskRenamePair(old, []byte(`{"tasks":[{"task_id":"A"}]}`)); ok {
		t.Fatal("removal must not be a rename")
	}
	// two removed, one added → ambiguous, not a rename
	if _, _, ok := taskRenamePair([]byte(`{"tasks":[{"task_id":"A"},{"task_id":"B"},{"task_id":"C"}]}`),
		[]byte(`{"tasks":[{"task_id":"A"},{"task_id":"D"}]}`)); ok {
		t.Fatal("ambiguous swap must not be a rename")
	}
}
