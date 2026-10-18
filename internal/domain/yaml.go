package domain

import (
	"encoding/json"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// yamlTask mirrors Task but uses string for schema fields so YAML block
// scalars (which are strings) unmarshal correctly before conversion to
// json.RawMessage.
type yamlTask struct {
	ID           string         `yaml:"task_id"`
	Type         TaskType       `yaml:"type"`
	Description  string         `yaml:"description"`
	Group        string         `yaml:"group,omitempty"`
	InputSchema  string         `yaml:"input_schema,omitempty"`
	OutputSchema string         `yaml:"output_schema,omitempty"`
	Config       *TaskConfig    `yaml:"config,omitempty"`
	Metadata     map[string]any `yaml:"metadata,omitempty"`
}

// yamlDefinition mirrors the YAML file structure before normalization.
type yamlDefinition struct {
	ProcessID   string     `yaml:"process_id"`
	Description string     `yaml:"description"`
	Tasks       []yamlTask `yaml:"tasks"`
	Topology    []Edge     `yaml:"topology"`
}

// ParseYAMLFile reads a .yaml file and returns a validated ProcessDefinition.
func ParseYAMLFile(path string) (*ProcessDefinition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read yaml: %w", err)
	}
	return ParseYAML(data)
}

// ParseYAML parses raw YAML bytes into a validated ProcessDefinition.
func ParseYAML(data []byte) (*ProcessDefinition, error) {
	var raw yamlDefinition
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal yaml: %w", err)
	}

	if raw.ProcessID == "" {
		return nil, fmt.Errorf("process_id is required")
	}
	if len(raw.Tasks) == 0 {
		return nil, fmt.Errorf("at least one task is required")
	}

	def := &ProcessDefinition{
		ID:          raw.ProcessID,
		Description: raw.Description,
		Tasks:       make(map[string]*Task, len(raw.Tasks)),
		TaskList:    make([]*Task, 0, len(raw.Tasks)),
		Topology:    raw.Topology,
	}

	for i := range raw.Tasks {
		rt := &raw.Tasks[i]
		t := &Task{
			ID:          rt.ID,
			Type:        rt.Type,
			Description: rt.Description,
			Group:       rt.Group,
			Config:      rt.Config,
			Metadata:    rt.Metadata,
		}
		if rt.InputSchema != "" {
			if !json.Valid([]byte(rt.InputSchema)) {
				return nil, fmt.Errorf("task %q: input_schema is not valid JSON", rt.ID)
			}
			t.InputSchema = json.RawMessage(rt.InputSchema)
		}
		if rt.OutputSchema != "" {
			if !json.Valid([]byte(rt.OutputSchema)) {
				return nil, fmt.Errorf("task %q: output_schema is not valid JSON", rt.ID)
			}
			t.OutputSchema = json.RawMessage(rt.OutputSchema)
		}

		if t.ID == "" {
			return nil, fmt.Errorf("task at index %d: task_id is required", i)
		}
		if _, dup := def.Tasks[t.ID]; dup {
			return nil, fmt.Errorf("duplicate task_id %q", t.ID)
		}
		def.Tasks[t.ID] = t
		def.TaskList = append(def.TaskList, t)

		switch t.Type {
		case TaskTypeStart:
			if def.StartTaskID != "" {
				return nil, fmt.Errorf("multiple START tasks: %q and %q", def.StartTaskID, t.ID)
			}
			def.StartTaskID = t.ID
		case TaskTypeEnd:
			def.EndTaskIDs = append(def.EndTaskIDs, t.ID)
		}
	}

	if def.StartTaskID == "" {
		return nil, fmt.Errorf("no START task defined")
	}
	if len(def.EndTaskIDs) == 0 {
		return nil, fmt.Errorf("no END task defined")
	}

	// Validate topology references.
	for i, edge := range def.Topology {
		if len(edge.JoinFrom) > 0 {
			// Join edge: validate all join_from task IDs; From is intentionally empty.
			for _, jf := range edge.JoinFrom {
				if _, ok := def.Tasks[jf]; !ok {
					return nil, fmt.Errorf("topology[%d]: unknown join_from task %q", i, jf)
				}
			}
		} else {
			// Regular or fork edge: From must reference a known task.
			if _, ok := def.Tasks[edge.From]; !ok {
				return nil, fmt.Errorf("topology[%d]: unknown 'from' task %q", i, edge.From)
			}
		}
		if _, ok := def.Tasks[edge.To]; !ok {
			return nil, fmt.Errorf("topology[%d]: unknown 'to' task %q", i, edge.To)
		}
	}

	return def, nil
}
