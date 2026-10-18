# Contributing to Oumma (engine)

Thank you for contributing! Oumma is the Go workflow engine and platform
backend. This guide gets you from clone to a green local stack.

## Ground rule

**`main` must always be shippable**: a fresh clone + `docker compose up`
must work, and `go build ./... && go vet ./... && go test ./...` must be
green (CI enforces both on every PR).

## Running locally

```bash
docker compose -f docker-compose.yaml -f docker-compose.local.yml up -d --build
```

- **One external port: `8095`** — every service is path-based
  (`/api/auth/...`, `/processes/...`, `/agent/...`, `/dms-api/...`,
  `/oper-api/...`, `/mailpit/` for the Mailpit inbox).
- Configuration is 100% environment: copy `.env.example` to `.env` and set
  at least `OPENAI_BASE_URL` / `OPENAI_MODEL` / `OPENAI_API_KEY`
  (and optionally the `DESIGNER_OPENAI_*` pair for the design chat).
- Postgres is internal to the compose network:
  `docker exec -it oumma-postgres-1 psql -U oumma -d oumma`.

Without docker (unit tests only):

```bash
go build ./... && go test ./...
```

The bundled Keycloak defaults in `docker-compose.yaml` are the **local test
realm** (`brief-test-realm`, clients `briefuserapp`/`oumma-admin`) so the
demo runs out of the box — point `KEYCLOAK_*` at your own realm for real use.

## Branches & pull requests

- Branch off `main`: `feat/<slug>`, `fix/<slug>`, `docs/<slug>`.
- Open a PR back to `main`; **squash-merge** so one logical change = one commit.
- PR description: what changed, why, and **how you verified it**
  (e.g. "verified: POST /processes/x → 202, dry-run log shows payload").
- No secrets in any branch — real credentials live only in `.env`/the environment.

## A note on `workflows/*.yaml`

These are the **seed process definitions** the engine loads — but the
management APIs **rewrite them at runtime** (the volume is mounted rw).
Before committing, `git pull` and re-check `git status` so runtime edits
don't sneak into unrelated PRs.

## Code conventions

- Standard library first; dependencies are deliberate.
- Handlers stay thin: validation at the edge, logic in `internal/`.
- Every LLM-consuming endpoint goes through the quota guard and writes to
  the `llm_logs` ledger.
- Table/schema changes happen in `internal/repository/postgres.go`
  (`CREATE TABLE IF NOT EXISTS` blocks) — keep them additive.


## Contact

All mail goes to **`contact@brief-dz.com`** — put the theme in the subject:

| Subject prefix | Use for |
|---|---|
| `[Security]` | Vulnerability reports (private — see SECURITY.md) |
| `[Conduct]` | Code of Conduct incidents |
| `[Bug]` | Bug details that are hard to file as an issue (or file an issue — preferred) |
| `[Feature]` | Ideas and use cases |
| `[Patch]` | Contribution/PR questions and anything else |

## Licensing

Apache-2.0 (see `LICENSE`). New files should not carry a conflicting license
header; the repo default applies.
