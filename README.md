# Oumma — Agentic Workflow Engine & Platform Backend

![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)

**Oumma** is the single Go backend that powers the Oumma platform: an
**LLM-orchestrated workflow engine** plus the platform's data, identity and
design services — all in one binary on one port (`:8095`), all persisted in
**one PostgreSQL database**.

- **Workflow engine** — process instances run as goroutines over a task graph
  (YAML-defined); an LLM controller decides contextual transitions, while
  single-successor hops and fork/join merges route **deterministically with
  zero LLM tokens**.
- **Agents** — a per-process **AI agent** (`/agent/chat`, `/agent/document`)
  that answers questions and performs app actions on the user's behalf, and a
  **designer agent** (`/api/management/design/ask`) that builds and modifies
  process designs through a safe, confirmation-gated tool catalog. The two
  modes can run on **different LLM endpoints** (see
  [`docker-compose.yaml`](docker-compose.yaml) / `.env.example`).
- **In-process data services** — auth (Keycloak proxy, self-registration with
  email activation), process/role management, document service (`/dms-api`)
  and operational form index (`/oper-api`). No sidecar containers.
- **MCP-like tool steps** — Gmail, Telegram, WhatsApp and generic HTTP
  webhooks as system tasks: outbound calls resolved from the process
  context, executed deterministically (dry-run by default).
- **PostgreSQL-first** — every runtime and governance fact lives in Postgres:
  execution states, checkpoints, design versions, LLM usage ledger, email
  activations, notifications history. Restart-safe, auditable, queryable.

The web frontend lives in the [`oumma-app`](../oumma-app) repository.

---

## Table of Contents

- [Agent Capabilities](#agent-capabilities)
- [PostgreSQL: The Single Source of Truth](#postgresql-the-single-source-of-truth)
- [Architecture Overview](#architecture-overview)
- [Project Structure](#project-structure)
- [Authentication & Authorization](#authentication--authorization)
- [In-Process Data Services](#in-process-data-services)
- [AI Assistant (ask) & LLM Tools](#ai-assistant-ask--llm-tools)
- [Data Model & Database Structure](#data-model--database-structure)
- [Workflow Definition (YAML)](#workflow-definition-yaml)
- [API Reference](#api-reference)
- [Direct Task Override](#direct-task-override)
- [Language Support](#language-support)
- [Engine Execution Flow](#engine-execution-flow)
- [Configuration](#configuration)
- [Using Local AI Models](#using-local-ai-models)
- [Running Locally](#running-locally)
- [Docker](#docker)
- [Build Script (buildx)](#build-script-buildx)

---

## Agent Capabilities

Oumma exposes two LLM-powered agents, each with a tool catalog and its own
optional endpoint/model (the designer agent can run on a different LLM than
the process agent — `OPENAI_*` vs `DESIGNER_OPENAI_*`).

### 1. The process agent (AI Assist)

`POST /agent/chat` and `POST /agent/document` back the **AI Assist**
sidebar in the app. The agent runs a tool loop over app actions, scoped by
the caller's token (roles/permissions are always enforced server-side):

- **Launch processes** with validated form data (form structure checked,
  required fields enforced, `form_id` mandatory).
- **Answer questions** about the user's dossiers, tasks and process data.
- **Document analysis** — uploaded documents (text + image parts) are
  summarized/extracted into the process context.
- **Draft and complete work** the user is allowed to do — every action hits
  the same permission checks as the UI (complete-task, comments,
  attachments are role-gated).

### 2. The designer agent

`POST /api/management/design/ask` backs the **design chat** of the Process
Designer. The agent edits a process through a described MCP-style tool
catalog — each call is proposed, **confirmed by the designer**, then applied
to the single *pending design version*:

- **Topology**: add/rename/delete steps (HUMAN/SYSTEM/START/END), connect,
  fork (parallel), join, conditions; the pending design is BPMN-validated
  before it can be deployed.
- **Roles & permissions**: add roles, per-step role assignments,
  permission sets, Keycloak group sync.
- **Forms**: create/update form steps (field types, required, 3-language
  labels) so chat JSON becomes a real form.
- **Labels & search items**: trilingual (EN/FR/AR) label catalog for every
  UI string; the app resolves all labels through it.
- **Tools**: add MCP-like tool steps (Gmail / Telegram / WhatsApp / HTTP)
  with config fields and context-mapped inputs.
- **Project lifecycle**: create a process (scaffolded START→END), rename the
  display name, change the process icon, rename the technical id
  (never-deployed only), delete a never-executed project, list/inspect
  versions.

**Deliberately not agent-driven:** deployment, discarding and quota claims
are human decisions only. Design asks consume the designer's monthly token
quota (`DESIGNER_TOKEN_QUOTA`); agent chat consumes the user's quota
(`NORMAL_USER_TOKEN_QUOTA`).

### 3. Deterministic (zero-token) behaviors

The LLM is only invoked where judgment is needed:

- single-successor hops and unconditional fork/join merges route without
  any LLM call;
- tool steps (MCP tools) resolve their inputs from the process context and
  fire plain HTTP calls — no tokens, repeatable, dry-run safe;
- the design chat is tool-driven: the LLM picks the tool, the code applies
  it.

---

## PostgreSQL: The Single Source of Truth

Every durable fact about the platform lives in **one PostgreSQL database**
(`DATABASE_URL`); the in-memory stores are only used in unit tests. This is
what makes the engine **restart-safe, auditable and reportable**:

| Table | What it persists |
|---|---|
| `execution_states` | Every process **instance**: current task, phase, global context (JSONB), task outputs, form data — restored verbatim on engine restart. |
| `process_definitions` | The deployed engine definitions (id + version + YAML). |
| `process_checkpoints` | Per-task execution checkpoints (phase, prepared input, overrides) so a crash mid-task resumes exactly where it stopped. |
| `process_designs` | The **design version history**: pending version + deployed versions with automatic semver (`major.minor.patch`) and the design document. |
| `llm_logs` | The **usage ledger**: every LLM call with username, department, definition/instance, model, prompt+completion tokens, cost. Feeds the per-user monthly quota and the cost rollup. |
| `usage_running` / `usage_monthly` / `usage_meta` | Monthly token/cost rollups (running month, closed months, rollup cursor). |
| `email_activations` | Single-use, 24-hour email activation tokens for self-registration. |
| `task_comments` / `task_attachments` / `task_histories` | Dossier collaboration: role-gated comments and attachments, plus the full task history (who did what, when, with which inputs/outputs). |
| `process_track_steps` | Track-step progress per instance (multi-step completion). |

Consequences for operations:

- **Restart safety** — kill the container mid-process; states, checkpoints
  and pending inputs are rehydrated from Postgres and instances continue.
- **Auditability** — `task_histories` + `llm_logs` answer *who did what,
  which version ran, and what the AI cost*.
- **Governance** — quotas, version pinning (running instances keep the
  version they started on) and design history are all row-level facts, not
  in-memory state.

---

## One Port, Many Links

The whole platform is exposed on **a single external port** (`8095` by
default) with path-based links — no sidecar ports to open:

| Path on `:8095` | Served by |
|---|---|
| `/api/auth/...` | auth, registration, email activation |
| `/api/management/...` | designer & management APIs |
| `/api/user-processes` | process read model |
| `/agent/...` | the process-mode AI agent |
| `/processes/...` | the engine (start, complete, tasks, documents, audit) |
| `/dms-api/...` | document service |
| `/oper-api/...` | operational form index |
| `/mailpit/...` | Mailpit web inbox (reverse-proxied, via `MAILPIT_WEB_URL`) — activation emails land here |

No other host ports are published. Postgres is internal to the compose
network — reach it with
`docker exec -it oumma-postgres-1 psql -U oumma -d oumma`.

> The bundled Keycloak defaults in `docker-compose.yaml`
> (`brief-test-realm`, clients `briefuserapp`/`oumma-admin`) are the **local
> test realm** so the demo works out of the box. For anything beyond local
> development, create your own realm and clients and override `KEYCLOAK_*`.

## Licensing

Apache License 2.0 — see [LICENSE](LICENSE).

## Architecture Overview

```
┌────────────┐  POST /{process_id}  ┌──────────────┐
│   Client   │ ──────────────────►  │  HTTP Handler │
│            │ ◄──────────────────  │              │
└────────────┘     202 Accepted     └──────┬───────┘
                                           │
                                           ▼
                                    ┌──────────────┐
                                    │    Engine     │
                                    │  (goroutine)  │
                                    └──────┬───────┘
                                           │
                          ┌────────────────┼────────────────┐
                          ▼                ▼                ▼
                   ┌────────────┐   ┌────────────┐   ┌────────────┐
                   │ LLM Client │   │ Strategies  │   │ Repository │
                   │ (OpenAI)   │   │ (Executors) │   │ (In-Memory)│
                   └────────────┘   └────────────┘   └────────────┘
```

---

## Project Structure

```
oumma/
├── cmd/
│   └── main.go                  # Entry point, HTTP server, YAML loader
├── internal/
│   ├── domain/
│   │   ├── task.go              # Task, TaskType, TaskConfig
│   │   ├── process.go           # ProcessDefinition, Edge, topology helpers
│   │   ├── state.go             # ExecutionState, ProcessStatus, UUID
│   │   ├── llm_log.go           # LLMLog struct for LLM call logging
│   │   ├── commentary.go        # TaskComment, TaskAttachment
│   │   └── yaml.go              # YAML parser & validator
│   ├── ai/
│   │   ├── client.go            # OpenAI-compatible LLM client (with log capture)
│   │   └── prompts.go           # Prompt templates (input prep + topology routing)
│   ├── engine/
│   │   ├── engine.go            # Core execution loop, resume logic, /ask tool loop
│   │   ├── strategies.go        # TaskExecutor interface + START/END/SYSTEM strategies
│   │   └── tools.go             # LLM data-service tools (in-process, claims-authorized)
│   ├── handler/
│   │   ├── handler.go           # HTTP handlers (RESTful process & task routes)
│   │   └── auth.go              # RequireAuth / RequireAuthFunc middleware + claims context
│   ├── auth/                    # Go port of brief-auth-backend AuthController → /api/auth
│   │   ├── service.go           # Keycloak client (password/refresh/introspect/admin API)
│   │   └── handlers.go          # /api/auth/signin|signout|refresh|session/active|health
│   ├── processconfig/           # Go port of UserProcess+ManagementController → YAML-backed
│   │   ├── types.go             # app: YAML config + JSON wire DTOs (Java-parity names)
│   │   ├── store.go             # YAML load / hot reload / atomic write-back
│   │   ├── userprocess.go       # /api/user-processes/* (role-filtered read model)
│   │   └── management.go        # /api/management/* (Designer CRUD on YAML)
│   ├── documents/               # Go port of brief-dms-docservice → /dms-api
│   │   ├── schema.go            # dms_documents / dms_tags / versions tables
│   │   ├── service.go           # Document + tag store (Postgres)
│   │   ├── handlers.go          # /dms-api HTTP handlers (group-authorized)
│   │   └── tools.go             # LLM tool bodies (list/get documents, list tags)
│   ├── forms/                   # Go port of brief-oper-data-index → /oper-api
│   │   ├── schema.go            # oper_process_forms / oper_steps / fields tables
│   │   ├── types.go             # ProcessForm/Step/FormField JSON models + request DTOs
│   │   ├── load.go              # Query loading, version reduction, dept filters
│   │   ├── user.go              # /user/forms endpoints (list/search/getform/versions)
│   │   ├── management.go        # /management/processform CRUD (versioning, overrides)
│   │   ├── health.go            # /health, /live, /ready, /started
│   │   └── tools.go             # LLM tool bodies (list/search forms)
│   ├── events/
│   │   ├── events.go            # Event models (process/task stream events)
│   │   ├── dispatcher.go        # Dispatcher interface
│   │   └── producer.go          # Kafka producer (segmentio/kafka-go) + log fallback
│   └── repository/
│       ├── store.go             # Repository + LLMLogStore interfaces, in-memory impl
│       └── postgres.go          # PostgreSQL implementation (JSONB)
├── migrations/
│   ├── 001_init.sql             # Initial database schema
│   └── 002_comments_attachments.sql  # Comments & attachments tables
├── workflows/
│   ├── order-processing.yaml    # Sample order workflow
│   ├── onboarding.yaml          # Banking onboarding workflow
│   └── declaration-sinistre.yaml # Insurance claim workflow
├── docs/                        # Uploaded document files (DMS upload dir, gitignored)
├── Dockerfile
├── build.sh                     # Docker buildx build script
├── docker-compose.yaml          # Postgres + oumma stack
├── docker-compose.local.yml     # Local dev overrides (LLM, auth, kafka)
├── .dockerignore
├── go.mod
└── go.sum
```

---

## Authentication & Authorization

Every route (except `GET /health` and the public `/api/auth/*` routes) requires an `Authorization: Bearer <token>` header. The `RequireAuthFunc` middleware (`internal/handler/auth.go`) validates tokens **in-process** via `internal/auth` (`auth.Service.ValidateSession`): the token is introspected against Keycloak (`/realms/{realm}/protocol/openid-connect/token/introspect`) and accepted only when `active: true`. The caller's claims (`preferred_username`, `groups` — leading `/` stripped — and `department`) are stored in the request context for downstream handlers (`handler.ClaimsFromContext`).

The auth backend itself (former `brief-auth-backend` Java service, `:8082`) is now served by oumma at `/api/auth/*` — see the in-process services table below.

The middleware then enriches the request context with claims decoded from the JWT payload:

- **`preferred_username`** — the caller's identity (used for audit fields like `deleted_by`)
- **`groups`** — token groups with the Keycloak `/` prefix stripped (e.g. `/CustRelations` → `CustRelations`). Enforced by the DMS endpoints (see below) and the oper `searchSavedForms` group check.
- **`department`** — flat string array of allowed department codes. The engine uses it to scope `GET /processes/{id}` and `POST .../ask`: an instance is only visible when its `global_context.department.codes` intersects the caller's codes (tokens without departments see everything).

### Group-based access control (DMS)

| Endpoint | Rule |
|---|---|
| `POST /dms-api/documents/upload` | the form's `groups` field must be non-empty and intersect the caller's token groups (else 403) |
| `GET /dms-api/documents` (list), `DELETE /documents/{id}`, `GET /documents/{id}/versions` | `groups` query param required, must intersect the caller's groups (else 403) |
| `GET /documents/{id}`, `PUT /documents/{id}`, `GET /documents/search` | when `groups` is passed it must intersect the caller's groups |
| `GET /documents/{id}/download`, all `/dms-api/tags*` | authenticated only (no group check) |

### Department filtering (oper-data-index)

`/oper-api/user/forms*` accept a `departments` query param; matching is done against the `department` node inside each step's `form_input` JSON (array, string, or `{"codes": [...]}` object). `searchSavedForms` additionally accepts a `groups` param: the caller must belong to one of the requested groups (group names are compared after truncating at `_` and lowercasing) or be in `Designer`.

---

## In-Process Data Services

The two former Java Quarkus sidecars were refactored to Go and are now integral parts of this binary: one process, one mux, one Postgres database. They were removed from the deployment stack entirely (`deploy-dev.sh` and the standalone compose files no longer exist).

### `/dms-api` — Document Management Service (port of `brief-dms-docservice`)

| Method & Path | Description |
|---|---|
| `POST /dms-api/documents/upload` | Multipart upload (`file`, `type`, `size`, `userId`, `processId`, `processInstanceId`, `organizationId`, `description`, `tags`, `groups`). Max 10 MB; types: `pdf docx xlsx txt jpg png`. Files stored under `DMS_UPLOAD_DIR/{userId}/{org}/{process}/{instance}/{date}/` |
| `GET /dms-api/documents` | List by `userId` (filters: `tag`, `status`, `fromDate`, `toDate`, `page`, `size`, `sort`) — `groups` required |
| `GET /dms-api/documents/search` | Search by `organizationId`, `processId`, `processInstanceId`, `tags`, `keyword` (at least one required); pagination + `sort` |
| `GET /dms-api/documents/{id}` | Document metadata (404 when soft-deleted) |
| `PUT /dms-api/documents/{id}` | Update name/description/processInstanceId/tagIds/status |
| `DELETE /dms-api/documents/{id}` | Soft delete (`status=DELETED`) → 204 |
| `GET /dms-api/documents/{id}/versions` | Version list |
| `GET /dms-api/documents/{id}/download` | File bytes, `Content-Disposition: attachment` |
| `POST /dms-api/tags` | Create tag `{name, color?, processId?}` → 201 |
| `GET /dms-api/tags/{id}` · `PUT /dms-api/tags/{id}` | Get / update (PUT requires matching `processId`, else 403) |
| `DELETE /dms-api/tags/{id}` | Delete → 204 (409 when referenced by documents) |
| `GET /dms-api/tags/search?processId=&query=` | `{"data": [{process_id, tags: []}]}` |
| `GET /dms-api/tags/all?processId=` | Flat tag list |
| `GET /dms-api/tags/` (+`?search=`) | `{"results": [...]}` — app tag-editor contract |
| `POST /dms-api/tags/` · `PATCH /dms-api/tags/{id}/` | App tag-editor create (201) / partial update (200) |

Trailing-slash variants (`/documents/{id}/`, `/documents/{id}/download/`, `/tags/{id}/`) are registered because the frontend builds URLs that way.

Tables (created in the shared oumma database at startup): `dms_documents`, `dms_document_versions`, `dms_tags`, `dms_document_tags`.

### `/oper-api` — Operational Data Index (port of `brief-oper-data-index`)

| Method & Path | Description |
|---|---|
| `GET /oper-api/user/forms` | List saved/active forms. Params: `process_id`, `form_id`, `step_id`, `version`, `activeform` (true = has process instance, false = saved drafts), `departments`. Steps are reduced to the latest version per `form_id`/`step_id` |
| `GET /oper-api/user/forms/searchSavedForms` (+`/searchform` alias) | Keyword (`query` tokens), `field_name`/`field_value`, `process_id`, `step_id`, `limit`, `activeform` (default: active only), `departments`, `groups` |
| `GET /oper-api/user/forms/getform?form_id=` | Single form at its latest version |
| `GET /oper-api/user/forms/formversions` | All versions (`detailed=no` → latest only); `form_id` repeatable |
| `POST /oper-api/management/processform` | Create form (steps get generated `form_id` UUIDs, `version=1`). `field_unique` precheck short-circuits with `200 {formexists, form_id}` |
| `PUT /oper-api/management/processform` | New version of a step (`form_id`+`step.step_id` locate it). `source: "process"` overrides the latest version in place instead. Unique fields may not be changed/removed (403) |
| `GET /oper-api/management/processform?processId=` | All forms of a process |
| `DELETE /oper-api/management/processform` | Body: `{process_id, steps:[{step_id, form_id, deleted_reason}]}`; steps attached to active instances are blocked; audit rows written to `oper_process_form_deleted` |
| `DELETE /oper-api/management/processform/steps?process_id=&form_id=&step_id=` | Delete a form's steps (403 when attached to active instances) |
| `GET /oper-api/management/auth-test` | Token check → `{message, hasValidToken, username}` |
| `GET /oper-api/health` (+`/live`, `/ready`, `/started`) | Health endpoints (`/ready` pings the DB) |

All list responses sanitize sensitive field properties (`field_db_query`, `field_database_user`, `field_database_password`, `api_key`). Step JSON carries `process_instance_id` as an array and epoch-millis `created_at` as ISO-8601 — matching the original Java wire format exactly, so the frontend required no model changes.

Tables: `oper_process_forms`, `oper_steps`, `oper_form_fields`, `oper_form_field_options`, `oper_form_field_table_field_names`, `oper_process_form_deleted`.

### `/api/auth` — Authentication (port of `brief-auth-backend` AuthController)

| Method & Path | Description |
|---|---|
| `POST /api/auth/signin` | Keycloak password grant (`{username, password}`) → `AuthResponse` incl. `user_info` claims |
| `POST /api/auth/refresh` | Keycloak refresh grant (`{refresh_token}`) |
| `POST /api/auth/signout` | Revokes the refresh token; always 200 `{message, success}` |
| `GET /api/auth/session/active` | Introspects the Bearer token → `SessionStatusResponse {active, user_info, expires_in}` (200 even when inactive — app contract) |
| `GET /api/auth/health` | Liveness |

The same `auth.Service` validates tokens for every other route **in-process** (no HTTP hop). Wire formats match the former Java service exactly.

### `/api/user-processes` + `/api/management` — Process/Role Config (port of `UserProcessController` + `ManagementController`)

Role-based process configuration no longer lives in a Postgres database — it is the `app:` section of each `workflows/{process_id}.yaml` file (see [Workflow Definition (YAML)](#workflow-definition-yaml)). `internal/processconfig` loads the directory at startup, hot-reloads on file changes (5 s poll), and serves both controllers with the Java wire formats (incl. the `field_multple_choice` typo and the mixed snake/camel case): access is role-filtered by Keycloak groups (`group == roleId` or prefix before `_`); `Designer` bypasses the filters.

| Method & Path | Description |
|---|---|
| `GET /api/user-processes/my-accessible-processes` | Processes the caller may run; roles filtered to the caller's groups; `doc_roles` always = all process roles with all permissions |
| `GET /api/user-processes/task-groups[/{processId}]` | Task-group items/views (group_id = process id) |
| `GET /api/user-processes/forms/{processId}[/{stepId}]` | Form structure (steps + fields) — the app renders forms from this |
| `GET /api/user-processes/ai-params/{processId}` | LLM model config for a process |
| `GET /api/user-processes/search-items/{processId}/{roleIds}` | Localized field labels; comma-separated roles; bare array; 404/403 semantics as Java |
| `GET /api/user-processes/authorised-search/{processId}/{roleIds}` | Roles a role may search in (bare array) |
| `GET /api/user-processes/view-access-roles/{processId}/{roleIds}` | View-access role ids (bare array) |
| `GET /api/user-processes/templates/{processId}` (+`?allowed_groups=`) · `POST /api/user-processes/templates` | Print templates filtered by `allowed_groups`/`allowed_steps` |
| `GET /api/user-processes/extract-all` | Processes owned by the caller's JWT subject |
| `GET /api/management/extract-all` · `extract-by-id/{id}` · `extract-by-role/{roleId}` · `search?name=` · `templates/{id}` | Admin reads (Designer only) |
| `POST /api/management/insert-process` · `PUT update-process/{id}` · `DELETE delete-process/{id}` | Process CRUD — read-modify-write of the YAML file (atomic, hot-reloaded) |
| `POST /api/management/add-role/{id}` · `add-action-group/{id}/{role}/{task}` | Role + task/action-group config |
| `POST /api/management/create-task-group` · `add-task-items/{groupId}` · `DELETE delete-task-group/{groupId}` | Task-group config |
| `POST /api/management/add-forms` · `ai-params` | Step/field and AI-param config (external field validation ported) |
| `POST /api/management/add-search-item` · `add-authorised-search` · `add-view-access-roles` · `add-template` | Role/template config |
| `POST /api/management/create-user` · `POST confirm-email/{userId}` | Keycloak Admin API user management (dedicated `oumma-admin` client) |

See `AUTH_BACKEND_PORT_PLAN.md` for the full port spec, data migration notes and parity results.

---

## AI Assistant (`/ask`) & LLM Tools

`POST /processes/{process_id}/{id}/ask` answers a user question about a running instance, grounded in the instance's live state (global context, task outputs, execution track) plus optional UI context:

```json
{ "question": "Which documents are attached to this instance?", "context": "...", "lang": "fr" }
```

The engine runs an agentic tool loop (max 3 rounds): the LLM may emit a fenced ```tool block naming one of the in-process data-service tools, the engine executes it **in the same process** (authorized via the caller's claims from the request context — no HTTP hop, no token forwarding), appends the result, and asks the LLM again until it answers.

| Tool | Backing call |
|---|---|
| `list_documents` | DMS `searchDocuments` (processId / processInstanceId / keyword), scoped to the caller's groups |
| `get_document` | DMS `getDocument(id)` |
| `list_document_tags` | DMS `allTagsByProcessId` |
| `list_forms` | oper `userForms` (`active` flag) |
| `search_forms` | oper `searchSavedForms` (keyword) |

Tools are registered only when `DATABASE_URL` is set (the data services require Postgres). LLM calls for `/ask` are logged to `llm_logs` like every other engine LLM call.

---

### `ExecutionState` — Process Instance (runtime state)

| Field                | Type              | Description                                          |
|----------------------|-------------------|------------------------------------------------------|
| `process_id`         | `string` (UUID)   | Unique identifier for this running instance          |
| `definition_id`      | `string`          | References the YAML `process_id` (the "map")         |
| `current_task_id`    | `string`          | The task the engine will execute next                 |
| `last_completed_task_id` | `string`      | The most recently completed task                      |
| `status`             | `enum`            | `RUNNING` · `PAUSED` · `COMPLETED` · `FAILED` · `ABORTED` |
| `active_task_instance_id` | `string` (UUID) | UUID of the currently paused HUMAN task instance (cleared on resume/abort) |
| `global_context`     | `map[string]any`  | Accumulated key-value context across all tasks        |
| `task_outputs`       | `map[string]any`  | Output of each completed task, keyed by task_id       |
| `created_at`         | `time.Time`       | Instance creation timestamp (UTC)                     |
| `updated_at`         | `time.Time`       | Last state mutation timestamp (UTC)                   |
| `error`              | `string`          | Error message if status is `FAILED`                   |

### `ProcessDefinition` — Workflow Blueprint (loaded from YAML)

| Field          | Type               | Description                                      |
|----------------|--------------------|-------------------------------------------------|
| `id`           | `string`           | From YAML `process_id`, used as `definition_id`  |
| `description`  | `string`           | Business intent of the workflow                  |
| `tasks`        | `map[string]*Task` | All tasks indexed by `task_id`                   |
| `start_task_id`| `string`           | Auto-detected from the task with `type: START`   |
| `end_task_id`  | `string`           | Auto-detected from the task with `type: END`     |
| `topology`     | `[]Edge`           | Directed edges defining allowed transitions      |

### `Task` — Workflow Node

| Field           | Type              | Description                                       |
|-----------------|-------------------|--------------------------------------------------|
| `id`            | `string`          | Unique identifier within the process              |
| `type`          | `enum`            | `START` · `END` · `HUMAN` · `SYSTEM`              |
| `description`   | `string`          | What this task does                                |
| `group`         | `string`          | Role/group allowed to execute this task (e.g. `cashier`) |
| `input_schema`  | `json.RawMessage` | JSON Schema describing expected input              |
| `output_schema` | `json.RawMessage` | JSON Schema describing expected output             |
| `config`        | `TaskConfig`      | SYSTEM tasks: `url`, `method`, `headers`           |
| `metadata`      | `map[string]any`  | Arbitrary metadata                                 |

### `LLMLog` — LLM Call Log

Every LLM API call (input preparation and routing) is persisted with full request/response capture.

| Field           | Type              | Description                                       |
|-----------------|-------------------|--------------------------------------------------|
| `id`            | `string` (UUID)   | Unique log entry identifier                       |
| `process_id`    | `string`          | The process instance this call belongs to          |
| `task_id`       | `string`          | The task being processed                           |
| `operation`     | `string`          | `prepare_input` or `route`                         |
| `model`         | `string`          | Model name used (e.g. `qwen2.5-coder:7b`)         |
| `system_prompt` | `string`          | Full system prompt sent to the LLM                 |
| `user_prompt`   | `string`          | Full user prompt sent to the LLM                   |
| `response`      | `string`          | Raw LLM response text                              |
| `error`         | `string`          | Error message if the call failed                   |
| `duration_ms`   | `int64`           | Call duration in milliseconds                      |
| `created_at`    | `time.Time`       | Timestamp of the call                              |

### `TaskComment` — Human Comment on a Task Instance

| Field              | Type            | Description                                                    |
|--------------------|-----------------|----------------------------------------------------------------|
| `id`               | `string` (UUID) | Unique comment identifier                                      |
| `process_id`       | `string`        | The process instance this comment belongs to                   |
| `task_id`          | `string`        | The task being commented on (e.g. `manual_review`)             |
| `task_instance_id` | `string` (UUID) | The specific task execution this comment is tied to            |
| `content`          | `string`        | Free-form comment text                                         |
| `created_at`       | `time.Time`     | Timestamp (UTC)                                                |

### `TaskAttachment` — File or Link Attached to a Task Instance

| Field              | Type            | Description                                                    |
|--------------------|-----------------|----------------------------------------------------------------|
| `id`               | `string` (UUID) | Unique attachment identifier                                   |
| `process_id`       | `string`        | The process instance this attachment belongs to                |
| `task_id`          | `string`        | The task being annotated                                       |
| `task_instance_id` | `string` (UUID) | The specific task execution this attachment is tied to         |
| `name`             | `string`        | Display name (e.g. `invoice.pdf`)                              |
| `uri`              | `string`        | Resource location (URL, S3 path, etc.)                         |
| `deleted`          | `bool`          | Soft-delete flag                                               |
| `deleted_at`       | `*time.Time`    | Set when soft-deleted; `null` otherwise                        |
| `created_at`       | `time.Time`     | Timestamp (UTC)                                                |

### `Edge` — Topology Connection

| Field       | Type     | Description                                           |
|-------------|----------|------------------------------------------------------|
| `from`      | `string` | Source task_id                                        |
| `to`        | `string` | Destination task_id                                   |
| `condition` | `string` | Natural-language condition the LLM evaluates          |

### Storage

The engine supports two storage backends:

**PostgreSQL (recommended for production)** — Set `DATABASE_URL` to enable. State and definitions are stored as JSONB in PostgreSQL, with all LLM call logs persisted in a dedicated `llm_logs` table. Schema is auto-migrated on startup.

**In-memory (default)** — When `DATABASE_URL` is not set, the engine uses an in-memory store. All data is lost on restart. LLM logs are still captured in memory and queryable via the API.

```go
type Repository interface {
    SaveState(ctx context.Context, state *ExecutionState) error
    GetState(ctx context.Context, processID string) (*ExecutionState, error)
    ListStates(ctx context.Context) ([]*ExecutionState, error)
    RegisterDefinition(ctx context.Context, def *ProcessDefinition) error
    GetDefinition(ctx context.Context, defID string) (*ProcessDefinition, error)
}

type LLMLogStore interface {
    SaveLLMLog(ctx context.Context, log *LLMLog) error
    ListLLMLogs(ctx context.Context, processID string) ([]*LLMLog, error)
}

type TaskCommentStore interface {
    SaveComment(ctx context.Context, c *TaskComment) error
    ListComments(ctx context.Context, processID, taskInstanceID string) ([]*TaskComment, error)
}

type TaskAttachmentStore interface {
    SaveAttachment(ctx context.Context, a *TaskAttachment) error
    GetAttachment(ctx context.Context, id string) (*TaskAttachment, error)
    ListAttachments(ctx context.Context, processID, taskInstanceID string) ([]*TaskAttachment, error)
    SoftDeleteAttachment(ctx context.Context, id string) error
}
```

The PostgreSQL implementation (`PostgresStore`) satisfies both interfaces. To swap in a different database, implement these methods.

---

## Workflow Definition (YAML)

Place `.yaml` files in the `processes` volume. Each file defines one workflow. The filename can be anything; the `process_id` field inside the file is the identifier used by the API.

Each file may additionally carry an `app:` section — the role-based access config ported from `brief-auth-backend` (consumed by `/api/user-processes` and `/api/management`, see [In-Process Data Services](#in-process-data-services)). The engine ignores it; `processconfig` hot-reloads it (5 s poll) and management writes read-modify-write it atomically. See `workflows/onboarding.yaml` for the full shape: `process_name`, `process_icon_path`, `process_link`, `can_be_launched`, `company`, `department_codes`, `user_id`, `roles[]` (`role_id`, `role_name`, `task_roles[]`, `view_access_roles[]`, `authorised_search[]`, `doc_roles[]`, `role_tasks[]` with `task_actions[]`/`action_items[]`), `forms[]` (steps/fields, incl. `field_multple_choice`), `ai` (model + prompts), `templates[]`, `task_groups[]`.

```yaml
process_id: order-processing
description: End-to-end order processing workflow.

tasks:
  - task_id: start
    type: START
    description: Initialize the pipeline

  - task_id: validate_order
    type: SYSTEM
    description: Validate order via external API
    group: system
    input_schema: |
      {"type":"object","properties":{"order_id":{"type":"string"}}}
    config:
      url: https://api.example.com/validate
      method: POST
      headers:
        Authorization: Bearer ${TOKEN}

  - task_id: manual_review
    type: HUMAN
    description: Human reviews the order
    group: cashier

  - task_id: process_payment
    type: SYSTEM
    description: Charge the customer
    group: system
    config:
      url: https://api.example.com/pay
      method: POST

  - task_id: end
    type: END
    description: Finalize

topology:
  - from: start
    to: validate_order
    condition: always
  - from: validate_order
    to: manual_review
    condition: order is valid
  - from: validate_order
    to: end
    condition: order is invalid
  - from: manual_review
    to: process_payment
    condition: approved
  - from: manual_review
    to: end
    condition: rejected
  - from: process_payment
    to: end
    condition: always
```

### Departments & Groups

- **`departments`** — a list of organizational units provided as **input when starting a process** (in the body of `POST /{process_id}`). Stored in `global_context.departments`. Used by the engine to scope the process to specific org units.
- **`group`** — the role allowed to execute a task, defined per-task in the YAML (e.g. `cashier`, `manager`, `system`).

Example: a process started with `"departments": ["operations", "finance"]` stores those values in `global_context`. When the process pauses at a HUMAN task, the caller uses `GET /{process_id}/{id}` to find the instance (checking `active_task_instance_id`) and `GET /{process_id}/{id}/{task_name}/{task_id}` to inspect the task.

### Task Types

| Type     | Behavior                                                                 |
|----------|-------------------------------------------------------------------------|
| `START`  | Lifecycle marker. Initializes the process, uses initial context as input |
| `END`    | Lifecycle marker. Finalizes the process, terminates the loop             |
| `SYSTEM` | Executes an outbound HTTP REST call synchronously; result is merged into `global_context` and forwarded to the next task |
| `HUMAN`  | Pauses execution, persists state, waits for `POST /{process_id}/{id}/{task_name}/{task_id}` |

### SYSTEM Task Configuration

A `SYSTEM` task makes an outbound HTTP call. The engine serialises the prepared input as JSON, POSTs it to the configured URL, and merges the JSON response body into `global_context` as the task output. From the frontend's perspective a `SYSTEM` task is identical to a `HUMAN` task result — it appears in `task_outputs` — but it never causes the process to pause.

```yaml
- task_id: validate_order
  type: SYSTEM
  description: Validate order details and check inventory via external API
  input_schema: |
    {
      "type": "object",
      "properties": {
        "order_id":    {"type": "string"},
        "items":       {"type": "array"},
        "customer_id": {"type": "string"}
      },
      "required": ["order_id"]
    }
  output_schema: |
    {
      "type": "object",
      "properties": {
        "valid":            {"type": "boolean"},
        "inventory_status": {"type": "string"},
        "message":          {"type": "string"}
      }
    }
  config:
    url: https://httpbin.org/post
    method: POST
    headers:
      X-Service: order-validation
      Authorization: Bearer ${TOKEN}
```

| `config` field | Required | Description |
|----------------|----------|-------------|
| `url`          | yes      | Full URL of the external REST endpoint |
| `method`       | no       | HTTP method — defaults to `POST` |
| `headers`      | no       | Key-value map of request headers |

Both `input_schema` and `output_schema` follow the same JSON Schema format used by `HUMAN` tasks — they describe the data contract between the process and the external service and are surfaced by the frontend in the same way.

### Topology Rules

- Every workflow must have exactly **one START** and **one END** task.
- Topology edges define which transitions are **possible** — the LLM chooses among them.
- The `condition` field is a natural-language hint the LLM evaluates against the task output.
- **Contextual Overrides**: when a user sends an `override` string with the complete-task request (`POST /{process_id}/{id}/{task_name}/{task_id}`), the LLM is allowed to deviate from topology and jump to any task in the graph.

---

## API Reference

All routes follow a RESTful structure. `{process_id}` is the YAML `process_id` value; `{id}` is the running instance UUID returned by the start endpoint.

---

### `POST /{process_id}/{id}/ask` — Ask the AI Assistant

Conversational, context-grounded answer about the instance (see [AI Assistant & LLM Tools](#ai-assistant-ask--llm-tools)). The caller's token must authorize the instance (department filter applies).

**Request Body:**
```json
{ "question": "What is the current status of the dossier?", "context": "active selection…", "lang": "fr" }
```

**Response:** `200 OK`
```json
{ "answer": "…", "process_id": "d3f1a2b4-…" }
```

---

### `POST /{process_id}` — Start Instance

Start a new process instance. The entire request body is used as the initial `global_context`.

**Request Body (optional):**
```json
{
  "order_id": "ORD-001",
  "customer_id": "CUST-42",
  "departments": ["operations", "finance"],
  "lang": "fr"
}
```

> **`lang`** — optional BCP-47 language code. When set, the LLM generates all human-readable reasoning (explanations, comments, routing rationale) in the specified language for the lifetime of the process. JSON field names, task IDs, and boolean/numeric values always remain in English. See [Language Support](#language-support) for the full list of supported codes.

**Response:** `202 Accepted`
```json
{
  "process_id": "d3f1a2b4-5678-4c9e-a0b1-c2d3e4f56789",
  "status": "RUNNING"
}
```

---

### `GET /{process_id}` — List Instances

Returns all instances for the given process definition.

**Response:** `200 OK` — array of `ExecutionState` objects.

---

### `GET /{process_id}/{id}` — Get Instance

Returns the full `ExecutionState` for a specific instance.

**Response:** `200 OK`
```json
{
  "process_id": "d3f1a2b4-5678-4c9e-a0b1-c2d3e4f56789",
  "definition_id": "order-processing",
  "current_task_id": "manual_review",
  "last_completed_task_id": "validate_order",
  "status": "PAUSED",
  "active_task_instance_id": "task-456-uuid",
  "global_context": { "order_id": "ORD-001", "departments": ["operations", "finance"] },
  "task_outputs": {
    "validate_order": { "valid": true, "status_code": 200 }
  },
  "created_at": "2026-04-10T12:00:00Z",
  "updated_at": "2026-04-10T12:00:02Z"
}
```

Use `active_task_instance_id` alongside `current_task_id` to build task-scoped URLs when the process is in `PAUSED` state.

---

### `DELETE /{process_id}/{id}` — Abort Instance

Sends an abort signal to a running or paused process. The engine marks the process `ABORTED` and stops the execution goroutine.

**Response:** `200 OK`
```json
{
  "process_id": "d3f1a2b4-5678-4c9e-a0b1-c2d3e4f56789",
  "status": "ABORTED"
}
```

---

### `POST /{process_id}/{id}/{signal}` — Signal Instance

Delivers a named signal (e.g. `payment_timeout`, `expired`) to a running or paused process. The body is optional freeform context.

**Request Body (optional):**
```json
{ "reason": "no payment response in 30s" }
```

**Response:** `202 Accepted`
```json
{
  "process_id": "d3f1a2b4-5678-4c9e-a0b1-c2d3e4f56789",
  "signal": "payment_timeout",
  "status": "DELIVERED"
}
```

---

### `GET /{process_id}/{id}/{task_name}/{task_id}` — Get Task Instance

Returns the state of a paused HUMAN task instance, including its comments.

- `{task_name}` — the `task_id` from the YAML (e.g. `manual_review`)
- `{task_id}` — the value of `active_task_instance_id` from the parent instance

**Response:** `200 OK`
```json
{
  "process_id": "d3f1a2b4-5678-4c9e-a0b1-c2d3e4f56789",
  "task_name": "manual_review",
  "task_instance_id": "task-456-uuid",
  "status": "PAUSED",
  "global_context": { "order_id": "ORD-001" },
  "comments": []
}
```

---

### `POST /{process_id}/{id}/{task_name}/{task_id}` — Complete Task

Completes the paused HUMAN task and resumes the process. Task comments are automatically fetched and injected into the LLM routing context.

**Request Body:**
```json
{
  "input": {
    "approved": true,
    "reviewer_notes": "Order verified. Proceed to payment."
  },
  "override": "(optional) Override directive for the LLM router",
  "lang": "fr"
}
```

| Field      | Type   | Description |
|------------|--------|-------------|
| `input`    | object | Task output values merged into `global_context` |
| `override` | string | Optional override directive. See [Direct Task Override](#direct-task-override) |
| `lang`     | string | Optional BCP-47 language code. Updates `global_context["lang"]` immediately, affecting the routing LLM call for this step and all subsequent steps. See [Language Support](#language-support) |

**Response:** `202 Accepted`
```json
{
  "process_id": "d3f1a2b4-5678-4c9e-a0b1-c2d3e4f56789",
  "task_id": "manual_review",
  "status": "RESUMED"
}
```

---

### `POST /{process_id}/{id}/{task_name}/{task_id}/comments` — Add Comment

Adds a text comment to a task instance. Comments are persisted and injected into the LLM prompt on task completion.

**Request Body:**
```json
{ "content": "Customer is a returning buyer — safe to approve." }
```

**Response:** `201 Created`
```json
{
  "id": "cmt-uuid",
  "process_id": "d3f1a2b4-5678-4c9e-a0b1-c2d3e4f56789",
  "task_id": "manual_review",
  "task_instance_id": "task-456-uuid",
  "content": "Customer is a returning buyer — safe to approve.",
  "created_at": "2026-04-10T12:00:05Z"
}
```

---

### `GET /{process_id}/{id}/{task_name}/{task_id}/comments` — List Comments

Returns all comments for a task instance in creation order.

**Response:** `200 OK` — array of `TaskComment` objects.

---

### `POST /{process_id}/{id}/{task_name}/{task_id}/attachments` — Add Attachment

Attaches a named file or URI to a task instance.

**Request Body:**
```json
{
  "name": "invoice.pdf",
  "uri": "s3://my-bucket/invoices/invoice-001.pdf"
}
```

**Response:** `201 Created`
```json
{
  "id": "att-uuid",
  "process_id": "d3f1a2b4-5678-4c9e-a0b1-c2d3e4f56789",
  "task_id": "manual_review",
  "task_instance_id": "task-456-uuid",
  "name": "invoice.pdf",
  "uri": "s3://my-bucket/invoices/invoice-001.pdf",
  "deleted": false,
  "created_at": "2026-04-10T12:00:06Z"
}
```

---

### `DELETE /{process_id}/{id}/{task_name}/{task_id}/attachments/{at_id}` — Delete Attachment

Soft-deletes an attachment (`deleted: true`, `deleted_at` set in the database; the record is retained but excluded from future listings).

**Response:** `200 OK`
```json
{
  "attachment_id": "att-uuid",
  "status": "DELETED"
}
```

---

### `GET /definitions` — List Definitions

Returns all loaded workflow definitions.

**Response:** `200 OK` — array of `ProcessDefinition` objects.

---

## Direct Task Override

The `override` field on the complete-task endpoint (`POST /{process_id}/{id}/{task_name}/{task_id}`) has two distinct modes.

### LLM-Guided Override (soft)

A freeform instruction passed to the LLM alongside the topology. The LLM may use it to deviate from the declared edges (e.g. escalate, skip, repeat) but still decides the next task itself.

```json
{
  "input": { "approved": true },
  "override": "Customer is a VIP — escalate urgently to the fraud_check task"
}
```

The LLM routing response will include `"override_applied": true` in the SSE event payload.

### Direct Task Jump (deterministic)

When the override directive contains a recognized anchor phrase followed by a task ID that exists in the workflow, the engine **bypasses the LLM entirely** and jumps directly to the named task. No LLM call is made for routing; the transition is guaranteed.

Supported anchor phrases (case-insensitive, matched left-to-right):

| Phrase | Example |
|--------|---------|
| `forward directly to <task_id>` | `"forward directly to fraud_check"` |
| `directly to <task_id>` | `"directly to fraud_check"` |
| `forward to <task_id>` | `"forward to fraud_check"` |
| `jump to <task_id>` | `"jump to fraud_check"` |
| `skip to <task_id>` | `"skip to fraud_check"` |
| `go to <task_id>` | `"go to fraud_check"` |
| `route to <task_id>` | `"route to fraud_check"` |

**Example — jump from `manual_review` directly to `process_payment`, bypassing the LLM:**

```bash
curl -s -X POST \
  http://localhost:8080/order-processing/d3f1a2b4.../manual_review/task-456-uuid \
  -H "Content-Type: application/json" \
  -d '{
    "input": { "approved": true },
    "override": "jump to process_payment"
  }'
```

The resulting SSE `llm_routed` event will carry:
```json
{
  "next_task_id": "process_payment",
  "override_applied": true,
  "direct_jump": true
}
```

**Rules:**
- The target task ID must exist in the workflow definition (unknown IDs fall back to LLM routing).
- Longest match wins when multiple task IDs could match the tail of the directive.
- The jump is recorded in the execution track with a `"direct override"` reason string.
- The process can jump to any task regardless of the declared topology edges.

---

## Language Support

The engine can generate all LLM-produced human-readable content (reasoning explanations, routing rationale, task commentary) in a language other than English. JSON field names, task IDs, and boolean/numeric values always remain in English as defined by the workflow schema.

### Setting the language

**At process start** — include `"lang"` in the startup body. It is stored in `global_context["lang"]` and applies to every LLM call for the lifetime of the process:

```bash
curl -s -X POST http://localhost:8080/order-processing \
  -H "Content-Type: application/json" \
  -d '{
    "order_id": "ORD-001",
    "lang": "fr"
  }'
```

**At task completion** — include `"lang"` in the complete-task body. It updates `global_context["lang"]` immediately and takes effect from that routing step onward:

```bash
curl -s -X POST \
  http://localhost:8080/order-processing/.../manual_review/task-456-uuid \
  -H "Content-Type: application/json" \
  -d '{
    "input": { "approved": true },
    "lang": "ar"
  }'
```

Both the non-streaming (`POST /{task_id}`) and streaming (`POST /{task_id}/stream`) complete-task endpoints accept `lang`.

### Supported languages

| Code | Language |
|------|----------|
| `en` | English (default — no extra prompt overhead) |
| `fr` | French |
| `ar` | Arabic |
| `es` | Spanish |
| `de` | German |
| `zh` | Chinese (Mandarin) |
| `ja` | Japanese |
| `pt` | Portuguese |
| `it` | Italian |
| `nl` | Dutch |
| `ru` | Russian |
| `tr` | Turkish |

Any BCP-47 code not in the table above is still forwarded to the LLM using the raw code as the language name.

When `lang` is `"en"` or absent, no language instruction is appended to the prompt — there is no extra token cost for the default case.

---

## Engine Execution Flow

```
┌─────────┐
│  START  │
└────┬────┘
     │
     ▼
┌──────────────────────┐
│ Load current task    │◄───────────────────────┐
└────────┬─────────────┘                        │
         │                                      │
         ▼                                      │
┌──────────────────────┐                        │
│ Is END task?         │── yes ──► COMPLETED    │
└────────┬─────────────┘                        │
         │ no                                   │
         ▼                                      │
┌──────────────────────┐                        │
│ LLM: Prepare Input   │                        │
│ (map context→schema) │                        │
└────────┬─────────────┘                        │
         │                                      │
         ▼                                      │
┌──────────────────────┐                        │
│ Execute Task         │                        │
│ (Strategy Pattern)   │                        │
│                      │                        │
│  SYSTEM → HTTP call  │                        │
│  HUMAN  → PAUSE      │                        │
│  START  → pass-thru  │                        │
└────────┬─────────────┘                        │
         │                                      │
         ▼                                      │
┌──────────────────────┐                        │
│ LLM: Route Next Task │                        │
│ (topology + override)│                        │
└────────┬─────────────┘                        │
         │                                      │
         ▼                                      │
┌──────────────────────┐                        │
│ Update state, persist│────────────────────────┘
└──────────────────────┘
```

---

## Configuration

All configuration is via environment variables:

| Variable          | Default                    | Description                                  |
|-------------------|----------------------------|----------------------------------------------|
| `DATABASE_URL`    | *(empty)*                  | PostgreSQL connection URL. If unset, uses in-memory storage **and disables `/dms-api`, `/oper-api` and the LLM data-service tools** |
| `ADDR`            | `:8080`                    | HTTP listen address                          |
| `PROCESSES_DIR`   | `/processes`               | Directory to scan for workflow YAML files    |
| `OPENAI_BASE_URL` | *(placeholder)*            | Process-mode LLM base URL (OpenAI-compatible: vLLM, Ollama, OpenAI…) — **no static default; must be set** |
| `OPENAI_MODEL`    | `your-model-name`          | Process-mode model name                        |
| `OPENAI_API_KEY`  | *(empty)*                  | Process-mode API key (empty = keyless local server) |
| `DESIGNER_OPENAI_BASE_URL` / `_MODEL` / `_API_KEY` | *(empty)* | **Designer-agent LLM** — the design chat runs on this endpoint/model; empty = falls back to the process LLM |
| `LLM_CALL_TIMEOUT_SEC` | `300`              | Per-LLM-call deadline                          |
| `MCP_TOOLS_DRY_RUN` | `true`                 | MCP tool steps simulate delivery (log payload); `false` = real outbound calls |
| `MCP_TELEGRAM_API_BASE` | `https://api.telegram.org` | Telegram Bot API base                        |
| `MCP_GMAIL_GATEWAY` | *(empty)*                | Fallback email gateway for Gmail tool steps    |
| `MCP_WHATSAPP_API_BASE` | `https://graph.facebook.com/v18.0` | WhatsApp Cloud API base            |
| `MCP_HTTP_TIMEOUT_SEC` | `30`                  | Outbound tool-call timeout                     |
| `NORMAL_USER_TOKEN_QUOTA` | `20000`            | Monthly LLM token budget for normal users (0 = unlimited) |
| `DESIGNER_TOKEN_QUOTA`  | `50000`            | Monthly LLM token budget for designers (0 = unlimited) |
| `QUOTA_CLAIM_EMAIL`     | `contact@brief-dz.com` | Recipient of quota top-up requests             |
| `AUTH_BACKEND_URL` | `http://localhost:8082`   | (removed — token validation is in-process) |
| `AUTH_BACKEND_SESSION_ENDPOINT` | `/api/auth/session/active` | (removed) |
| `KEYCLOAK_URL` | `http://localhost:8080` | Keycloak base URL (token/introspect/admin APIs) |
| `KEYCLOAK_REALM` | `brief-test-realm` | Keycloak realm |
| `KEYCLOAK_CLIENT_ID` | `briefuserapp` | Client for password/refresh grants |
| `KEYCLOAK_CLIENT_SECRET` | — | Client secret for the grants above |
| `KEYCLOAK_ADMIN_CLIENT_ID` | `oumma-admin` | Dedicated admin client (create-user / confirm-email) |
| `KEYCLOAK_ADMIN_CLIENT_SECRET` | — | Secret of the admin client |
| `KAFKA_BROKERS`   | *(empty)*                  | Comma-separated broker list. When set, process/task events are produced to Kafka (`process.stream`, `task.stream`); otherwise events are log-only |
| `DMS_UPLOAD_DIR`  | `docs`                     | Where `/dms-api` stores uploaded document files (mount a volume, e.g. `./docs:/docs`) |
| `LLM_KEEP_ALIVE`  | `5m`                       | Ollama keep-alive hint forwarded to the LLM server |
| `LLM_NUM_CTX` / `LLM_NUM_BATCH` / `LLM_NUM_PREDICT` | `4096` / `512` / `256` | LM Studio / vLLM sampling options forwarded with each request |
| `LLM_TEMPERATURE` | `0.7`                      | Sampling temperature                         |
| `REAPER_INTERVAL_SEC` | `30`                   | Watchdog interval: fails RUNNING instances whose execution goroutine died |

You can also pass YAML file paths as CLI arguments to override directory scanning:

```bash
oumma workflows/a.yaml workflows/b.yaml
```

---

## Using Local AI Models

The engine uses the OpenAI Chat Completions protocol, which is also exposed by **Ollama** and **vLLM**. No code changes are needed — just set `OPENAI_BASE_URL` and `OPENAI_MODEL`. The `OPENAI_API_KEY` can be left empty for local models.

### Ollama

```bash
# 1. Install & start Ollama (https://ollama.com)
ollama serve

# 2. Pull a model
ollama pull llama3.1:8b

# 3. Run the engine against Ollama
export OPENAI_BASE_URL="http://localhost:11434/v1"
export OPENAI_MODEL="llama3.1:8b"
export OPENAI_API_KEY=""   # not required
./oumma
```

**Docker Compose (PostgreSQL + Ollama + Oumma):**

```yaml
services:
  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: oumma
      POSTGRES_PASSWORD: oumma
      POSTGRES_DB: oumma
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U oumma"]
      interval: 5s
      timeout: 3s
      retries: 5

  ollama:
    image: ollama/ollama:latest
    ports:
      - "11434:11434"
    volumes:
      - ollama_data:/root/.ollama

  oumma:
    build: .
    ports:
      - "8080:8080"
    volumes:
      - ./workflows:/processes
    environment:
      DATABASE_URL: postgres://oumma:oumma@postgres:5432/oumma?sslmode=disable
      OPENAI_BASE_URL: http://ollama:11434/v1
      OPENAI_MODEL: llama3.1:8b
      OPENAI_API_KEY: ""
    depends_on:
      postgres:
        condition: service_healthy
      - ollama

volumes:
  pgdata:
  ollama_data:
```

### vLLM

```bash
# 1. Start vLLM with OpenAI-compatible server
python -m vllm.entrypoints.openai.api_server \
  --model mistralai/Mistral-7B-Instruct-v0.3 \
  --port 8000

# 2. Run the engine against vLLM
export OPENAI_BASE_URL="http://localhost:8000/v1"
export OPENAI_MODEL="mistralai/Mistral-7B-Instruct-v0.3"
export OPENAI_API_KEY=""   # not required
./oumma
```

**Docker Compose (vLLM + Oumma):**

```yaml
services:
  vllm:
    image: vllm/vllm-openai:latest
    ports:
      - "8000:8000"
    command: ["--model", "mistralai/Mistral-7B-Instruct-v0.3", "--port", "8000"]
    deploy:
      resources:
        reservations:
          devices:
            - capabilities: [gpu]

  oumma:
    build: .
    ports:
      - "8080:8080"
    volumes:
      - ./workflows:/processes
    environment:
      OPENAI_BASE_URL: http://vllm:8000/v1
      OPENAI_MODEL: mistralai/Mistral-7B-Instruct-v0.3
      OPENAI_API_KEY: ""
    depends_on:
      - vllm

volumes: {}
```

### Recommended Local Models

| Provider | Model | Notes |
|----------|-------|-------|
| Ollama   | `llama3.1:8b` | Good balance of speed and quality |
| Ollama   | `llama3.1:70b` | Better reasoning, needs 48GB+ VRAM |
| Ollama   | `qwen2.5:14b` | Strong JSON/structured output |
| Ollama   | `mistral:7b` | Fast, lighter resource usage |
| vLLM     | `mistralai/Mistral-7B-Instruct-v0.3` | Production-grade serving |
| vLLM     | `meta-llama/Llama-3.1-8B-Instruct` | Best open-source quality |

> **Tip:** The engine requires the model to return valid JSON. Models with strong instruction-following (Llama 3.1, Qwen 2.5, Mistral) work best. Smaller models (< 7B) may produce malformed JSON and cause routing failures.

---

## Running Locally

```bash
# 1. Build
go build -o oumma ./cmd/main.go

# 2. Run (reads from ./workflows/ by default)
export OPENAI_API_KEY="sk-..."
./oumma
```

### Full Walkthrough — `order-processing` from Start to End

This traces every step of the sample workflow.

`start → validate_order → manual_review → process_payment → end`

```
┌─────────┐     ┌────────────────┐     ┌───────────────┐     ┌─────────────────┐     ┌───────┐
│  start  │────►│ validate_order │────►│ manual_review │────►│ process_payment │────►│  end  │
└─────────┘     └────────────────┘     └───────────────┘     └─────────────────┘     └───────┘
  START              SYSTEM                 HUMAN                  SYSTEM               END
 (auto)           group: system          group: cashier          group: system          (auto)
                    (auto)             (PAUSED → resume)           (auto)
```

**Step 1 — Start the process**

```bash
curl -s -X POST http://localhost:8080/order-processing \
  -H "Content-Type: application/json" \
  -d '{
    "order_id": "ORD-001",
    "customer_id": "CUST-42",
    "items": [{"sku": "WIDGET-A", "qty": 2}],
    "amount": 149.99,
    "departments": ["operations", "finance"],
    "lang": "fr"
  }'
```

Response (`202 Accepted`):
```json
{
  "process_id": "d3f1a2b4-5678-4c9e-a0b1-c2d3e4f56789",
  "status": "RUNNING"
}
```

The engine runs automatically: START → validate_order (HTTP call) → pauses at manual_review.

**Step 2 — List active instances**

```bash
curl -s http://localhost:8080/order-processing | jq .
```

Returns all instances for `order-processing`. A `PAUSED` instance has a non-empty `active_task_instance_id` — this is the task slot identifier needed to build task-scoped URLs.

**Step 3 — Inspect the paused instance**

```bash
curl -s http://localhost:8080/order-processing/d3f1a2b4-5678-4c9e-a0b1-c2d3e4f56789 | jq .
```

Response:
```json
{
  "process_id": "d3f1a2b4-5678-4c9e-a0b1-c2d3e4f56789",
  "definition_id": "order-processing",
  "current_task_id": "manual_review",
  "status": "PAUSED",
  "active_task_instance_id": "task-456-uuid",
  "global_context": {
    "order_id": "ORD-001",
    "customer_id": "CUST-42",
    "amount": 149.99,
    "departments": ["operations", "finance"]
  },
  "task_outputs": {
    "validate_order": {"valid": true, "inventory_status": "in_stock", "status_code": 200}
  }
}
```

Build the task URL from `current_task_id` + `active_task_instance_id`.

**Step 4 — Get the task instance**

```bash
curl -s http://localhost:8080/order-processing/d3f1a2b4-5678-4c9e-a0b1-c2d3e4f56789/manual_review/task-456-uuid | jq .
```

**Step 5 — Add a reviewer comment**

```bash
curl -s -X POST \
  http://localhost:8080/order-processing/d3f1a2b4-5678-4c9e-a0b1-c2d3e4f56789/manual_review/task-456-uuid/comments \
  -H "Content-Type: application/json" \
  -d '{"content": "Returning customer, order history is clean. Safe to approve."}'
```

Response (`201 Created`):
```json
{
  "id": "cmt-001-uuid",
  "task_instance_id": "task-456-uuid",
  "content": "Returning customer, order history is clean. Safe to approve.",
  "created_at": "2026-04-10T12:00:05Z"
}
```

Comments are stored in the database and automatically injected into the LLM routing context when the task is completed.

**Step 6 — Attach a supporting document**

```bash
curl -s -X POST \
  http://localhost:8080/order-processing/d3f1a2b4-5678-4c9e-a0b1-c2d3e4f56789/manual_review/task-456-uuid/attachments \
  -H "Content-Type: application/json" \
  -d '{"name": "customer-history.pdf", "uri": "s3://docs/customer-history.pdf"}'
```

**Step 7 — Complete the task (approve)**

```bash
curl -s -X POST \
  http://localhost:8080/order-processing/d3f1a2b4-5678-4c9e-a0b1-c2d3e4f56789/manual_review/task-456-uuid \
  -H "Content-Type: application/json" \
  -d '{
    "input": {
      "approved": true,
      "reviewer_notes": "Verified. Proceed to payment."
    }
  }'
```

Response (`202 Accepted`):
```json
{
  "process_id": "d3f1a2b4-5678-4c9e-a0b1-c2d3e4f56789",
  "task_id": "manual_review",
  "status": "RESUMED"
}
```

The engine fetches the task's comments, injects them into the LLM routing context alongside the reviewer input, then routes to `process_payment` → `end`.

**Step 8 — Verify completion**

```bash
curl -s http://localhost:8080/order-processing/d3f1a2b4-5678-4c9e-a0b1-c2d3e4f56789 | jq .
```

```json
{
  "status": "COMPLETED",
  "current_task_id": "end",
  "task_outputs": {
    "validate_order": {"valid": true, "inventory_status": "in_stock"},
    "manual_review": {"approved": true, "reviewer_notes": "Verified. Proceed to payment."},
    "process_payment": {"transaction_id": "TXN-98765", "status": "success"}
  }
}
```

### Alternative Paths

**Reject at review:**
```bash
curl -s -X POST \
  http://localhost:8080/order-processing/d3f1a2b4-5678-4c9e-a0b1-c2d3e4f56789/manual_review/task-456-uuid \
  -H "Content-Type: application/json" \
  -d '{"input": {"approved": false, "reviewer_notes": "Suspected fraud."}}'
```
The LLM evaluates the topology and routes `manual_review → end`, skipping payment.

**Override — direct jump to a specific task:**
```bash
curl -s -X POST \
  http://localhost:8080/order-processing/d3f1a2b4-5678-4c9e-a0b1-c2d3e4f56789/manual_review/task-456-uuid \
  -H "Content-Type: application/json" \
  -d '{
    "input": {"approved": true},
    "override": "jump to process_payment"
  }'
```
The engine detects the `jump to <task_id>` anchor, bypasses the LLM entirely, and routes directly to `process_payment`. The SSE `llm_routed` event will carry `"direct_jump": true`.

**Override — LLM-guided deviation:**
```bash
curl -s -X POST \
  http://localhost:8080/order-processing/d3f1a2b4-5678-4c9e-a0b1-c2d3e4f56789/manual_review/task-456-uuid \
  -H "Content-Type: application/json" \
  -d '{
    "input": {"approved": true},
    "override": "VIP customer — escalate urgently to fraud_check regardless of topology"
  }'
```
No recognized anchor → freeform LLM-guided override. The LLM may pick any reachable task. `"override_applied": true` is set in the SSE event.

**Switch language mid-process:**
```bash
curl -s -X POST \
  http://localhost:8080/order-processing/d3f1a2b4-5678-4c9e-a0b1-c2d3e4f56789/manual_review/task-456-uuid \
  -H "Content-Type: application/json" \
  -d '{
    "input": {"approved": true},
    "lang": "ar"
  }'
```
Updates `global_context["lang"]` to `"ar"` before the routing call; all subsequent LLM output for this process will be in Arabic.

**Abort a stuck process:**
```bash
curl -s -X DELETE \
  http://localhost:8080/order-processing/d3f1a2b4-5678-4c9e-a0b1-c2d3e4f56789
```
The engine marks the process `ABORTED` and stops the goroutine.

**Send a signal:**
```bash
curl -s -X POST \
  http://localhost:8080/order-processing/d3f1a2b4-5678-4c9e-a0b1-c2d3e4f56789/payment_timeout \
  -H "Content-Type: application/json" \
  -d '{"reason": "no payment response in 30s"}'
```

---

## Docker

### Build

```bash
docker build -t oumma .
```

### Run

Mount your workflow YAML files into the `/processes` volume:

```bash
# With OpenAI
docker run -d \
  --name oumma \
  -p 8080:8080 \
  -v $(pwd)/workflows:/processes \
  -e OPENAI_API_KEY="sk-..." \
  -e OPENAI_MODEL="gpt-4o" \
  oumma

# With Ollama (running on the host)
docker run -d \
  --name oumma \
  -p 8080:8080 \
  -v $(pwd)/workflows:/processes \
  -e OPENAI_BASE_URL="http://host.docker.internal:11434/v1" \
  -e OPENAI_MODEL="llama3.1:8b" \
  oumma
```

The engine scans `/processes` at startup and loads every `.yaml` / `.yml` file it finds. Each file's `process_id` is the `{process_id}` segment used in all API routes (e.g. `POST /order-processing` to start a new instance).

### Volume Structure

```
/processes/                        # Docker volume (mapped)
├── order-processing.yaml          # process_id: order-processing
├── user-onboarding.yaml           # process_id: user-onboarding
└── refund-workflow.yaml           # process_id: refund-workflow
```

---

## Build Script (buildx)

The included `build.sh` uses Docker Buildx for multi-platform images:

```bash
# Local build (current platform, loaded into Docker)
./build.sh

# Custom tag
IMAGE_TAG=v1.2.0 ./build.sh

# Multi-platform push to registry
REGISTRY=ghcr.io/myorg PUSH=true IMAGE_TAG=v1.2.0 ./build.sh

# Specify platforms
PLATFORMS="linux/amd64,linux/arm64,linux/arm/v7" PUSH=true ./build.sh
```

### Script Variables

| Variable      | Default                       | Description                     |
|---------------|-------------------------------|---------------------------------|
| `IMAGE_NAME`  | `oumma`                       | Docker image name               |
| `IMAGE_TAG`   | `latest`                      | Image tag                       |
| `PLATFORMS`   | `linux/amd64,linux/arm64`     | Target platforms for buildx     |
| `PUSH`        | `false`                       | Push to registry (`true`/`false`) |
| `REGISTRY`    | *(empty)*                     | Registry prefix (e.g. `ghcr.io/org`) |
