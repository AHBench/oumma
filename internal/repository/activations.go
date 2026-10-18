package repository

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Email activation tokens (user registration → activation link →
// Keycloak email confirmation). A token maps to the Keycloak user id it
// was minted for; it is single-use and expires (24h by default).
// ---------------------------------------------------------------------------

type EmailActivation struct {
	Token         string     `json:"token"`
	KeycloakUserID string    `json:"keycloak_user_id"`
	Username      string     `json:"username"`
	Email         string     `json:"email"`
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	UsedAt        *time.Time `json:"used_at,omitempty"`
}

// ActivationStore persists email activation tokens.
type ActivationStore interface {
	CreateActivation(ctx context.Context, a EmailActivation) error
	// GetActivation returns the token record, or sql.ErrNoRows when absent.
	GetActivation(ctx context.Context, token string) (*EmailActivation, error)
	MarkActivationUsed(ctx context.Context, token string) error
}

// --- PostgreSQL -----------------------------------------------------------

func (s *PostgresStore) CreateActivation(ctx context.Context, a EmailActivation) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO email_activations (token, user_id, username, email, expires_at)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (token) DO NOTHING`,
		a.Token, a.KeycloakUserID, a.Username, a.Email, a.ExpiresAt)
	if err != nil {
		return fmt.Errorf("create activation: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetActivation(ctx context.Context, token string) (*EmailActivation, error) {
	var a EmailActivation
	var usedAt sql.NullTime
	err := s.db.QueryRowContext(ctx,
		`SELECT token, user_id, username, email, created_at, expires_at, used_at
		 FROM email_activations WHERE token = $1`, token).
		Scan(&a.Token, &a.KeycloakUserID, &a.Username, &a.Email, &a.CreatedAt, &a.ExpiresAt, &usedAt)
	if err != nil {
		return nil, err
	}
	if usedAt.Valid {
		t := usedAt.Time
		a.UsedAt = &t
	}
	return &a, nil
}

func (s *PostgresStore) MarkActivationUsed(ctx context.Context, token string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE email_activations SET used_at = NOW() WHERE token = $1 AND used_at IS NULL`, token)
	if err != nil {
		return fmt.Errorf("mark activation used: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("activation token not found or already used")
	}
	return nil
}

// --- In-memory --------------------------------------------------------------

type memoryActivations struct {
	mu       sync.RWMutex
	byToken  map[string]EmailActivation
}

func newMemoryActivations() *memoryActivations {
	return &memoryActivations{byToken: map[string]EmailActivation{}}
}

func (m *memoryActivations) CreateActivation(_ context.Context, a EmailActivation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byToken[a.Token] = a
	return nil
}

func (m *memoryActivations) GetActivation(_ context.Context, token string) (*EmailActivation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.byToken[token]
	if !ok {
		return nil, sql.ErrNoRows
	}
	cp := a
	return &cp, nil
}

func (m *memoryActivations) MarkActivationUsed(_ context.Context, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.byToken[token]
	if !ok || a.UsedAt != nil {
		return fmt.Errorf("activation token not found or already used")
	}
	now := time.Now().UTC()
	a.UsedAt = &now
	m.byToken[token] = a
	return nil
}

// Global in-memory activation store (used when DATABASE_URL is unset —
// local development). Sorted access is not required; the map is guarded.
var sharedMemoryActivations = newMemoryActivations()

func (s *InMemoryStore) CreateActivation(ctx context.Context, a EmailActivation) error {
	return sharedMemoryActivations.CreateActivation(ctx, a)
}
func (s *InMemoryStore) GetActivation(ctx context.Context, token string) (*EmailActivation, error) {
	return sharedMemoryActivations.GetActivation(ctx, token)
}
func (s *InMemoryStore) MarkActivationUsed(ctx context.Context, token string) error {
	return sharedMemoryActivations.MarkActivationUsed(ctx, token)
}
