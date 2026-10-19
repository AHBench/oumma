package repository

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// DesignReview is one AI design-review exchange kept in a process's design
// history: the prompt the designer sent and the reviewer's verdict, tagged
// with the deployed design version that was in force when the review ran.
type DesignReview struct {
	ID            string    `json:"id"`
	ProcessID     string    `json:"processId"`
	DesignVersion int       `json:"designVersion"`
	Prompt        string    `json:"prompt"`
	Answer        string    `json:"answer"`
	CreatedAt     time.Time `json:"createdAt"`
}

// DesignReviewStore persists AI design reviews per process (design history).
type DesignReviewStore interface {
	SaveDesignReview(ctx context.Context, r *DesignReview) error
	ListDesignReviews(ctx context.Context, processID string, limit int) ([]*DesignReview, error)
}

// --- PostgresStore implementation ---

func (s *PostgresStore) SaveDesignReview(ctx context.Context, r *DesignReview) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO design_ai_reviews (id, process_id, design_version, prompt, answer, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		r.ID, r.ProcessID, r.DesignVersion, r.Prompt, r.Answer, r.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("save design review: %w", err)
	}
	return nil
}

func (s *PostgresStore) ListDesignReviews(ctx context.Context, processID string, limit int) ([]*DesignReview, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, process_id, design_version, prompt, answer, created_at
		 FROM design_ai_reviews WHERE process_id = $1
		 ORDER BY created_at DESC LIMIT $2`, processID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list design reviews: %w", err)
	}
	defer rows.Close()

	var result []*DesignReview
	for rows.Next() {
		var rv DesignReview
		if err := rows.Scan(&rv.ID, &rv.ProcessID, &rv.DesignVersion, &rv.Prompt, &rv.Answer, &rv.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan design review: %w", err)
		}
		result = append(result, &rv)
	}
	return result, rows.Err()
}

// --- InMemoryStore implementation ---

type memoryDesignReviewStore struct {
	mu      sync.RWMutex
	reviews []*DesignReview
}

func newMemoryDesignReviews() *memoryDesignReviewStore { return &memoryDesignReviewStore{} }

func (s *memoryDesignReviewStore) SaveDesignReview(_ context.Context, r *DesignReview) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reviews = append(s.reviews, r)
	return nil
}

func (s *memoryDesignReviewStore) ListDesignReviews(_ context.Context, processID string, limit int) ([]*DesignReview, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 {
		limit = 20
	}
	var result []*DesignReview
	// newest first
	for i := len(s.reviews) - 1; i >= 0 && len(result) < limit; i-- {
		rv := s.reviews[i]
		if rv.ProcessID == processID {
			result = append(result, rv)
		}
	}
	return result, nil
}

// Compile-time check.
var _ DesignReviewStore = (*PostgresStore)(nil)
