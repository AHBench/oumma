package processconfig

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Automatic semver rules (major.minor.patch) for design deployments:
//
//	major  — the process flow itself changes: tasks added/removed/renamed,
//	         topology edges changed, or the start task changed.
//	minor  — structural app changes: roles or form steps added/removed/renamed,
//	         or search-item labels edited (a localized-content change that
//	         every user sees).
//	patch  — everything else: permission toggles, field edits, search items
//	         added/removed, form_input tweaks, doc permissions.
//
// The first deploy of a process is 1.0.0.
// computeDesignSemver returns the semver for a new design deploy: it diffs
// the previously deployed document against the new one and bumps the
// previous semver according to the rules above.
func computeDesignSemver(prevSemver string, current, next []byte) string {
	cur := designDocMap(current)
	nxt := designDocMap(next)
	if len(cur) == 0 {
		return "1.0.0"
	}
	if !designFlowChanged(cur, nxt) && !designStructureChanged(cur, nxt) && docsEqual(cur, nxt) {
		if prevSemver != "" {
			return prevSemver
		}
		return "1.0.0"
	}
	if designFlowChanged(cur, nxt) {
		return bumpMajor(prevSemver)
	}
	if designStructureChanged(cur, nxt) || designSearchLabelsChanged(cur, nxt) {
		return bumpMinor(prevSemver)
	}
	return bumpPatch(prevSemver)
}

// designSearchLabelsChanged reports label-only edits in the search-item
// catalog: the same items (name + kind) with different EN/FR/AR labels.
// Item additions/removals are a patch; editing existing labels is minor.
func designSearchLabelsChanged(cur, nxt map[string]any) bool {
	appCur, _ := cur["app"].(map[string]any)
	appNxt, _ := nxt["app"].(map[string]any)
	a := searchItemsMap(appCur)
	b := searchItemsMap(appNxt)
	if len(a) == 0 && len(b) == 0 {
		return false
	}
	if len(a) != len(b) {
		return false // items added/removed → not a label-only edit
	}
	for name, la := range a {
		lb, ok := b[name]
		if !ok || la != lb {
			return la != lb // same set, different labels
		}
	}
	return false
}

// searchItemsMap builds name -> "kind|en|fr|ar" for the app's catalog.
func searchItemsMap(app map[string]any) map[string]string {
	items, _ := app["search_items"].([]any)
	out := map[string]string{}
	for _, it := range items {
		m, _ := it.(map[string]any)
		name := strValAny(m["field_name"])
		if name == "" {
			continue
		}
		out[name] = strValAny(m["field_type"]) + "|" + strValAny(m["field_label_en"]) + "|" +
			strValAny(m["field_label_fr"]) + "|" + strValAny(m["field_label_ar"])
	}
	return out
}

func docsEqual(a, b map[string]any) bool {
	return jsonStrMap(a) == jsonStrMap(b)
}

func jsonStrMap(m map[string]any) string {
	b, _ := json.Marshal(m)
	return string(b)
}

func designDocMap(b []byte) map[string]any {
	m := map[string]any{}
	if len(b) > 0 {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// designFlowChanged reports engine-level differences (major bump).
func designFlowChanged(cur, nxt map[string]any) bool {
	if taskIDsChanged(cur, nxt) || jsonStr(cur["topology"]) != jsonStr(nxt["topology"]) ||
		strValAny(cur["start_task_id"]) != strValAny(nxt["start_task_id"]) {
		return true
	}
	return false
}

func taskIDsChanged(cur, nxt map[string]any) bool {
	return sortedTaskIDs(cur) != sortedTaskIDs(nxt)
}

func sortedTaskIDs(doc map[string]any) string {
	tasks, _ := doc["tasks"].([]any)
	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		m, _ := t.(map[string]any)
		id := strValAny(m["task_id"])
		if id == "" {
			id = strValAny(m["id"])
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return strings.Join(ids, "|")
}

// designStructureChanged reports role/step structural differences (minor bump).
func designStructureChanged(cur, nxt map[string]any) bool {
	appCur, _ := cur["app"].(map[string]any)
	appNxt, _ := nxt["app"].(map[string]any)
	return roleIDsSorted(appCur) != roleIDsSorted(appNxt) ||
		stepIDsSorted(appCur) != stepIDsSorted(appNxt)
}

func roleIDsSorted(app map[string]any) string {
	roles, _ := app["roles"].([]any)
	ids := make([]string, 0, len(roles))
	for _, r := range roles {
		m, _ := r.(map[string]any)
		ids = append(ids, strValAny(m["role_id"]))
	}
	sort.Strings(ids)
	return strings.Join(ids, "|")
}

func stepIDsSorted(app map[string]any) string {
	forms, _ := app["forms"].(map[string]any)
	steps, _ := forms["steps"].([]any)
	ids := make([]string, 0, len(steps))
	for _, st := range steps {
		m, _ := st.(map[string]any)
		ids = append(ids, strValAny(m["step_id"]))
	}
	sort.Strings(ids)
	return strings.Join(ids, "|")
}

func bumpMajor(v string) string {
	m, _, _ := parseSemver(v)
	return fmt.Sprintf("%d.0.0", m+1)
}
func bumpMinor(v string) string {
	ma, mi, _ := parseSemver(v)
	if ma == 0 {
		ma = 1
	}
	return fmt.Sprintf("%d.%d.0", ma, mi+1)
}
func bumpPatch(v string) string {
	ma, mi, p := parseSemver(v)
	if ma == 0 {
		ma, mi = 1, 0
	}
	return fmt.Sprintf("%d.%d.%d", ma, mi, p+1)
}

func parseSemver(v string) (int, int, int) {
	v = strings.TrimSpace(v)
	parts := strings.SplitN(v, ".", 3)
	get := func(i int) int {
		if i >= len(parts) {
			return 0
		}
		n := 0
		for _, c := range parts[i] {
			if c < '0' || c > '9' {
				return 0
			}
			n = n*10 + int(c-'0')
		}
		return n
	}
	if len(parts) == 0 || parts[0] == "" {
		return 0, 0, 0
	}
	return get(0), get(1), get(2)
}

func jsonStr(v any) string {
	if v == nil {
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func strValAny(v any) string {
	s, _ := v.(string)
	return s
}
