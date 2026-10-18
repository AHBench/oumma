package ai

import (
	"encoding/json"
	"fmt"
	"strings"

	"oumma/internal/domain"
)

// extractInput attempts deterministic field extraction from global context and
// previous task output, guided by the task's JSON Schema.
//
// It returns (result, true) when every required field declared in the schema
// can be resolved from the available data sources. The caller should fall back
// to an LLM call when (_, false) is returned.
func extractInput(req *PrepareInputRequest) (map[string]any, bool) {
	if len(req.Task.InputSchema) == 0 {
		// No schema — return global context as-is; nothing to validate.
		return req.GlobalCtx, true
	}

	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(req.Task.InputSchema, &schema); err != nil || len(schema.Properties) == 0 {
		return nil, false
	}

	// Build a merged lookup: prev output values take precedence over global
	// context because they represent the most recent task's direct output.
	merged := make(map[string]any, len(req.GlobalCtx))
	for k, v := range req.GlobalCtx {
		merged[k] = v
	}
	if prev, ok := req.PrevOutput.(map[string]any); ok {
		for k, v := range prev {
			merged[k] = v
		}
	}

	// Extract only schema-declared fields.
	result := make(map[string]any, len(schema.Properties))
	for k := range schema.Properties {
		if v, ok := merged[k]; ok {
			result[k] = v
		}
	}

	// Verify all required fields are present.
	for _, req := range schema.Required {
		if _, ok := result[req]; !ok {
			return nil, false
		}
	}

	return result, true
}

// langNames maps BCP-47 language codes to their human-readable names used in LLM instructions.
var langNames = map[string]string{
	"ar": "Arabic",
	"zh": "Chinese (Mandarin)",
	"de": "German",
	"en": "English",
	"es": "Spanish",
	"fr": "French",
	"it": "Italian",
	"ja": "Japanese",
	"nl": "Dutch",
	"pt": "Portuguese",
	"ru": "Russian",
	"tr": "Turkish",
}

// langInstruction returns a language directive block to append to LLM prompts.
func langInstruction(lang string) string {
	if lang == "" {
		return ""
	}
	norm := strings.ToLower(strings.TrimSpace(lang))
	if norm == "en" || norm == "" {
		return ""
	}
	name, ok := langNames[norm]
	if !ok {
		name = norm
	}
	return fmt.Sprintf(`
LANGUAGE: Generate ALL human-readable text (reasoning, explanations, comments) in %s (%s). JSON field names, task IDs, and boolean/numeric values must remain in English as defined by the schema.`,
		name, strings.ToUpper(norm))
}

// inputSystemPrompt returns the system prompt for the input-preparation step,
// tuned to the model family.
func inputSystemPrompt(family string) string {
	switch family {
	case "qwen":
		return "You are a workflow input mapper. Output ONLY a valid JSON object. No markdown, no explanation, no thinking. Fill the schema strictly from the provided data."
	case "gemma":
		return "You are a workflow engine controller. Your sole output must be a valid JSON object matching the given schema. Do not include markdown or prose."
	default:
		return "You are a workflow engine controller that prepares task inputs. Always respond with valid JSON only."
	}
}

// routeSystemPrompt returns the system prompt for the routing step,
// tuned to the model family.
func routeSystemPrompt(family string) string {
	switch family {
	case "qwen":
		return `You are a strict workflow router. You MUST select next_task_id from the VALID NEXT TASKS list only. Output ONLY a JSON object: {"next_task_id":"...","reasoning":"...","override_applied":false}. No markdown, no thinking, no extra fields.`
	case "gemma":
		return "You are a workflow engine router. Navigate the workflow topology to determine the next task. Always respond with valid JSON containing next_task_id, reasoning, and override_applied."
	default:
		return "You are a workflow engine router. Navigate the workflow topology to determine the next task. Always respond with valid JSON containing next_task_id, reasoning, and override_applied."
	}
}

// BuildInputPrompt constructs the prompt for the LLM input-preparation step,
// using a template optimised for the given model family.
func BuildInputPrompt(req *PrepareInputRequest, modelFamily string) string {
	switch modelFamily {
	case "qwen":
		return buildInputPromptQwen(req)
	default:
		return buildInputPromptDefault(req)
	}
}

// BuildRoutingPrompt constructs the prompt for the LLM routing step,
// using a template optimised for the given model family.
func BuildRoutingPrompt(req *RouteRequest, modelFamily string) string {
	// Fork-join decisions get a dedicated structured prompt regardless of model
	// family: the standard prompts risk having global context compacted away and
	// the model confusing merged fields with branch-specific values.
	if req.BranchOutputs != nil {
		return buildForkJoinRoutingPrompt(req, modelFamily)
	}
	switch modelFamily {
	case "qwen":
		return buildRoutingPromptQwen(req)
	default:
		return buildRoutingPromptDefault(req)
	}
}

// --- Input prompt templates ---

// schemaKeys parses a JSON Schema object and returns the top-level property
// names declared in it. Returns nil when the schema cannot be parsed.
func schemaKeys(schema json.RawMessage) []string {
	if len(schema) == 0 {
		return nil
	}
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		return nil
	}
	keys := make([]string, 0, len(s.Properties))
	for k := range s.Properties {
		keys = append(keys, k)
	}
	return keys
}

// filterContext returns a new map containing only the keys declared in the
// schema properties. This drastically reduces token count for large contexts.
func filterContext(ctx map[string]any, keys []string) map[string]any {
	if len(keys) == 0 {
		return ctx
	}
	out := make(map[string]any, len(keys))
	for _, k := range keys {
		if v, ok := ctx[k]; ok {
			out[k] = v
		}
	}
	return out
}

// buildInputPromptQwen is a concise, constraint-first template for Qwen3.
// Only schema-declared keys are extracted from context to minimise token count
// and reduce inference time on large models.
func buildInputPromptQwen(req *PrepareInputRequest) string {
	task := req.Task

	// Extract only the fields the schema requires from global context.
	keys := schemaKeys(task.InputSchema)
	relevant := filterContext(req.GlobalCtx, keys)
	ctxJSON, _ := json.Marshal(relevant)

	return fmt.Sprintf(`TASK:%s SCHEMA:%s
DATA:%s%s
Output a JSON object with the keys from SCHEMA, filled from DATA. No extra keys.`,
		task.ID,
		string(task.InputSchema),
		string(ctxJSON),
		langInstruction(req.Lang),
	)
}

// buildInputPromptDefault is the standard verbose template used for Gemma and
// other OpenAI-compatible models.
func buildInputPromptDefault(req *PrepareInputRequest) string {
	task := req.Task
	ctxJSON, _ := json.MarshalIndent(req.GlobalCtx, "", "  ")
	prevJSON, _ := json.MarshalIndent(req.PrevOutput, "", "  ")

	var historyBlock string
	if len(req.TaskOutputs) > 0 {
		historyJSON, _ := json.MarshalIndent(req.TaskOutputs, "", "  ")
		historyBlock = fmt.Sprintf("\nEXECUTION HISTORY (completed tasks and their outputs):\n%s\n", string(historyJSON))
	}

	return fmt.Sprintf(`You are a workflow engine controller. Prepare the input for the next task.

WORKFLOW: %s
Description: %s

Task: %s (ID: %s)
Description: %s

Expected Input Schema:
%s

Global Context:
%s

Previous Task Output:
%s
%s%s
Generate a JSON object that conforms to the Expected Input Schema, using the data available in the Global Context, Previous Task Output, and Execution History. Return ONLY a valid JSON object with no markdown formatting.`,
		req.DefinitionID, req.DefinitionDesc,
		task.Type, task.ID, task.Description,
		string(task.InputSchema),
		string(ctxJSON),
		string(prevJSON),
		historyBlock,
		langInstruction(req.Lang),
	)
}

// --- Routing prompt templates ---

// buildForkJoinRoutingPrompt builds a specialised, highly-structured routing
// prompt for fork-join decisions. It avoids using the (large, merged) global
// context as the primary signal and instead surfaces each branch's individual
// output explicitly. This prevents the model from (a) having the output
// compacted away and (b) confusing merged global-context field values with the
// specific per-branch approval values that the join conditions reference.
func buildForkJoinRoutingPrompt(req *RouteRequest, modelFamily string) string {
	// Serialise per-branch outputs with task_id labels.
	var branchBlock strings.Builder
	branchBlock.WriteString("BRANCH OUTPUTS (output of each completed parallel branch, keyed by task_id):\n")
	if len(req.BranchOutputs) == 0 {
		branchBlock.WriteString("  (no branch outputs available)\n")
	} else {
		for taskID, out := range req.BranchOutputs {
			b, _ := json.MarshalIndent(out, "    ", "  ")
			fmt.Fprintf(&branchBlock, "  task_id=%q:\n    %s\n", taskID, string(b))
		}
	}

	// Edge conditions.
	var edgeBlock strings.Builder
	for _, e := range req.Topology {
		cond := "always"
		if e.Condition != "" {
			cond = strings.TrimSpace(e.Condition)
		}
		fmt.Fprintf(&edgeBlock, "  -> %q  [condition: %s]\n", e.To, cond)
	}

	// Valid IDs.
	var validIDs []string
	for _, t := range req.ReachableTasks {
		validIDs = append(validIDs, fmt.Sprintf("%q", t.ID))
	}

	return fmt.Sprintf(`You are a workflow engine router deciding the next task after a parallel fork-join.

WORKFLOW: %s

FORK-JOIN COMPLETED:
  Branches: %s  (join mode: and)

%s
JOIN EDGES (candidate next tasks and their activation conditions):
%s
VALID NEXT TASK IDs (choose EXACTLY ONE): [%s]

INSTRUCTIONS:
1. Read BRANCH OUTPUTS above. Each entry shows the EXACT output of one parallel branch.
2. For each JOIN EDGE, evaluate its condition against the BRANCH OUTPUTS.
   - A condition like "manager_approval is Yes AND juriste_approval is Yes" means BOTH branches
     must have their respective approval field equal to "Yes" (or true / approved).
   - A condition like "manager_approval is No OR juriste_approval is No" means AT LEAST ONE
     branch returned a rejection ("No", false, or approved=false).
3. Select the task_id whose condition is satisfied by the actual branch outputs.
4. Your final JSON answer MUST match your evaluation — do NOT contradict your reasoning.
5. Never pick a task_id that is not in the VALID list.

Return ONLY this JSON (no markdown, no extra text):
{"next_task_id": "<chosen_task_id>", "reasoning": "<one sentence>", "override_applied": false}%s`,
		req.DefinitionID,
		req.Task.ID,
		branchBlock.String(),
		edgeBlock.String(),
		strings.Join(validIDs, ", "),
		langInstruction(req.Lang),
	)
}

// buildNextTaskLookahead builds a one-step lookahead block for each candidate
// next task: it shows where each reachable task leads in the full topology.
// This lets the LLM understand the downstream consequences of each routing
// choice and avoids picking the wrong branch (e.g. at a fork-join).
func buildNextTaskLookahead(reachableTasks []*domain.Task, allEdges []domain.Edge) string {
	if len(reachableTasks) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("NEXT TASK TOPOLOGY (one-step lookahead — what each candidate leads to):\n")
	for _, t := range reachableTasks {
		// Collect regular outgoing edges for this task.
		var outs []string
		for _, e := range allEdges {
			if e.From == t.ID {
				cond := "always"
				if e.Condition != "" {
					// Truncate long conditions to keep the prompt concise.
					c := strings.TrimSpace(e.Condition)
					if len(c) > 80 {
						c = c[:80] + "…"
					}
					cond = c
				}
				outs = append(outs, fmt.Sprintf("%s [%s]", e.To, cond))
			}
		}
		if len(outs) == 0 {
			fmt.Fprintf(&sb, "  %q (%s): no further steps (terminal)\n", t.ID, t.Type)
		} else {
			fmt.Fprintf(&sb, "  %q (%s): -> %s\n", t.ID, t.Type, strings.Join(outs, " | "))
		}
	}
	return sb.String()
}

// buildFullTopology renders all process edges as a compact text map.
// This gives the model a bird's-eye view of the complete workflow graph so
// it understands why certain routes are mandatory and cannot be skipped.
func buildFullTopology(edges []domain.Edge) string {
	if len(edges) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("FULL PROCESS MAP (all edges):\n")
	for _, e := range edges {
		cond := "always"
		if e.Condition != "" {
			cond = e.Condition
		}
		from := e.From
		if from == "" && len(e.JoinFrom) > 0 {
			from = "join[" + strings.Join(e.JoinFrom, ",") + "]"
		}
		if e.Fork {
			fmt.Fprintf(&sb, "  %s ==(fork)==> %s  [%s]\n", from, e.To, cond)
		} else {
			fmt.Fprintf(&sb, "  %s --> %s  [%s]\n", from, e.To, cond)
		}
	}
	return sb.String()
}

// buildRoutingPromptQwen is a strict, minimal routing template for Qwen3.
// Constraints are front-loaded; valid choices are enumerated explicitly.
func buildRoutingPromptQwen(req *RouteRequest) string {
	task := req.Task
	outputJSON, _ := json.Marshal(req.Output)
	ctxJSON, _ := json.Marshal(req.GlobalCtx)

	// Build a list of valid next task IDs — the only choices allowed.
	var validIDs []string
	for _, t := range req.ReachableTasks {
		validIDs = append(validIDs, fmt.Sprintf("%q", t.ID))
	}
	validList := strings.Join(validIDs, ", ")

	// Edge conditions for the current task.
	var edgeBlock strings.Builder
	for _, e := range req.Topology {
		cond := "always"
		if e.Condition != "" {
			cond = e.Condition
		}
		fmt.Fprintf(&edgeBlock, "  %s --> %s  [%s]\n", task.ID, e.To, cond)
	}

	fullTopo := buildFullTopology(req.AllEdges)

	var historyBlock string
	if len(req.TaskOutputs) > 0 {
		histJSON, _ := json.Marshal(req.TaskOutputs)
		historyBlock = fmt.Sprintf("\nHISTORY:%s\n", string(histJSON))
	}

	var overrideBlock string
	if req.UserOverride != "" {
		overrideBlock = fmt.Sprintf("\nUSER OVERRIDE: %q — you MAY choose any task from ALL TASKS if this demands a deviation.\n", req.UserOverride)
		var allIDs []string
		for id := range req.AllTasks {
			allIDs = append(allIDs, fmt.Sprintf("%q", id))
		}
		validList = strings.Join(allIDs, ", ")
	}

	lookahead := buildNextTaskLookahead(req.ReachableTasks, req.AllEdges)

	return fmt.Sprintf(`ROUTE DECISION for workflow %q

COMPLETED: task_id=%q  type=%s
OUTPUT: %s

EDGES FROM THIS TASK:
%s
%s
%s
VALID NEXT TASK IDs (choose EXACTLY ONE): [%s]

CONTEXT: %s%s%s%s

RULES:
1. next_task_id MUST be one of the VALID NEXT TASK IDs above.
2. Evaluate edge conditions against OUTPUT and CONTEXT values.
3. "always" condition means unconditional — pick it when there is only one edge.
4. Never invent a task ID. Never pick a task not in the valid list.
5. END tasks are valid destinations — do not avoid them.
6. Use NEXT TASK TOPOLOGY to confirm your choice leads to the correct downstream path.

Return JSON: {"next_task_id":"<id>","reasoning":"<brief>","override_applied":<bool>}`,
		req.DefinitionID,
		task.ID, task.Type,
		string(outputJSON),
		edgeBlock.String(),
		fullTopo,
		lookahead,
		validList,
		string(ctxJSON),
		historyBlock,
		overrideBlock,
		langInstruction(req.Lang),
	)
}

// buildRoutingPromptDefault is the standard routing template for Gemma and
// other models. It includes the full process topology for global context.
func buildRoutingPromptDefault(req *RouteRequest) string {
	task := req.Task
	outputJSON, _ := json.MarshalIndent(req.Output, "", "  ")
	ctxJSON, _ := json.MarshalIndent(req.GlobalCtx, "", "  ")

	var topoList strings.Builder
	for _, e := range req.Topology {
		cond := "always"
		if e.Condition != "" {
			cond = e.Condition
		}
		fromLabel := e.From
		if fromLabel == "" && len(e.JoinFrom) > 0 {
			fromLabel = "join[" + strings.Join(e.JoinFrom, ", ") + "]"
		}
		fmt.Fprintf(&topoList, "  %s -> %s  [condition: %s]\n", fromLabel, e.To, cond)
	}

	var reachableList strings.Builder
	for _, t := range req.ReachableTasks {
		fmt.Fprintf(&reachableList, "  - ID: %q  Type: %s  Description: %s\n", t.ID, t.Type, t.Description)
	}

	var allTaskList strings.Builder
	for _, t := range req.AllTasks {
		fmt.Fprintf(&allTaskList, "  - ID: %q  Type: %s  Description: %s\n", t.ID, t.Type, t.Description)
	}

	var historyBlock string
	if len(req.TaskOutputs) > 0 {
		historyJSON, _ := json.MarshalIndent(req.TaskOutputs, "", "  ")
		historyBlock = fmt.Sprintf("\nEXECUTION HISTORY (completed tasks and their outputs):\n%s\n", string(historyJSON))
	}

	var overrideBlock string
	if req.UserOverride != "" {
		overrideBlock = fmt.Sprintf(`
USER OVERRIDE (HIGH PRIORITY):
The user has provided an explicit contextual override: %q
If this override mandates a deviation from the normal topology (e.g., "Emergency Stop", "Jump to Approval", "Skip validation"), you MUST honor this intent and route to the appropriate task — even if it is not in the topology-reachable set. In that case, choose the best matching task from the All Tasks list.
`, req.UserOverride)
	}

	fullTopo := buildFullTopology(req.AllEdges)
	lookahead := buildNextTaskLookahead(req.ReachableTasks, req.AllEdges)

	return fmt.Sprintf(`You are a workflow engine router. Determine which task should execute next.

WORKFLOW: %s
Description: %s

COMPLETED TASK:
  ID: %s  Type: %s
  Description: %s

TASK OUTPUT:
%s

WORKFLOW TOPOLOGY (edges from current task):
%s
REACHABLE TASKS (topology-defined next steps):
%s
NEXT TASK TOPOLOGY (one-step lookahead — downstream path for each candidate):
%s
ALL TASKS (for override scenarios only):
%s
%s
GLOBAL CONTEXT:
%s
%s%s%s
ROUTING RULES:
1. BY DEFAULT, select the next_task_id from the REACHABLE TASKS based on the task output and edge conditions.
2. EVALUATE each edge condition against the concrete data values in GLOBAL CONTEXT and TASK OUTPUT. Conditions reference data like amounts, statuses, flags — check the actual values to determine which condition is satisfied.
3. When multiple edges exist, choose the one whose condition best matches the current data. Do NOT default to the first or simplest edge.
4. A workflow may have multiple END tasks (type: END), each representing a distinct terminal outcome (e.g., fraud confirmed vs. normal resolution). When an edge leads to an END task, select it according to the conditions — do NOT avoid or skip END tasks.
5. Use NEXT TASK TOPOLOGY to verify that the task you select leads to the correct downstream path given the execution context.
6. If a USER OVERRIDE is present and it explicitly demands a deviation, you MAY select any task from ALL TASKS.
7. Never invent a task ID that does not exist.
8. Consider the EXECUTION HISTORY to understand the full context of what has happened in this process instance.

Return ONLY a JSON object with this exact structure:
{"next_task_id": "<task_id>", "reasoning": "<brief explanation>", "override_applied": <true|false>}`,
		req.DefinitionID, req.DefinitionDesc,
		task.ID, task.Type, task.Description,
		string(outputJSON),
		topoList.String(),
		reachableList.String(),
		lookahead,
		allTaskList.String(),
		fullTopo,
		string(ctxJSON),
		historyBlock,
		overrideBlock,
		langInstruction(req.Lang),
	)
}
