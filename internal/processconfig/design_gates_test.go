package processconfig

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCompileGateways(t *testing.T) {
	doc := `{
  "id": "p1",
  "start_task_id": "start",
  "end_task_ids": ["end"],
  "tasks": [
    {"task_id": "start", "type": "START"},
    {"task_id": "a", "type": "HUMAN"},
    {"task_id": "gw", "type": "GATEWAY_EXCLUSIVE"},
    {"task_id": "b", "type": "HUMAN"},
    {"task_id": "c", "type": "HUMAN"},
    {"task_id": "end", "type": "END"}
  ],
  "topology": [
    {"from": "start", "to": "a"},
    {"from": "a", "to": "gw"},
    {"from": "gw", "to": "b", "condition": "amount > 5000"},
    {"from": "gw", "to": "c"},
    {"from": "b", "to": "end"},
    {"from": "c", "to": "end"}
  ]
}`
	out, err := compileGateways([]byte(doc))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	top, _ := json.Marshal(got["topology"])
	if strings.Contains(string(top), "gw") {
		t.Fatalf("gateway leaked into topology: %s", top)
	}
	if !strings.Contains(string(top), `"condition":"amount \u003e 5000"`) || !strings.Contains(string(top), `"from":"a","to":"b"`) {
		t.Fatalf("expected a->b with condition: %s", top)
	}
	if !strings.Contains(string(top), `"from":"a","to":"c"`) {
		t.Fatalf("expected a->c: %s", top)
	}
	tasks, _ := json.Marshal(got["tasks"])
	if strings.Contains(string(tasks), "GATEWAY") {
		t.Fatalf("gateway leaked into tasks: %s", tasks)
	}
}

func TestCompileGatewaysJoin(t *testing.T) {
	doc := `{
  "id": "p2",
  "tasks": [
    {"task_id": "a", "type": "HUMAN"},
    {"task_id": "b", "type": "HUMAN"},
    {"task_id": "j", "type": "GATEWAY_JOIN_OR"},
    {"task_id": "c", "type": "HUMAN"}
  ],
  "topology": [
    {"from": "a", "to": "j"},
    {"from": "b", "to": "j"},
    {"from": "j", "to": "c"}
  ]
}`
	out, err := compileGateways([]byte(doc))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"join_from":["a","b"]`) || !strings.Contains(s, `"join":"or"`) || !strings.Contains(s, `"to":"c"`) {
		t.Fatalf("join edge wrong: %s", s)
	}
	if strings.Contains(s, `"j"`) {
		t.Fatalf("gateway leaked: %s", s)
	}
}

func TestCompileGatewaysUnconnectedFails(t *testing.T) {
	doc := `{
  "id": "p3",
  "tasks": [{"task_id": "a", "type": "HUMAN"}, {"task_id": "gw", "type": "GATEWAY_EXCLUSIVE"}],
  "topology": [{"from": "a", "to": "gw"}]
}`
	if _, err := compileGateways([]byte(doc)); err == nil || !strings.Contains(err.Error(), "output") {
		t.Fatalf("expected output error, got %v", err)
	}
}

func TestCompileGatewaysNone(t *testing.T) {
	doc := []byte(`{"id":"p4","topology":[{"from":"a","to":"b"}]}`)
	out, err := compileGateways(doc)
	if err != nil || string(out) != string(doc) {
		t.Fatalf("passthrough failed: %s %v", out, err)
	}
}
