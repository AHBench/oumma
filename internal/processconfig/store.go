package processconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// AppProcess is one workflow YAML file that carries an "app:" section.
type AppProcess struct {
	ID          string // process_id (file stem)
	Path        string // absolute path of the YAML file
	Description string // top-level description (engine section)
	App         *AppConfig
}

// Store is the in-memory, read-mostly registry of app process configs.
// Reads are RLock-guarded; management writes go through Update which takes
// the write lock, rewrites the YAML file atomically and refreshes the cache.
type Store struct {
	mu    sync.RWMutex
	dir   string
	procs map[string]*AppProcess
	mtime map[string]time.Time

	writeMu sync.Mutex // serialises YAML file writes
	log     *slog.Logger

	onChangedMu sync.Mutex
	onChanged   func(processID string) // set via SetOnChanged (pending-version snapshot)

	// versionDoc resolves the stored document of a deployed design version
	// (wired in main.go from the design repository). nil = versioned lookups
	// fall back to the live store.
	versionDocMu sync.RWMutex
	versionDoc   func(processID string, version int) ([]byte, bool)

	// formIDFunc resolves the oper-data form_id for a process step (the id
	// generated when the step's form was saved/updated). Nil = no oper-data
	// wired (in-memory test stores).
	formIDMu   sync.RWMutex
	formIDFunc func(processID, stepID string) string
}

// SetFormIDFunc wires the oper-data form_id lookup so step DTOs expose the
// saved form's identifier (launches must carry it).
func (s *Store) SetFormIDFunc(fn func(processID, stepID string) string) {
	s.formIDMu.Lock()
	s.formIDFunc = fn
	s.formIDMu.Unlock()
}

func (s *Store) formIDFor(processID, stepID string) string {
	s.formIDMu.RLock()
	fn := s.formIDFunc
	s.formIDMu.RUnlock()
	if fn == nil {
		return ""
	}
	return fn(processID, stepID)
}

// SetVersionDocFunc wires the design-version document lookup so that
// permission and form lookups for RUNNING instances use the design version
// the instance was launched with, not the live (latest) document.
func (s *Store) SetVersionDocFunc(fn func(processID string, version int) ([]byte, bool)) {
	s.versionDocMu.Lock()
	s.versionDoc = fn
	s.versionDocMu.Unlock()
}

// appForVersion returns the app config of a deployed design version
// (version > 0), falling back to the live store config (version 0 or the
// version document is unavailable).
func (s *Store) appForVersion(processID string, version int) *AppConfig {
	if version > 0 {
		s.versionDocMu.RLock()
		fn := s.versionDoc
		s.versionDocMu.RUnlock()
		if fn != nil {
			if doc, ok := fn(processID, version); ok && len(doc) > 0 {
				m := map[string]any{}
				if json.Unmarshal(doc, &m) == nil {
					if app, err := appFromMap(m); err == nil && app != nil {
						return app
					}
				}
			}
		}
	}
	if p, ok := s.Get(processID); ok {
		return p.App
	}
	return nil
}

// StepForVersion returns the form step with the given id for a design
// version (version 0 = live store).
func (s *Store) StepForVersion(processID, stepID string, version int) *StepConfig {
	app := s.appForVersion(processID, version)
	if app == nil {
		return nil
	}
	for i := range app.Forms.Steps {
		if app.Forms.Steps[i].StepID == stepID {
			return &app.Forms.Steps[i]
		}
	}
	return nil
}

// SetOnChanged registers a callback fired after every successful design
// write. The design service uses it to refresh the single pending version.
func (s *Store) SetOnChanged(fn func(processID string)) {
	s.onChangedMu.Lock()
	s.onChanged = fn
	s.onChangedMu.Unlock()
}

func (s *Store) notifyChanged(processID string) {
	s.onChangedMu.Lock()
	fn := s.onChanged
	s.onChangedMu.Unlock()
	if fn != nil {
		go fn(processID)
	}
}

// DocumentJSON returns the full process document (engine + app sections)
// as JSON — the unit of a design version snapshot.
func (s *Store) DocumentJSON(processID string) ([]byte, error) {
	s.mu.RLock()
	p, ok := s.procs[processID]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("process %s not found", processID)
	}
	s.mu.RLock()
	path := p.Path
	s.mu.RUnlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", processID, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", processID, err)
	}
	return json.Marshal(doc)
}

// NewStore loads every *.yaml / *.yml file in dir that has an "app:" section.
// Files without one (engine-only workflows) are ignored.
func NewStore(dir string, log *slog.Logger) (*Store, error) {
	s := &Store{dir: dir, procs: map[string]*AppProcess{}, mtime: map[string]time.Time{}, log: log}
	if err := s.Load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Load (re)reads the whole directory.
func (s *Store) Load() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("read processes dir: %w", err)
	}
	next := map[string]*AppProcess{}
	nextM := map[string]time.Time{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		path := filepath.Join(s.dir, name)
		info, err := e.Info()
		if err == nil {
			nextM[path] = info.ModTime()
		}
		data, err := os.ReadFile(path)
		if err != nil {
			s.log.Warn("processconfig: cannot read file", "path", path, "error", err)
			continue
		}
		var raw AppYAML
		if err := yaml.Unmarshal(data, &raw); err != nil {
			// Not our file (e.g. engine-only or malformed) — skip.
			continue
		}
		if raw.App == nil || raw.ProcessID == "" {
			continue
		}
		desc := ""
		var descYAML struct {
			Description string `yaml:"description"`
		}
		_ = yaml.Unmarshal(data, &descYAML)
		desc = descYAML.Description
		next[raw.ProcessID] = &AppProcess{ID: raw.ProcessID, Path: path, Description: desc, App: raw.App}
	}
	s.mu.Lock()
	s.procs = next
	s.mtime = nextM
	s.mu.Unlock()
	s.log.Info("processconfig loaded", "dir", s.dir, "processes", len(next))
	return nil
}

// StartReloadPoll reloads when any YAML file changes on disk.
func (s *Store) StartReloadPoll(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.checkMtimes()
		}
	}
}

func (s *Store) checkMtimes() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	changed := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		path := filepath.Join(s.dir, name)
		info, err := e.Info()
		if err != nil {
			continue
		}
		last, ok := s.mtimeOf(path)
		if !ok || !info.ModTime().Equal(last) {
			changed = true
			break
		}
	}
	if changed {
		if err := s.Load(); err != nil {
			s.log.Warn("processconfig reload failed", "error", err)
		}
	}
}

func (s *Store) mtimeOf(path string) (time.Time, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.mtime[path]
	return m, ok
}

// Get returns the app process with the given id.
func (s *Store) Get(id string) (*AppProcess, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.procs[id]
	return p, ok
}

// DeleteProcess removes the process YAML file and its in-memory entry.
// Only allowed while the project has never been deployed — enforced by
// the caller (design service).
func (s *Store) DeleteProcess(id string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	p, ok := s.Get(id)
	if !ok {
		return fmt.Errorf("process not found: %s", id)
	}
	if err := os.Remove(p.Path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", p.Path, err)
	}
	if err := s.Load(); err != nil {
		return err
	}
	return nil
}

// All returns every app process, sorted by id.
func (s *Store) All() []*AppProcess {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*AppProcess, 0, len(s.procs))
	for _, p := range s.procs {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Has reports whether the store knows a process with the given id.
func (s *Store) Has(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.procs[id]
	return ok
}

// FileContent returns the raw YAML file content of a process.
func (s *Store) FileContent(id string) (string, error) {
	s.mu.RLock()
	p, ok := s.procs[id]
	s.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("process not found: %s", id)
	}
	b, err := os.ReadFile(p.Path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ParseAppFile reports whether the given YAML file carries an "app:" section
// with a process_id (used by the engine loader to skip app-config-only files
// that are not valid engine definitions).
func ParseAppFile(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var raw AppYAML
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return false
	}
	return raw.App != nil && raw.ProcessID != ""
}

// Update mutates the "app:" section of a process YAML file under the write
// lock. fn receives the document as a generic map (engine sections included)
// and returns the changed document. The file is written atomically and the
// in-memory cache is refreshed. If the file does not exist yet, fn receives
// a map pre-populated with process_id.
func (s *Store) Update(processID string, fn func(doc map[string]any) (map[string]any, error)) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	path := filepath.Join(s.dir, processID+".yaml")
	doc := map[string]any{"process_id": processID}
	if data, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	}
	newDoc, err := fn(doc)
	if err != nil {
		return err
	}
	out, err := yaml.Marshal(newDoc)
	if err != nil {
		return fmt.Errorf("marshal yaml: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return fmt.Errorf("write yaml: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename yaml: %w", err)
	}
	if err := s.Load(); err != nil {
		return err
	}
	s.notifyChanged(processID)
	return nil
}

// RenameProcess changes a process' id (file + document + in-memory map).
// Only allowed while the project has never been deployed — enforced by
// the caller (design service).
func (s *Store) RenameProcess(oldID, newID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	old, ok := s.Get(oldID)
	if !ok {
		return fmt.Errorf("process not found: %s", oldID)
	}
	if s.Has(newID) {
		return fmt.Errorf("a process with id %q already exists", newID)
	}
	data, err := os.ReadFile(old.Path)
	if err != nil {
		return fmt.Errorf("read %s: %w", old.Path, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", old.Path, err)
	}
	doc["process_id"] = newID
	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal yaml: %w", err)
	}
	newPath := filepath.Join(s.dir, newID+".yaml")
	tmp := newPath + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return fmt.Errorf("write yaml: %w", err)
	}
	if err := os.Rename(tmp, newPath); err != nil {
		return fmt.Errorf("rename yaml: %w", err)
	}
	if old.Path != newPath {
		if err := os.Remove(old.Path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove old file: %w", err)
		}
	}
	if err := s.Load(); err != nil {
		return err
	}
	s.notifyChanged(newID)
	return nil
}

// appAsMap converts a typed AppConfig to the generic map form used when
// writing YAML documents.
func appAsMap(a *AppConfig) (map[string]any, error) {
	b, err := yaml.Marshal(a)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// appFromMap extracts the "app:" section of a generic YAML document.
func appFromMap(doc map[string]any) (*AppConfig, error) {
	raw, ok := doc["app"]
	if !ok || raw == nil {
		return nil, nil
	}
	b, err := yaml.Marshal(raw)
	if err != nil {
		return nil, err
	}
	a := &AppConfig{}
	if err := yaml.Unmarshal(b, a); err != nil {
		return nil, err
	}
	// task_name is the single task identifier. Legacy configurations carry
	// only task_id — fall back to it once, then drop the legacy field so
	// rewritten files no longer contain it.
	for ri := range a.Roles {
		for ti := range a.Roles[ri].RoleTasks {
			t := &a.Roles[ri].RoleTasks[ti]
			if t.TaskName == "" && t.TaskID != "" {
				t.TaskName = t.TaskID
			}
			t.TaskID = ""
		}
	}
	return a, nil
}
