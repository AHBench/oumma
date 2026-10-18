package processconfig

import (
	"strings"
)

// ---------------------------------------------------------------------------
// YAML configuration — the "app:" section of workflows/{process_id}.yaml.
// Key names mirror the former brief_auth_db columns (see
// AUTH_BACKEND_PORT_PLAN.md §2).
// ---------------------------------------------------------------------------

// AppYAML is the subset of a workflow YAML file this package cares about.
type AppYAML struct {
	ProcessID string     `yaml:"process_id"`
	App       *AppConfig `yaml:"app,omitempty"`
}

// AppConfig is the ported auth-backend configuration for one process.
type AppConfig struct {
	Name               string           `yaml:"name"`
	ProcessDescription string           `yaml:"process_description"`
	Link               string           `yaml:"link"`
	IconPath         string           `yaml:"icon_path"`
	// IconKey selects one of the app's built-in process icons (rendered in
	// the process menu). Empty = the app's default icon.
	IconKey string `yaml:"icon_key,omitempty"`
	Company          string           `yaml:"company"`
	ServiceName      string           `yaml:"service_name"`
	PodName          string           `yaml:"pod_name"`
	CanBeLaunched    *bool            `yaml:"can_be_launched"`
	LaunchRoleID     string           `yaml:"launch_role_id"`
	SharedProcessIDs []string         `yaml:"shared_process_ids"`
	UserID           string           `yaml:"user_id"`
	Username         string           `yaml:"username"`
	Roles            []RoleConfig     `yaml:"roles"`
	Forms            FormsConfig      `yaml:"forms"`
	AI               *AIConfig        `yaml:"ai"`
	Templates        []TemplateConfig `yaml:"templates"`
	TaskGroups       *TaskGroupsConfig `yaml:"task_groups"`
	// SearchItems is the process-level label catalog: localized labels
	// (EN/FR/AR) for app menu items, form tab titles, process-data fields
	// and form field labels. The app resolves every UI label through this
	// catalog first, falling back to its own defaults.
	SearchItems []SearchItemConfig `yaml:"search_items"`
}

// Search item kinds — what a catalog entry localizes in the app. All three
// kinds localize LABELS ONLY: process-data fields are matched by name so
// their displayed label can be translated; the field's value/key is never
// touched.
const (
	SearchKindMenuItem   = "menu_item"   // an app menu entry (field_name = menu key)
	SearchKindTab        = "tab"         // a form tab title
	SearchKindFieldLabel = "field_label" // a form/process-data field label ($-name)
	SearchKindHint       = "hint"        // a status-bar tip for a hovered action
)

// ValidSearchKinds lists the accepted field_type values for search items.
func ValidSearchKinds() []string {
	return []string{SearchKindMenuItem, SearchKindTab, SearchKindFieldLabel, SearchKindHint}
}

func isValidSearchKind(k string) bool {
	for _, v := range ValidSearchKinds() {
		if k == v {
			return true
		}
	}
	return false
}

// RoleConfig mirrors process_roles + its children.
type RoleConfig struct {
	RoleID               string                 `yaml:"role_id"`
	RoleName             string                 `yaml:"role_name"`
	CanLaunch            *bool                  `yaml:"can_launch"`
	CanEditInitialForm   bool                   `yaml:"can_edit_initial_form"`
	ApplyDeptFilter      bool                   `yaml:"apply_dept_filter"`
	DocRoles             []DocRoleConfig        `yaml:"doc_roles"`
	ViewAccessRoles      []string               `yaml:"view_access_roles"`
	SearchItems          []SearchItemConfig     `yaml:"search_items"`
	AuthorisedSearch     []AuthorisedSearchItem `yaml:"authorised_search_in_roles"`
	RoleTasks            []RoleTaskConfig       `yaml:"role_tasks"`
}

// DocRoleConfig mirrors doc_roles. Permissions lists the allowed
// permission ids (all granted=true, matching the Java behaviour).
type DocRoleConfig struct {
	RoleID      string   `yaml:"role_id"`
	RoleName    string   `yaml:"role_name"`
	Permissions []string `yaml:"permissions"`
}

// SearchItemConfig mirrors search_items.
type SearchItemConfig struct {
	FieldName    string `yaml:"field_name"`
	FieldType    string `yaml:"field_type"`
	FieldLabelEN string `yaml:"field_label_en"`
	FieldLabelFR string `yaml:"field_label_fr"`
	FieldLabelAR string `yaml:"field_label_ar"`
}

// AuthorisedSearchItem mirrors authorised_search_in_roles.
type AuthorisedSearchItem struct {
	RoleID          string `yaml:"role_id"`
	RoleName        string `yaml:"role_name"`
	RoleDescription string `yaml:"role_description"`
}

// RoleTaskConfig mirrors role_tasks → task_actions → action_items.
type RoleTaskConfig struct {
	TaskID               string            `yaml:"task_id,omitempty"` // legacy only — normalized to TaskName on load
	TaskName             string            `yaml:"task_name"`
	InitialTask          bool              `yaml:"initial_task"`
	CanEditInitialForm   bool              `yaml:"can_edit_initial_form"`
	RequiresDecisionForm *bool             `yaml:"requires_decision_form"`
	ActionGroups         []ActionGroupConf `yaml:"action_groups"`
}

type ActionGroupConf struct {
	ActionGroup string          `yaml:"action_group"`
	Actions     []ActionItemCfg `yaml:"actions"`
}

type ActionItemCfg struct {
	ActionName  string `yaml:"action_name"`
	ActionValue bool   `yaml:"action_value"`
}

// FormsConfig mirrors process_steps + form_fields.
type FormsConfig struct {
	Steps []StepConfig `yaml:"steps"`
}

type StepConfig struct {
	StepID     string `yaml:"step_id"`
	Launchable *bool  `yaml:"launchable"`
	// FormInput is arbitrary JSON/YAML (any native structure).
	FormInput any          `yaml:"form_input"`
	Fields    []FieldConfig `yaml:"fields"`
}

// FieldConfig mirrors form_fields (wire names kept verbatim, incl. the
// Java "multple_choice" typo).
type FieldConfig struct {
	FieldName            string   `yaml:"field_name"`
	FieldLabel           string   `yaml:"field_label"`
	FieldType            string   `yaml:"field_type"`
	FieldTab             string   `yaml:"field_tab"`
	FieldRow             *int     `yaml:"field_row"`
	FieldRequired        *bool    `yaml:"field_required"`
	FieldEdit            *bool    `yaml:"field_edit"`
	FieldHeaderDisplay   *bool    `yaml:"field_header_display"`
	FieldMinLength       *int     `yaml:"field_min_length"`
	FieldMaxLength       *int     `yaml:"field_max_length"`
	FieldPattern         string   `yaml:"field_pattern"`
	FieldOptions         []string `yaml:"field_options"`
	FieldSource          string   `yaml:"field_source"`
	FieldSourceType      string   `yaml:"field_source_type"`
	FieldDBQuery         string   `yaml:"field_db_query"`
	FieldDatabaseUser    string   `yaml:"field_database_user"`
	FieldDatabasePasswd  string   `yaml:"field_database_password"`
	APIKey               string   `yaml:"api_key"`
	FieldNameTable       string   `yaml:"field_table_name"`
	FieldTableFieldNames []string `yaml:"field_table_field_names"`
	UserCanAdd           *bool    `yaml:"user_can_add"`
	FieldMultpleChoice   *bool    `yaml:"field_multple_choice"`
	FieldUnique          *bool    `yaml:"field_unique"`
	// FieldTemplate is arbitrary JSON/YAML (any native structure).
	FieldTemplate any `yaml:"field_template"`
}

// AIConfig mirrors brief_ai_params + ai_prompts.
type AIConfig struct {
	Model          string            `yaml:"model"`
	EmbeddingModel string            `yaml:"embedding_model"`
	ProcessDesc    string            `yaml:"process_desc"`
	Prompts        []AIPromptConfig  `yaml:"prompts"`
}

type AIPromptConfig struct {
	Type    string      `yaml:"type"`
	Name    string      `yaml:"name"`
	Prompts []PromptCfg `yaml:"prompts"`
}

type PromptCfg struct {
	Role    string `yaml:"role"`
	Content string `yaml:"content"`
}

// TemplateConfig mirrors process_templates.
type TemplateConfig struct {
	TemplateID    string   `yaml:"template_id"`
	Name          string   `yaml:"name"`
	Type          string   `yaml:"template_type"`
	AllowedGroups []string `yaml:"allowed_groups"`
	AllowedSteps  []string `yaml:"allowed_steps"`
	Filters       any      `yaml:"filters"`
	// JSONTemplate is arbitrary JSON/YAML (any native structure).
	JSONTemplate any `yaml:"json_template"`
}

// TaskGroupsConfig mirrors task_groups + task_items (group_id = process id).
type TaskGroupsConfig struct {
	TaskItems []TaskItemConfig `yaml:"task_items"`
	TaskView  []TaskItemConfig `yaml:"task_view"`
}

type TaskItemConfig struct {
	Order           int    `yaml:"order"`
	ItemName        string `yaml:"item_name"`
	ItemDescription string `yaml:"item_description"`
	URLActive       bool   `yaml:"url_active"`
	Detailed        bool   `yaml:"detailed"`
	Atomic          *bool  `yaml:"atomic"`
	Format          string `yaml:"format"`
	FormatString    string `yaml:"format_string"`
	ItemColumnName  string `yaml:"item_column_name"`
}

// ---------------------------------------------------------------------------
// JSON wire DTOs — field names copied verbatim from the Java @JsonProperty
// annotations (see the parity baselines in /tmp/authparity).
// ---------------------------------------------------------------------------

type ProcessDTO struct {
	ProcessID          string         `json:"process_id"`
	ProcessName        string         `json:"process_name"`
	ProcessDescription string         `json:"process_description"`
	UserID             string         `json:"user_id"`
	Username           string         `json:"username"`
	ProcessLink        string         `json:"process_link"`
	ProcessServiceName string         `json:"process_service_name"`
	ProcessPodName     string         `json:"process_pod_name"`
	ProcessIconPath    string         `json:"process_icon_path"`
	ProcessIconKey     string         `json:"process_icon_key"`
	Company            string         `json:"company"`
	ProcessLaunchRoleID string        `json:"process_launch_role_id"`
	ProcessCanBeLaunched *bool        `json:"process_can_be_launched"`
	Roles              []RoleDTO      `json:"roles"`
	SharedProcessIDs   []string       `json:"shared_process_ids"`
	InitialSteps       []string       `json:"initial_steps"`
	// SearchItems is the process-level label catalog (menu items, tabs,
	// field labels) carried on the DTO so the app can resolve localized
	// labels from the process list itself — no extra request that a
	// transient auth flake could break.
	SearchItems []SearchItemDTO `json:"search_items"`
}

type RoleDTO struct {
	ApplyDeptFilter    bool             `json:"apply_dept_filter"`
	RoleName           string           `json:"role_name"`
	RoleID             string           `json:"role_id"`
	RoleCanLaunch      *bool            `json:"role_can_launch"`
	CanEditInitialForm *bool            `json:"can_edit_initial_form"`
	DocRoles           []DocRoleDTO     `json:"doc_roles"`
	RoleTasks          []RoleTaskDTO    `json:"role_tasks"`
	ViewAccessRoles    []string         `json:"view_access_roles"`
}

type DocRoleDTO struct {
	RoleName       string             `json:"role_name"`
	RoleID         string             `json:"role_id"`
	DocPermissions []DocPermissionDTO `json:"doc_permissions"`
}

type DocPermissionDTO struct {
	ID         string `json:"id"`
	Permission bool   `json:"permission"`
}

type RoleTaskDTO struct {
	TaskName             string         `json:"task_name"`
	CanEditInitialForm   *bool          `json:"can_edit_initial_form"`
	RequiresDecisionForm *bool          `json:"requires_decision_form"`
	InitialTask          *bool          `json:"initial_task"`
	TaskActions          []TaskActionDTO `json:"task_actions"`
}

type TaskActionDTO struct {
	ActionGroup string          `json:"action_group"`
	ActionItems []ActionItemDTO `json:"action_items"`
}

type ActionItemDTO struct {
	ActionName  string `json:"action_name"`
	ActionValue *bool  `json:"action_value"`
}

type TaskGroupDTO struct {
	ID        string          `json:"id"`
	TaskItems []TaskItemDTO   `json:"task_items"`
	TaskView  []TaskItemDTO   `json:"task_view"`
}

type TaskItemDTO struct {
	Order           int    `json:"order"`
	ItemName        string `json:"item_name"`
	ItemDescription string `json:"item_description"`
	URLActive       bool   `json:"url_active"`
	Detailed        bool   `json:"detailed"`
	Atomic          bool   `json:"atomic"`
	Format          string `json:"format"`
	FormatString    string `json:"format_string"`
	ItemColumnName  string `json:"item_column_name"`
}

type ProcessFormDTO struct {
	ProcessID string         `json:"process_id"`
	Steps     []ProcessStepDTO `json:"steps"`
}

type ProcessStepDTO struct {
	StepID     string         `json:"step_id"`
	Launchable *bool          `json:"launchable"`
	FormFields []FormFieldDTO `json:"form_fields"`
	FormInput  any            `json:"form_input"`
	// FormID is the oper-data form identifier for this step (generated when
	// the step's form was saved/updated). Launches must carry it.
	FormID string `json:"form_id,omitempty"`
}

type FormFieldDTO struct {
	FieldRow             *int    `json:"field_row"`
	FieldTab             string  `json:"field_tab"`
	FieldType            string  `json:"field_type"`
	FieldLabel           string  `json:"field_label"`
	FieldName            string  `json:"field_name"`
	FieldRequired        *bool   `json:"field_required"`
	FieldMinLength       *int    `json:"field_minLength"`
	FieldMaxLength       *int    `json:"field_maxLength"`
	FieldPattern         *string `json:"field_pattern"`
	FieldOptions         []string `json:"field_options"`
	FieldHeaderDisplay   *bool   `json:"field_header_display"`
	FieldSource          *string `json:"field_source"`
	FieldSourceType      *string `json:"field_source_type"`
	FieldDBQuery         *string `json:"field_db_query"`
	FieldDatabaseUser    *string `json:"field_database_user"`
	FieldDatabasePasswd  *string `json:"field_database_password"`
	APIKey               *string `json:"api_key"`
	FieldNameTable       *string `json:"field_table_name"`
	FieldTableFieldNames []string `json:"field_table_field_names"`
	UserCanAdd           *bool   `json:"user_can_add"`
	FieldMultpleChoice   *bool   `json:"field_multple_choice"`
	FieldEdit            *bool   `json:"field_edit"`
	FieldUnique          *bool   `json:"field_unique"`
	FieldTemplate        any     `json:"field_template"`
}

type BriefAIParamsDTO struct {
	ProcessID      string         `json:"process_id"`
	ProcessDesc    string         `json:"process_desc"`
	Prompts        []AIPromptDTO  `json:"prompts"`
	Model          string         `json:"model"`
	EmbeddingModel string         `json:"embedding_model"`
}

type AIPromptDTO struct {
	Type    string      `json:"type"`
	Name    string      `json:"name"`
	Prompts []PromptDTO `json:"prompts"`
}

type PromptDTO struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// SearchItemDTO — Java has no @JsonProperty here, so camelCase keys.
type SearchItemDTO struct {
	FieldName    string `json:"fieldName"`
	FieldType    string `json:"fieldType"`
	FieldLabelFR string `json:"fieldLabel_fr"`
	FieldLabelAR string `json:"fieldLabel_ar"`
	FieldLabelEN string `json:"fieldLabel_en"`
}

// AuthorisedSearchInRoleDTO — camelCase keys (no @JsonProperty in Java).
type AuthorisedSearchInRoleDTO struct {
	RoleID          string `json:"roleId"`
	RoleDescription string `json:"roleDescription"`
	RoleName        string `json:"roleName"`
}

type TemplateItemDTO struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	JSONTemplate  any      `json:"jsonTemplate"`
	ProcessID     string   `json:"process_id"`
	TemplateType  string   `json:"template_type"`
	Filters       any      `json:"filters"`
	AllowedGroups []string `json:"allowed_groups"`
	AllowedSteps  []AllowedStepDTO `json:"allowed_steps"`
}

type AllowedStepDTO struct {
	StepID string `json:"stepId"`
}

// ---------------------------------------------------------------------------
// config → DTO conversion
// ---------------------------------------------------------------------------

var allDocPermissions = []string{"read", "write", "share", "delete", "view"}

func boolPtr(b bool) *bool            { return &b }
func strPtr(s string) *string         { if s == "" { return nil }; return &s }
func strPtrNonEmpty(s string) *string { return strPtr(s) }

func docPermissions(permIDs []string) []DocPermissionDTO {
	ids := permIDs
	if len(ids) == 0 {
		ids = allDocPermissions
	}
	out := make([]DocPermissionDTO, 0, len(ids))
	for _, id := range ids {
		out = append(out, DocPermissionDTO{ID: id, Permission: true})
	}
	return out
}

func searchItemsDTOs(items []SearchItemConfig) []SearchItemDTO {
	out := make([]SearchItemDTO, 0, len(items))
	for _, si := range items {
		out = append(out, SearchItemDTO{
			FieldName:    si.FieldName,
			FieldType:    si.FieldType,
			FieldLabelEN: si.FieldLabelEN,
			FieldLabelFR: si.FieldLabelFR,
			FieldLabelAR: si.FieldLabelAR,
		})
	}
	return out
}

// toDTO builds the process DTO the way the Java convertToDTO does.
func (p *AppProcess) toDTO() *ProcessDTO {
	a := p.App
	desc := a.ProcessDescription
	if desc == "" {
		desc = p.Description
	}
	shared := a.SharedProcessIDs
	if shared == nil {
		shared = []string{}
	}
	initialSteps := []string{}
	for _, st := range a.Forms.Steps {
		if st.Launchable != nil && *st.Launchable {
			initialSteps = append(initialSteps, st.StepID)
		}
	}
	d := &ProcessDTO{
		ProcessID:            p.ID,
		ProcessName:          a.Name,
		ProcessDescription:   desc,
		UserID:               a.UserID,
		Username:             a.Username,
		ProcessLink:          a.Link,
		ProcessServiceName:   a.ServiceName,
		ProcessPodName:       a.PodName,
		ProcessIconPath:      a.IconPath,
		ProcessIconKey:     a.IconKey,
		Company:              a.Company,
		InitialSteps:         initialSteps,
		ProcessLaunchRoleID:  a.LaunchRoleID,
		ProcessCanBeLaunched: a.CanBeLaunched,
		SharedProcessIDs:     shared,
		SearchItems:          searchItemsDTOs(a.SearchItems),
	}
	if a.CanBeLaunched == nil {
		d.ProcessCanBeLaunched = boolPtr(true)
	}
	for _, rc := range a.Roles {
		d.Roles = append(d.Roles, roleToDTO(rc))
	}
	return d
}

func roleToDTO(rc RoleConfig) RoleDTO {
	d := RoleDTO{
		ApplyDeptFilter: rc.ApplyDeptFilter,
		RoleName:        rc.RoleName,
		RoleID:          rc.RoleID,
		RoleCanLaunch:   rc.CanLaunch,
		ViewAccessRoles: rc.ViewAccessRoles,
	}
	if d.ViewAccessRoles == nil {
		d.ViewAccessRoles = []string{}
	}
	if rc.CanLaunch == nil {
		d.RoleCanLaunch = nil
	}
	d.CanEditInitialForm = boolPtr(rc.CanEditInitialForm)
	for _, dr := range rc.DocRoles {
		d.DocRoles = append(d.DocRoles, DocRoleDTO{
			RoleName:       dr.RoleName,
			RoleID:         dr.RoleID,
			DocPermissions: docPermissions(dr.Permissions),
		})
	}
	for _, t := range rc.RoleTasks {
		td := RoleTaskDTO{
			TaskName:             t.TaskName,
			CanEditInitialForm:   boolPtr(t.CanEditInitialForm),
			RequiresDecisionForm: t.RequiresDecisionForm,
			InitialTask:          boolPtr(t.InitialTask),
		}
		if t.RequiresDecisionForm == nil {
			td.RequiresDecisionForm = boolPtr(true)
		}
		for _, ag := range t.ActionGroups {
			tad := TaskActionDTO{ActionGroup: ag.ActionGroup}
			for _, ai := range ag.Actions {
				tad.ActionItems = append(tad.ActionItems, ActionItemDTO{ActionName: ai.ActionName, ActionValue: boolPtr(ai.ActionValue)})
			}
			td.TaskActions = append(td.TaskActions, tad)
		}
		d.RoleTasks = append(d.RoleTasks, td)
	}
	return d
}

// allRolesAsDocRoles builds the "doc_roles = every role of the process, all
// permissions granted" list the Java controller injects into every returned
// role (admin and filtered alike).
func (p *AppProcess) allRolesAsDocRoles() []DocRoleDTO {
	out := make([]DocRoleDTO, 0, len(p.App.Roles))
	for _, rc := range p.App.Roles {
		out = append(out, DocRoleDTO{
			RoleName:       rc.RoleName,
			RoleID:         rc.RoleID,
			DocPermissions: docPermissions(nil),
		})
	}
	return out
}

func (p *AppProcess) toFormDTO() *ProcessFormDTO {
	d := &ProcessFormDTO{ProcessID: p.ID, Steps: []ProcessStepDTO{}}
	for _, s := range p.App.Forms.Steps {
		sd := ProcessStepDTO{
			StepID:     s.StepID,
			Launchable: s.Launchable,
			FormInput:  s.FormInput,
			FormFields: []FormFieldDTO{},
		}
		if s.Launchable == nil {
			sd.Launchable = boolPtr(true)
		}
		for _, f := range s.Fields {
			sd.FormFields = append(sd.FormFields, fieldToDTO(f))
		}
		d.Steps = append(d.Steps, sd)
	}
	return d
}

func (p *AppProcess) stepDTO(stepID string) *ProcessStepDTO {
	for _, s := range p.App.Forms.Steps {
		if s.StepID != stepID {
			continue
		}
		sd := &ProcessStepDTO{StepID: s.StepID, Launchable: s.Launchable, FormInput: s.FormInput, FormFields: []FormFieldDTO{}}
		if s.Launchable == nil {
			sd.Launchable = boolPtr(true)
		}
		for _, f := range s.Fields {
			sd.FormFields = append(sd.FormFields, fieldToDTO(f))
		}
		return sd
	}
	return nil
}

func fieldToDTO(f FieldConfig) FormFieldDTO {
	opts := f.FieldOptions
	if opts == nil {
		opts = []string{}
	}
	tfn := f.FieldTableFieldNames
	if tfn == nil {
		tfn = []string{}
	}
	d := FormFieldDTO{
		FieldRow:             f.FieldRow,
		FieldTab:             f.FieldTab,
		FieldType:            f.FieldType,
		FieldLabel:           f.FieldLabel,
		FieldName:            f.FieldName,
		FieldRequired:        f.FieldRequired,
		FieldMinLength:       f.FieldMinLength,
		FieldMaxLength:       f.FieldMaxLength,
		FieldHeaderDisplay:   f.FieldHeaderDisplay,
		FieldOptions:         opts,
		FieldTableFieldNames: tfn,
		FieldEdit:            f.FieldEdit,
		UserCanAdd:           f.UserCanAdd,
		FieldMultpleChoice:   f.FieldMultpleChoice,
		FieldUnique:          f.FieldUnique,
	}
	if f.FieldRequired == nil {
		d.FieldRequired = boolPtr(true)
	}
	if f.FieldEdit == nil {
		d.FieldEdit = boolPtr(true)
	}
	if f.UserCanAdd == nil {
		d.UserCanAdd = boolPtr(false)
	}
	if f.FieldMultpleChoice == nil {
		d.FieldMultpleChoice = boolPtr(false)
	}
	if f.FieldUnique == nil {
		d.FieldUnique = boolPtr(false)
	}
	d.FieldPattern = strPtrNonEmpty(f.FieldPattern)
	d.FieldSource = strPtrNonEmpty(f.FieldSource)
	d.FieldSourceType = strPtrNonEmpty(f.FieldSourceType)
	d.FieldDBQuery = strPtrNonEmpty(f.FieldDBQuery)
	d.FieldDatabaseUser = strPtrNonEmpty(f.FieldDatabaseUser)
	d.FieldDatabasePasswd = strPtrNonEmpty(f.FieldDatabasePasswd)
	d.APIKey = strPtrNonEmpty(f.APIKey)
	d.FieldNameTable = strPtrNonEmpty(f.FieldNameTable)
	d.FieldTemplate = f.FieldTemplate
	return d
}

func (p *AppProcess) toAIParamsDTO() *BriefAIParamsDTO {
	if p.App.AI == nil {
		return nil
	}
	a := p.App.AI
	d := &BriefAIParamsDTO{
		ProcessID:      p.ID,
		ProcessDesc:    a.ProcessDesc,
		Prompts:        []AIPromptDTO{},
		Model:          a.Model,
		EmbeddingModel: a.EmbeddingModel,
	}
	for _, pr := range a.Prompts {
		pd := AIPromptDTO{Type: pr.Type, Name: pr.Name, Prompts: []PromptDTO{}}
		for _, c := range pr.Prompts {
			pd.Prompts = append(pd.Prompts, PromptDTO{Role: c.Role, Content: c.Content})
		}
		d.Prompts = append(d.Prompts, pd)
	}
	return d
}

func (p *AppProcess) toTaskGroupDTO() *TaskGroupDTO {
	tg := p.App.TaskGroups
	if tg == nil {
		return nil
	}
	d := &TaskGroupDTO{ID: p.ID, TaskItems: []TaskItemDTO{}, TaskView: []TaskItemDTO{}}
	for _, it := range tg.TaskItems {
		d.TaskItems = append(d.TaskItems, taskItemToDTO(it))
	}
	for _, it := range tg.TaskView {
		d.TaskView = append(d.TaskView, taskItemToDTO(it))
	}
	return d
}

func taskItemToDTO(t TaskItemConfig) TaskItemDTO {
	atom := true
	if t.Atomic != nil {
		atom = *t.Atomic
	}
	return TaskItemDTO{
		Order:           t.Order,
		ItemName:        t.ItemName,
		ItemDescription: t.ItemDescription,
		URLActive:       t.URLActive,
		Detailed:        t.Detailed,
		Atomic:          atom,
		Format:          t.Format,
		FormatString:    t.FormatString,
		ItemColumnName:  t.ItemColumnName,
	}
}

func (p *AppProcess) toTemplateItems() []TemplateItemDTO {
	items := make([]TemplateItemDTO, 0, len(p.App.Templates))
	for _, t := range p.App.Templates {
		items = append(items, templateToItem(p.ID, t))
	}
	return items
}

func templateToItem(processID string, t TemplateConfig) TemplateItemDTO {
	d := TemplateItemDTO{
		ID:            t.TemplateID,
		Name:          t.Name,
		JSONTemplate:  t.JSONTemplate,
		ProcessID:     processID,
		TemplateType:  t.Type,
		Filters:       t.Filters,
		AllowedGroups: t.AllowedGroups,
		AllowedSteps:  []AllowedStepDTO{},
	}
	for _, s := range t.AllowedSteps {
		d.AllowedSteps = append(d.AllowedSteps, AllowedStepDTO{StepID: s})
	}
	return d
}

// roleByID returns the process role with the given role_id.
func (p *AppProcess) roleByID(roleID string) *RoleConfig {
	if p.App == nil {
		return nil
	}
	return p.App.roleByID(roleID)
}

func (a *AppConfig) roleByID(roleID string) *RoleConfig {
	for i := range a.Roles {
		if a.Roles[i].RoleID == roleID {
			return &a.Roles[i]
		}
	}
	return nil
}

// groupMatchesRole ports the Java matching rule: user group g matches role r
// when g == "/"+r or prefix(g,"_") == "/"+r. Oumma claims already strip the
// leading "/", so both slashed and plain groups are handled.
func groupMatchesRole(group, roleID string) bool {
	g := strings.TrimPrefix(group, "/")
	if g == roleID {
		return true
	}
	return strings.SplitN(g, "_", 2)[0] == roleID
}
