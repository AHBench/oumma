package engine

import (
	"testing"

	"oumma/internal/domain"
)

func TestDeterministicNext(t *testing.T) {
	def := &domain.ProcessDefinition{
		Tasks: map[string]*domain.Task{
			"A": {ID: "A"}, "B": {ID: "B"}, "C": {ID: "C"},
		},
		Topology: []domain.Edge{
			{From: "A", To: "B"},
		},
	}
	if got := deterministicNext(def, "A"); got != "B" {
		t.Fatalf("single successor: got %q", got)
	}
	// Multiple successors → LLM must choose.
	def.Topology = append(def.Topology, domain.Edge{From: "A", To: "C", Condition: "x"})
	if got := deterministicNext(def, "A"); got != "" {
		t.Fatalf("two successors should need LLM: got %q", got)
	}
	// Fork edges don't count as regular successors.
	fdef := &domain.ProcessDefinition{
		Tasks: map[string]*domain.Task{"A": {ID: "A"}, "B": {ID: "B"}, "C": {ID: "C"}},
		Topology: []domain.Edge{
			{From: "A", To: "B", Fork: true},
			{From: "A", To: "C", Fork: true},
		},
	}
	if got := deterministicNext(fdef, "A"); got != "" {
		t.Fatalf("fork origin should not be deterministic: got %q", got)
	}
}

func TestDeterministicJoinTarget(t *testing.T) {
	// Single unconditional join edge → deterministic.
	if got := deterministicJoinTarget([]domain.Edge{{JoinFrom: []string{"B", "C"}, To: "D"}}); got != "D" {
		t.Fatalf("single join: got %q", got)
	}
	// Conditioned join edge → LLM evaluates.
	if got := deterministicJoinTarget([]domain.Edge{{JoinFrom: []string{"B"}, To: "D", Condition: "score < 50"}}); got != "" {
		t.Fatalf("conditioned join should need LLM: got %q", got)
	}
	// Multiple join targets → LLM chooses.
	if got := deterministicJoinTarget([]domain.Edge{
		{JoinFrom: []string{"B"}, To: "D", Condition: "ok"},
		{JoinFrom: []string{"B"}, To: "E", Condition: "not ok"},
	}); got != "" {
		t.Fatalf("multi-target join should need LLM: got %q", got)
	}
}
