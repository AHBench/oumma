package repository

import (
	"gopkg.in/yaml.v3"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"oumma/internal/domain"
)

// ---------------------------------------------------------------------------
// Versioned process designs (task: "save the design to postgres JSON with
// versioning"). A design is the engine section of a process (the parsed
// ProcessDefinition, stored as JSON — the same bytes process_definitions
// uses). Saving a design never mutates the active one; deploying a version
// makes it active for NEW instances. Running instances keep the version
// they started with (ExecutionState.DefinitionVersion pin).
// ---------------------------------------------------------------------------

type DesignVersion struct {
	ProcessID string    `json:"process_id"`
	Version   int       `json:"version"`
	Semver    string    `json:"semver"` // automatic major.minor.patch (deployed versions)
	Status    string    `json:"status"` // pending | active | superseded
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	Design    []byte    `json:"design"` // JSON design document
}

type DesignStore interface {
	// SaveDesign appends a new version and returns its number.
	SaveDesign(ctx context.Context, processID string, design []byte, createdBy string) (int, error)
	// UpsertPendingDesign stores/replaces the single unconfirmed (pending)
	// version of a process. All un-deployed changes collapse into this one
	// version until it is deployed or discarded.
	UpsertPendingDesign(ctx context.Context, processID string, design []byte, createdBy string) error
	// GetPendingDesign returns the pending version (nil, nil when none).
	GetPendingDesign(ctx context.Context, processID string) (*DesignVersion, error)
	// DeletePendingDesign removes the pending version.
	DeletePendingDesign(ctx context.Context, processID string) error
	// SetActiveDesign marks one version active and the rest superseded.
	SetActiveDesign(ctx context.Context, processID string, version int) error
	// SetActiveDesignSemver marks a version active, stamps its automatic
	// semver, and supersedes the rest.
	SetActiveDesignSemver(ctx context.Context, processID string, version int, semver string) error
	// GetActiveDesign returns the active design (nil, 0 when none).
	GetActiveDesign(ctx context.Context, processID string) ([]byte, int, error)
	// GetDesign returns one specific version.
	GetDesign(ctx context.Context, processID string, version int) (*DesignVersion, error)
	// ListDesigns returns all versions of a process, newest first.
	ListDesigns(ctx context.Context, processID string) ([]*DesignVersion, error)
	// ListActiveDesigns returns the active design of every process
	// (used at boot to (re)register definitions ahead of file loading).
	ListActiveDesigns(ctx context.Context) ([]*DesignVersion, error)
}

const designSchema = `
CREATE TABLE IF NOT EXISTS process_designs (
    process_id TEXT NOT NULL,
    version    INTEGER NOT NULL,
    design     JSONB NOT NULL,
    status     TEXT NOT NULL DEFAULT 'saved',
    created_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    semver     TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (process_id, version)
);
ALTER TABLE process_designs ADD COLUMN IF NOT EXISTS semver TEXT NOT NULL DEFAULT '';
`

// --- Postgres implementation ------------------------------------------------

func (s *PostgresStore) initDesignSchema() error {
	_, err := s.db.Exec(designSchema)
	return err
}

func (s *PostgresStore) SaveDesign(ctx context.Context, processID string, design []byte, createdBy string) (int, error) {
	var version int
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO process_designs (process_id, version, design, status, created_by)
		 SELECT $1, COALESCE(MAX(version), 0) + 1, $2, 'saved', $3 FROM process_designs WHERE process_id = $1
		 RETURNING version`,
		processID, design, createdBy,
	).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("save design: %w", err)
	}
	return version, nil
}

func (s *PostgresStore) SetActiveDesign(ctx context.Context, processID string, version int) error {
	return s.SetActiveDesignSemver(ctx, processID, version, "")
}

func (s *PostgresStore) SetActiveDesignSemver(ctx context.Context, processID string, version int, semver string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE process_designs
		 SET status = CASE WHEN version = $2 THEN 'active' ELSE 'superseded' END,
		     semver = CASE WHEN version = $2 THEN $3 ELSE semver END
		 WHERE process_id = $1`,
		processID, version, semver)
	if err != nil {
		return fmt.Errorf("set active design: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("design version %d not found for %s", version, processID)
	}
	return nil
}

func (s *PostgresStore) UpsertPendingDesign(ctx context.Context, processID string, design []byte, createdBy string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin pending design: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM process_designs WHERE process_id = $1 AND status = 'pending'`, processID); err != nil {
		return fmt.Errorf("delete pending design: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO process_designs (process_id, version, design, status, created_by)
		 SELECT $1, COALESCE(MAX(version), 0) + 1, $2, 'pending', $3 FROM process_designs WHERE process_id = $1`,
		processID, design, createdBy); err != nil {
		return fmt.Errorf("insert pending design: %w", err)
	}
	return tx.Commit()
}

func (s *PostgresStore) GetPendingDesign(ctx context.Context, processID string) (*DesignVersion, error) {
	var dv DesignVersion
	err := s.db.QueryRowContext(ctx,
		`SELECT process_id, version, status, created_by, created_at, semver, design
		 FROM process_designs WHERE process_id = $1 AND status = 'pending'
		 ORDER BY version DESC LIMIT 1`,
		processID).Scan(&dv.ProcessID, &dv.Version, &dv.Status, &dv.CreatedBy, &dv.CreatedAt, &dv.Semver, &dv.Design)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get pending design: %w", err)
	}
	return &dv, nil
}

func (s *PostgresStore) DeletePendingDesign(ctx context.Context, processID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM process_designs WHERE process_id = $1 AND status = 'pending'`, processID)
	if err != nil {
		return fmt.Errorf("delete pending design: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetActiveDesign(ctx context.Context, processID string) ([]byte, int, error) {
	var design []byte
	var version int
	err := s.db.QueryRowContext(ctx,
		`SELECT design, version FROM process_designs WHERE process_id = $1 AND status = 'active'
		 ORDER BY version DESC LIMIT 1`,
		processID).Scan(&design, &version)
	if err == sql.ErrNoRows {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("get active design: %w", err)
	}
	return design, version, nil
}

func (s *PostgresStore) GetDesign(ctx context.Context, processID string, version int) (*DesignVersion, error) {
	var dv DesignVersion
	err := s.db.QueryRowContext(ctx,
		`SELECT process_id, version, status, created_by, created_at, semver, design
		 FROM process_designs WHERE process_id = $1 AND version = $2`,
		processID, version).Scan(&dv.ProcessID, &dv.Version, &dv.Status, &dv.CreatedBy, &dv.CreatedAt, &dv.Semver, &dv.Design)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("design version %d not found for %s", version, processID)
	}
	if err != nil {
		return nil, fmt.Errorf("get design: %w", err)
	}
	return &dv, nil
}

func (s *PostgresStore) ListDesigns(ctx context.Context, processID string) ([]*DesignVersion, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT process_id, version, status, created_by, created_at, semver, design
		 FROM process_designs WHERE process_id = $1 ORDER BY version DESC`,
		processID)
	if err != nil {
		return nil, fmt.Errorf("list designs: %w", err)
	}
	defer rows.Close()
	return scanDesignRows(rows)
}

func (s *PostgresStore) ListActiveDesigns(ctx context.Context) ([]*DesignVersion, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT process_id, version, status, created_by, created_at, semver, design
		 FROM process_designs WHERE status = 'active'`)
	if err != nil {
		return nil, fmt.Errorf("list active designs: %w", err)
	}
	defer rows.Close()
	return scanDesignRows(rows)
}

func scanDesignRows(rows *sql.Rows) ([]*DesignVersion, error) {
	out := make([]*DesignVersion, 0)
	for rows.Next() {
		var dv DesignVersion
		if err := rows.Scan(&dv.ProcessID, &dv.Version, &dv.Status, &dv.CreatedBy, &dv.CreatedAt, &dv.Semver, &dv.Design); err != nil {
			return nil, fmt.Errorf("scan design row: %w", err)
		}
		out = append(out, &dv)
	}
	return out, rows.Err()
}

// --- In-memory implementation ------------------------------------------------

type memoryDesigns struct {
	mu   sync.RWMutex
	vers map[string]map[int]*DesignVersion
}

var _ DesignStore = (*InMemoryStore)(nil)

func (s *InMemoryStore) designInit() {
	if s.designs == nil {
		s.designs = &memoryDesigns{vers: map[string]map[int]*DesignVersion{}}
	}
}

func (s *InMemoryStore) SaveDesign(_ context.Context, processID string, design []byte, createdBy string) (int, error) {
	s.designInit()
	s.designs.mu.Lock()
	defer s.designs.mu.Unlock()
	m := s.designs.vers[processID]
	if m == nil {
		m = map[int]*DesignVersion{}
		s.designs.vers[processID] = m
	}
	next := 1
	for v := range m {
		if v >= next {
			next = v + 1
		}
	}
	dv := &DesignVersion{
		ProcessID: processID,
		Version:   next,
		Status:    "saved",
		CreatedBy: createdBy,
		CreatedAt: time.Now().UTC(),
		Design:    append([]byte(nil), design...),
	}
	m[next] = dv
	return next, nil
}

func (s *InMemoryStore) SetActiveDesign(_ context.Context, processID string, version int) error {
	return s.SetActiveDesignSemver(context.Background(), processID, version, "")
}

func (s *InMemoryStore) SetActiveDesignSemver(_ context.Context, processID string, version int, semver string) error {
	s.designInit()
	s.designs.mu.Lock()
	defer s.designs.mu.Unlock()
	m := s.designs.vers[processID]
	if m == nil || m[version] == nil {
		return fmt.Errorf("design version %d not found for %s", version, processID)
	}
	for v, dv := range m {
		if v == version {
			dv.Status = "active"
			dv.Semver = semver
		} else {
			dv.Status = "superseded"
		}
	}
	return nil
}

func (s *InMemoryStore) UpsertPendingDesign(_ context.Context, processID string, design []byte, createdBy string) error {
	s.designInit()
	s.designs.mu.Lock()
	defer s.designs.mu.Unlock()
	m := s.designs.vers[processID]
	if m == nil {
		m = map[int]*DesignVersion{}
		s.designs.vers[processID] = m
	}
	for v, dv := range m {
		if dv.Status == "pending" {
			delete(m, v)
		}
	}
	next := 1
	for v := range m {
		if v >= next {
			next = v + 1
		}
	}
	m[next] = &DesignVersion{
		ProcessID: processID,
		Version:   next,
		Status:    "pending",
		CreatedBy: createdBy,
		CreatedAt: time.Now().UTC(),
		Design:    append([]byte(nil), design...),
	}
	return nil
}

func (s *InMemoryStore) GetPendingDesign(_ context.Context, processID string) (*DesignVersion, error) {
	s.designInit()
	s.designs.mu.RLock()
	defer s.designs.mu.RUnlock()
	var best *DesignVersion
	for _, dv := range s.designs.vers[processID] {
		if dv.Status == "pending" && (best == nil || dv.Version > best.Version) {
			best = dv
		}
	}
	return best, nil
}

func (s *InMemoryStore) DeletePendingDesign(_ context.Context, processID string) error {
	s.designInit()
	s.designs.mu.Lock()
	defer s.designs.mu.Unlock()
	m := s.designs.vers[processID]
	for v, dv := range m {
		if dv.Status == "pending" {
			delete(m, v)
		}
	}
	return nil
}

func (s *InMemoryStore) GetActiveDesign(_ context.Context, processID string) ([]byte, int, error) {
	s.designInit()
	s.designs.mu.RLock()
	defer s.designs.mu.RUnlock()
	for _, dv := range s.designs.vers[processID] {
		if dv.Status == "active" {
			return dv.Design, dv.Version, nil
		}
	}
	return nil, 0, nil
}

func (s *InMemoryStore) GetDesign(_ context.Context, processID string, version int) (*DesignVersion, error) {
	s.designInit()
	s.designs.mu.RLock()
	defer s.designs.mu.RUnlock()
	if dv := s.designs.vers[processID][version]; dv != nil {
		return dv, nil
	}
	return nil, fmt.Errorf("design version %d not found for %s", version, processID)
}

func (s *InMemoryStore) ListDesigns(_ context.Context, processID string) ([]*DesignVersion, error) {
	s.designInit()
	s.designs.mu.RLock()
	defer s.designs.mu.RUnlock()
	out := make([]*DesignVersion, 0)
	for _, dv := range s.designs.vers[processID] {
		out = append(out, dv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version > out[j].Version })
	return out, nil
}

func (s *InMemoryStore) ListActiveDesigns(_ context.Context) ([]*DesignVersion, error) {
	s.designInit()
	s.designs.mu.RLock()
	defer s.designs.mu.RUnlock()
	out := make([]*DesignVersion, 0)
	for _, m := range s.designs.vers {
		for _, dv := range m {
			if dv.Status == "active" {
				out = append(out, dv)
			}
		}
	}
	return out, nil
}

// DesignOf parses a design payload into a ProcessDefinition.
// DesignOf parses a stored design document into a validated
// ProcessDefinition. Stored documents are JSON serializations of the
// process definition (with an "app" section alongside the engine keys,
// tasks as a map keyed by task name) — the shape produced when a live
// definition is persisted — so the primary path unmarshals the definition
// directly. A JSON→YAML fallback covers legacy documents in the file shape.
func DesignOf(design []byte) (*domain.ProcessDefinition, error) {
	var def domain.ProcessDefinition
	if err := json.Unmarshal(design, &def); err == nil && len(def.Tasks) > 0 {
		// Full process documents (engine + app sections) are keyed
		// "process_id" (the YAML form), while JSON design snapshots use
		// "id" — accept both.
		if def.ID == "" {
			var probe map[string]any
			if perr := json.Unmarshal(design, &probe); perr == nil {
				if pid, _ := probe["process_id"].(string); pid != "" {
					def.ID = pid
				} else if pid, _ := probe["id"].(string); pid != "" {
					def.ID = pid
				}
			}
		}
		if def.ID != "" {
			if def.StartTaskID == "" || def.Tasks[def.StartTaskID] == nil {
				return nil, fmt.Errorf("design %q: start task %q not found", def.ID, def.StartTaskID)
			}
			def.RebuildTaskList()
			return &def, nil
		}
	}

	var probe map[string]any
	if err := json.Unmarshal(design, &probe); err != nil {
		return nil, fmt.Errorf("parse design: %w", err)
	}
	if len(probe) == 0 {
		return nil, fmt.Errorf("design is empty")
	}
	id, _ := probe["process_id"].(string)
	if id == "" {
		// Stored design documents are keyed by "id" (the file's top-level key);
		// both forms are accepted.
		id, _ = probe["id"].(string)
	}
	if id == "" {
		return nil, fmt.Errorf("design is missing process_id")
	}
	// Convert JSON to YAML and run the same validated parser the workflow
	// files use (tasks map rebuild, topology validation, schemas).
	yb, err := yaml.Marshal(probe)
	if err != nil {
		return nil, fmt.Errorf("design to yaml: %w", err)
	}
	fdef, err := domain.ParseYAML(yb)
	if err != nil {
		return nil, fmt.Errorf("parse design: %w", err)
	}
	return fdef, nil
}
