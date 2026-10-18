package documents

import (
	"context"
	"encoding/json"
	"errors"
)

// In-process LLM tool implementations. These run inside the oumma binary
// (no HTTP hop) and authorize using the caller's claims from the request
// context — the same groups the /ask caller presented.

// LLMListDocuments is the "list_documents" tool body: search documents by
// process / process-instance / keyword, restricted to the caller's groups.
func (s *Service) LLMListDocuments(ctx context.Context, processID, processInstanceID, keyword string) (string, error) {
	_, groups, ok := s.claims(ctx)
	if !ok {
		return "", errors.New("unauthenticated")
	}
	if e := s.groupAccess(ctx, groups, true); e != nil {
		return "", e
	}
	docs, err := s.searchDocuments(ctx, "", processID, processInstanceID, nil, keyword, 0, 25, "uploadDate DESC")
	if err != nil {
		return "", err
	}
	return marshalJSON(docs)
}

// LLMGetDocument is the "get_document" tool body.
func (s *Service) LLMGetDocument(ctx context.Context, id int64) (string, error) {
	_, groups, ok := s.claims(ctx)
	if !ok {
		return "", errors.New("unauthenticated")
	}
	if e := s.groupAccess(ctx, groups, false); e != nil {
		return "", e
	}
	doc, err := s.getDocument(ctx, id)
	if err != nil && !errors.Is(err, errNotFound) {
		return "", err
	}
	if doc == nil {
		return "", errors.New("document not found")
	}
	return marshalJSON(doc)
}

// LLMListTags is the "list_document_tags" tool body.
func (s *Service) LLMListTags(ctx context.Context, processID string) (string, error) {
	if _, _, ok := s.claims(ctx); !ok {
		return "", errors.New("unauthenticated")
	}
	tags, err := s.allTagsByProcessID(ctx, processID)
	if err != nil {
		return "", err
	}
	return marshalJSON(tags)
}

func marshalJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
