package engine

import (
	"reflect"
	"testing"

	"oumma/internal/domain"
)

func TestForkSetMatches(t *testing.T) {
	targets := []string{"A", "B"}
	cases := []struct {
		name      string
		persisted map[string]*domain.ParallelBranch
		want      bool
	}{
		{"exact match", map[string]*domain.ParallelBranch{"A": {}, "B": {}}, true},
		{"missing branch", map[string]*domain.ParallelBranch{"A": {}}, false},
		{"extra branch", map[string]*domain.ParallelBranch{"A": {}, "B": {}, "C": {}}, false},
		{"empty", map[string]*domain.ParallelBranch{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := forkSetMatches(tc.persisted, targets); got != tc.want {
				t.Fatalf("forkSetMatches() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestInstanceDepartments(t *testing.T) {
	cases := []struct {
		name string
		gc   map[string]any
		want []string
	}{
		{"nil context", nil, nil},
		{"map shape (app wire)", map[string]any{"department": map[string]any{"codes": []any{"10", "20"}}}, []string{"10", "20"}},
		{"map shape wins over plural", map[string]any{"department": map[string]any{"codes": []any{"10"}}, "departments": []any{"99"}}, []string{"10"}},
		{"flat department array", map[string]any{"department": []any{"10"}}, []string{"10"}},
		{"legacy plural", map[string]any{"departments": []any{"10"}}, []string{"10"}},
		{"empty map shape falls through to plural", map[string]any{"department": map[string]any{}, "departments": []any{"7"}}, []string{"7"}},
		{"absent", map[string]any{"other": 1}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &domain.ExecutionState{GlobalContext: tc.gc}
			got := InstanceDepartments(st)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("InstanceDepartments() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSliceContainsStr(t *testing.T) {
	if !sliceContainsStr([]string{"Managers", "Juriste"}, "Juriste") {
		t.Fatal("expected true")
	}
	if sliceContainsStr([]string{"Managers"}, "Juriste") {
		t.Fatal("expected false")
	}
}
