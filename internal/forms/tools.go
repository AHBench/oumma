package forms

import (
	"context"
	"encoding/json"
	"errors"
)

// In-process LLM tool implementations (run inside the oumma binary, no
// HTTP hop). Authorization comes from the caller's claims in ctx.

// LLMListForms is the "list_forms" tool body: list saved/active forms.
func (s *Service) LLMListForms(ctx context.Context, active bool) (string, error) {
	if _, _, ok := s.claims(ctx); !ok {
		return "", errors.New("unauthenticated")
	}
	forms, err := s.userForms(ctx, "", "", "", "", boolPtr(active), nil)
	if err != nil {
		return "", err
	}
	sanitizeProcessForms(forms)
	return marshalJSON(forms)
}

// LLMSearchForms is the "search_forms" tool body: keyword search over saved
// forms (Java default: activeform unset → only forms attached to instances).
func (s *Service) LLMSearchForms(ctx context.Context, query string) (string, error) {
	if _, _, ok := s.claims(ctx); !ok {
		return "", errors.New("unauthenticated")
	}
	forms, e := s.searchSavedForms(ctx, query, true, "", "", "", "", 0, nil, nil, nil)
	if e != nil {
		return "", e
	}
	sanitizeProcessForms(forms)
	return marshalJSON(forms)
}

func marshalJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
