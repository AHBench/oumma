package domain

import "sort"

// Edge represents a directed connection between two tasks in the workflow graph.
// Regular edges have From set. Fork edges additionally have Fork:true.
// Join edges (used after a parallel fork) have JoinFrom set instead of From.
type Edge struct {
	From      string   `json:"from,omitempty" yaml:"from,omitempty"`
	JoinFrom  []string `json:"join_from,omitempty" yaml:"join_from,omitempty"`
	To        string   `json:"to" yaml:"to"`
	Condition string   `json:"condition,omitempty" yaml:"condition,omitempty"`
	// Fork marks this edge as one branch of a parallel fork. When two or more
	// edges leaving the same task have Fork:true, the engine runs them all in
	// parallel instead of asking the LLM to pick one.
	Fork bool `json:"fork,omitempty" yaml:"fork,omitempty"`
	// Join specifies the join condition for the parallel branches that converge
	// onto this edge. Valid values: "and" (wait for all), "or" (first to finish).
	// Only meaningful on join edges (JoinFrom non-empty). Defaults to "and".
	Join string `json:"join,omitempty" yaml:"join,omitempty"`
}

// ProcessDefinition is the blueprint for a workflow.
// It declares the graph of tasks, the topology edges, and lifecycle boundaries.
type ProcessDefinition struct {
	ID          string           `json:"id" yaml:"process_id"`
	Description string           `json:"description" yaml:"description"`
	Tasks       map[string]*Task `json:"tasks" yaml:"-"`
	TaskList    []*Task          `json:"-" yaml:"tasks"`
	StartTaskID string           `json:"start_task_id" yaml:"-"`
	EndTaskIDs  []string         `json:"end_task_ids" yaml:"-"`
	Topology    []Edge           `json:"topology" yaml:"topology"`
}

// NeighborsOf returns the list of regular (non-fork, non-join) outgoing edges
// from a given task ID. Fork edges are excluded because they are handled
// separately by ForkEdgesFrom.
func (d *ProcessDefinition) NeighborsOf(taskID string) []Edge {
	var out []Edge
	for _, e := range d.Topology {
		if e.From == taskID && !e.Fork {
			out = append(out, e)
		}
	}
	return out
}

// ForkEdgesFrom returns edges that have Fork:true and originate from taskID.
// A result with two or more edges indicates the task triggers a parallel fork.
func (d *ProcessDefinition) ForkEdgesFrom(taskID string) []Edge {
	var out []Edge
	for _, e := range d.Topology {
		if e.From == taskID && e.Fork {
			out = append(out, e)
		}
	}
	return out
}

// JoinEdgesForTasks returns all join edges (those with JoinFrom set) whose
// JoinFrom set exactly matches the provided taskIDs. The join mode ("and" or
// "or") is read from the first matching edge; it defaults to "and".
func (d *ProcessDefinition) JoinEdgesForTasks(taskIDs []string) (edges []Edge, joinMode string) {
	needle := sortedCopy(taskIDs)
	joinMode = "and"
	for _, e := range d.Topology {
		if len(e.JoinFrom) == 0 {
			continue
		}
		if equalSets(sortedCopy(e.JoinFrom), needle) {
			edges = append(edges, e)
			if e.Join != "" && joinMode == "and" {
				joinMode = e.Join
			}
		}
	}
	return edges, joinMode
}

// ReachableFromJoin returns the tasks reachable via join edges whose JoinFrom
// exactly matches taskIDs.
func (d *ProcessDefinition) ReachableFromJoin(taskIDs []string) []*Task {
	joinEdges, _ := d.JoinEdgesForTasks(taskIDs)
	var tasks []*Task
	for _, e := range joinEdges {
		if t, ok := d.Tasks[e.To]; ok {
			tasks = append(tasks, t)
		}
	}
	return tasks
}

// RebuildTaskList reconstructs TaskList from the Tasks map.
// Call this after JSON unmarshaling, since TaskList is tagged json:"-" and
// is not persisted — only the Tasks map is.
func (d *ProcessDefinition) RebuildTaskList() {
	d.TaskList = make([]*Task, 0, len(d.Tasks))
	for _, t := range d.Tasks {
		d.TaskList = append(d.TaskList, t)
	}
}

// ReachableFrom returns the set of task IDs directly reachable from a given task
// via regular (non-fork) edges.
func (d *ProcessDefinition) ReachableFrom(taskID string) []*Task {
	var tasks []*Task
	for _, e := range d.NeighborsOf(taskID) {
		if t, ok := d.Tasks[e.To]; ok {
			tasks = append(tasks, t)
		}
	}
	return tasks
}

// sortedCopy returns a sorted copy of a string slice.
func sortedCopy(s []string) []string {
	cp := make([]string, len(s))
	copy(cp, s)
	sort.Strings(cp)
	return cp
}

// equalSets returns true when two sorted string slices are identical.
func equalSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
