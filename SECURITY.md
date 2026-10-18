# Security Policy

## Reporting a vulnerability

**Please do not open a public issue for security vulnerabilities.**

Report them privately so they can be fixed before public disclosure:

- **Email:** `contact@brief-dz.com`
  **Subject prefix:** `[Security]` (e.g. `[Security] Agent quota bypass via crafted token`)
- Include: the affected component (engine / auth / agent / design APIs),
  a minimal reproduction, and the version (git commit or compose build date).

We aim to acknowledge reports within **48 hours** and to ship a fix in a
release within **30 days** where feasible.

## Scope

In scope: the Go engine (workflow execution, auth proxy, management &
design APIs, agent endpoints, MCP tool execution) and its compose
deployment.

Out of scope: Keycloak itself, third-party LLM endpoints (vLLM/Ollama/
OpenAI), and user-provided content (process YAML) that merely *configures*
the engine.

## Things to double-check before reporting (by design)

- Endpoints require a valid Keycloak bearer token; the `Authorization`
  guard is global (public paths: `/health`, `/api/auth/*`, `/mailpit/*`).
- Role-based action checks (`RoleActionChecker`) run **server-side** on
  task completion, comments and attachments.
- MCP tool steps are **dry-run by default** (`MCP_TOOLS_DRY_RUN`); real
  outbound calls only happen when explicitly enabled with real
  credentials.
- Design deployment is a human-only action; the agent cannot deploy or
  discard versions.
