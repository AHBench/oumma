// Package documents is the Go port of the brief-dms-docservice
// (Quarkus/Java) microservice. It is an integral part of the oumma binary:
// same process, same HTTP mux (under /dms-api), same Postgres database.
package documents

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strconv"
	"strings"
)

// MaxUploadSize is the 10 MB limit enforced by the Java service.
const MaxUploadSize = 10485760

var allowedTypes = map[string]bool{
	"pdf": true, "docx": true, "xlsx": true, "txt": true, "jpg": true, "png": true,
}

// ClaimsFunc returns the authenticated caller's username and token groups.
// It is wired to the oumma auth middleware claims in cmd/main.go and
// replaces the Java AuthService.validateToken calls.
type ClaimsFunc func(ctx context.Context) (username string, groups []string, ok bool)

// Service is the DMS document + tag store.
type Service struct {
	db        *sql.DB
	uploadDir string
	claims    ClaimsFunc
	log       *slog.Logger
}

// New creates a DMS service bound to the shared database.
func New(db *sql.DB, uploadDir string, claims ClaimsFunc, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	if claims == nil {
		claims = func(context.Context) (string, []string, bool) { return "", nil, false }
	}
	return &Service{db: db, uploadDir: uploadDir, claims: claims, log: log}
}

var errNotFound = errors.New("not found")

func strPtr(s string, emptyIsNull bool) *string {
	if s == "" && emptyIsNull {
		return nil
	}
	v := s
	return &v
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// --- Documents ---

// createDocument mirrors DocumentService.createDocument.
func (s *Service) createDocument(ctx context.Context, name, docType string, size int64, status string,
	userID, organizationID, processID, processInstanceID, description, filePath string, tagIDs []int64) (*Document, error) {
	if size > MaxUploadSize {
		return nil, errors.New("File size exceeds the maximum allowed limit of 10 MB")
	}
	if !allowedTypes[docType] {
		return nil, errors.New("Unsupported file type: " + docType)
	}

	var id int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO dms_documents
			(name, type, size, upload_date, status, user_id, organization_id,
			 process_id, process_instance_id, description, file_path)
		 VALUES ($1, $2, $3, NOW(), $4, $5, $6, $7, $8, $9, $10)
		 RETURNING id`,
		name, docType, size, status, userID,
		strPtr(organizationID, true), strPtr(processID, true),
		strPtr(processInstanceID, true), strPtr(description, true), filePath,
	).Scan(&id)
	if err != nil {
		return nil, err
	}
	if len(tagIDs) > 0 {
		if err := s.setDocumentTags(ctx, id, tagIDs); err != nil {
			return nil, err
		}
	}
	return s.getDocument(ctx, id)
}

func (s *Service) getDocument(ctx context.Context, id int64) (*Document, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, type, size, upload_date, status, user_id, organization_id,
		        process_id, process_instance_id, description, file_path
		   FROM dms_documents WHERE id = $1`, id)
	var d Document
	err := row.Scan(&d.ID, &d.Name, &d.Type, &d.Size, &d.UploadDate, &d.Status,
		&d.UserID, &d.OrganizationID, &d.ProcessID, &d.ProcessInstanceID,
		&d.Description, &d.FilePath)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	if d.Status == "DELETED" {
		return nil, errNotFound
	}
	tagMap, err := s.tagsForDocuments(ctx, []int64{d.ID})
	if err != nil {
		return nil, err
	}
	d.Tags = tagMap[d.ID]
	if d.Tags == nil {
		d.Tags = []Tag{}
	}
	return &d, nil
}

// listDocuments mirrors DocumentService.listDocuments: filters by userId and
// status != 'DELETED' with pagination (the Java query also ignores the
// tag/status/date params in practice).
func (s *Service) listDocuments(ctx context.Context, userID string, page, size int) ([]*Document, error) {
	if userID == "" {
		return []*Document{}, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM dms_documents
		  WHERE user_id = $1 AND status <> 'DELETED'
		  ORDER BY id
		  LIMIT $2 OFFSET $3`, userID, size, page*size)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []*Document{}, nil
	}
	return s.documentsByIDs(ctx, ids)
}

// updateDocument mirrors DocumentService.updateDocument.
func (s *Service) updateDocument(ctx context.Context, id int64, name, description, processInstanceID string, tagIDs []int64, status string) (*Document, error) {
	if _, err := s.getDocument(ctx, id); err != nil {
		if errors.Is(err, errNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE dms_documents
		    SET name = $2, description = $3, process_instance_id = $4, status = $5
		  WHERE id = $1`,
		id, name, strPtr(description, true), strPtr(processInstanceID, true), status); err != nil {
		return nil, err
	}
	if err := s.setDocumentTags(ctx, id, tagIDs); err != nil {
		return nil, err
	}
	return s.getDocument(ctx, id)
}

// softDeleteDocument mirrors DocumentService.softDeleteDocument.
func (s *Service) softDeleteDocument(ctx context.Context, id int64) (bool, error) {
	if _, err := s.getDocument(ctx, id); err != nil {
		if errors.Is(err, errNotFound) {
			return false, nil
		}
		return false, err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE dms_documents SET status = 'DELETED' WHERE id = $1 AND status <> 'DELETED'`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// listVersions mirrors DocumentService.listVersions.
func (s *Service) listVersions(ctx context.Context, documentID int64) ([]*Version, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, version_number, upload_date, size, type, file_storage_id
		   FROM dms_document_versions WHERE document_id = $1 ORDER BY version_number`, documentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Version{}
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.ID, &v.VersionNumber, &v.UploadDate, &v.Size, &v.Type, &v.FileStorageID); err != nil {
			return nil, err
		}
		out = append(out, &v)
	}
	return out, rows.Err()
}

// searchDocuments mirrors DocumentService.searchDocuments.
func (s *Service) searchDocuments(ctx context.Context, organizationID, processID, processInstanceID string,
	tags []string, keyword string, page, size int, sort string) ([]*Document, error) {

	// Sanitize inputs: trim keyword, drop blank tags (same as Java).
	keyword = strings.TrimSpace(keyword)
	cleanTags := make([]string, 0, len(tags))
	for _, t := range tags {
		if t = strings.TrimSpace(t); t != "" {
			cleanTags = append(cleanTags, t)
		}
	}

	needTagJoin := len(cleanTags) > 0 || keyword != ""

	where := []string{"d.status <> 'DELETED'"}
	args := []any{}
	addCond := func(cond string, a ...any) {
		where = append(where, cond)
		args = append(args, a...)
	}
	if organizationID != "" {
		addCond("d.organization_id = $"+strconv.Itoa(len(args)+1), organizationID)
	}
	if processID != "" {
		addCond("d.process_id = $"+strconv.Itoa(len(args)+1), processID)
	}
	if processInstanceID != "" {
		addCond("d.process_instance_id = $"+strconv.Itoa(len(args)+1), processInstanceID)
	}

	if needTagJoin {
		match := []string{}
		if len(cleanTags) > 0 {
			placeholders := make([]string, len(cleanTags))
			for i, t := range cleanTags {
				placeholders[i] = "$" + strconv.Itoa(len(args)+1+i)
				args = append(args, t)
			}
			match = append(match, "t.name IN ("+strings.Join(placeholders, ",")+")")
		}
		if keyword != "" {
			kw := "%" + strings.ToLower(keyword) + "%"
			match = append(match, "LOWER(COALESCE(d.description, '')) LIKE $"+strconv.Itoa(len(args)+1))
			args = append(args, kw)
			match = append(match, "LOWER(COALESCE(t.name, '')) LIKE $"+strconv.Itoa(len(args)+1))
			args = append(args, kw)
		}
		where = append(where, "("+strings.Join(match, " OR ")+")")
	}

	from := "FROM dms_documents d"
	if needTagJoin {
		from += ` LEFT JOIN dms_document_tags dt ON dt.document_id = d.id
		         LEFT JOIN dms_tags t ON t.id = dt.tag_id`
	}

	order := sortClause(sort)

	q := `SELECT DISTINCT d.id, d.name, d.type, d.size, d.upload_date, d.status,
		              d.user_id, d.organization_id, d.process_id,
		              d.process_instance_id, d.description, d.file_path
		   ` + from + ` WHERE ` + strings.Join(where, " AND ") + `
		   ` + order + ` LIMIT $` + strconv.Itoa(len(args)+1) + ` OFFSET $` + strconv.Itoa(len(args)+2)
	args = append(args, size, page*size)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Document{}
	var ids []int64
	for rows.Next() {
		var d Document
		if err := rows.Scan(&d.ID, &d.Name, &d.Type, &d.Size, &d.UploadDate, &d.Status,
			&d.UserID, &d.OrganizationID, &d.ProcessID, &d.ProcessInstanceID,
			&d.Description, &d.FilePath); err != nil {
			return nil, err
		}
		out = append(out, &d)
		ids = append(ids, d.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return []*Document{}, nil
	}
	tagMap, err := s.tagsForDocuments(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, d := range out {
		if t := tagMap[d.ID]; len(t) > 0 {
			d.Tags = t
		} else {
			d.Tags = []Tag{}
		}
	}
	return out, nil
}

// sortClause maps a Java-style sort expression (entity field, e.g.
// "uploadDate DESC") to a safe SQL ORDER BY clause.
func sortClause(sort string) string {
	const def = "ORDER BY d.upload_date DESC"
	sort = strings.TrimSpace(sort)
	if sort == "" {
		return def
	}
	parts := strings.Fields(sort)
	col := mapCamelToColumn(parts[0])
	if col == "" {
		return def
	}
	dir := "ASC"
	if len(parts) > 1 && strings.EqualFold(parts[1], "desc") {
		dir = "DESC"
	}
	return "ORDER BY d." + col + " " + dir
}

func mapCamelToColumn(f string) string {
	switch f {
	case "id", "name", "type", "size", "status":
		return f
	case "uploadDate":
		return "upload_date"
	case "userId":
		return "user_id"
	case "organizationId":
		return "organization_id"
	case "processId":
		return "process_id"
	case "processInstanceId":
		return "process_instance_id"
	case "description", "filePath":
		if f == "filePath" {
			return "file_path"
		}
		return f
	}
	return ""
}

func (s *Service) documentsByIDs(ctx context.Context, ids []int64) ([]*Document, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, type, size, upload_date, status, user_id, organization_id,
		        process_id, process_instance_id, description, file_path
		   FROM dms_documents WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Document, 0, len(ids))
	for rows.Next() {
		var d Document
		if err := rows.Scan(&d.ID, &d.Name, &d.Type, &d.Size, &d.UploadDate, &d.Status,
			&d.UserID, &d.OrganizationID, &d.ProcessID, &d.ProcessInstanceID,
			&d.Description, &d.FilePath); err != nil {
			return nil, err
		}
		out = append(out, &d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	tags, err := s.tagsForDocuments(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, d := range out {
		if t := tags[d.ID]; len(t) > 0 {
			d.Tags = t
		} else {
			d.Tags = []Tag{}
		}
	}
	return out, nil
}

func (s *Service) tagsForDocuments(ctx context.Context, docIDs []int64) (map[int64][]Tag, error) {
	out := map[int64][]Tag{}
	if len(docIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT dt.document_id, t.id, t.name, t.color
		   FROM dms_document_tags dt JOIN dms_tags t ON t.id = dt.tag_id
		  WHERE dt.document_id = ANY($1)
		  ORDER BY t.id`, docIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var docID int64
		var tg Tag
		if err := rows.Scan(&docID, &tg.ID, &tg.Name, &tg.Color); err != nil {
			return nil, err
		}
		out[docID] = append(out[docID], tg)
	}
	return out, rows.Err()
}

func (s *Service) setDocumentTags(ctx context.Context, docID int64, tagIDs []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM dms_document_tags WHERE document_id = $1`, docID); err != nil {
		return err
	}
	seen := map[int64]bool{}
	for _, id := range tagIDs {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO dms_document_tags (document_id, tag_id) SELECT $1, $2 FROM dms_tags WHERE id = $2`,
			docID, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// --- Tags ---

// createTag mirrors TagService.createTag.
func (s *Service) createTag(ctx context.Context, name, color, processID string) (*Tag, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO dms_tags (name, color, process_id) VALUES ($1, $2, $3) RETURNING id`,
		name, color, processID).Scan(&id)
	if err != nil {
		return nil, err
	}
	return &Tag{ID: id, Name: name, Color: color}, nil
}

func (s *Service) getTag(ctx context.Context, id int64) (*Tag, error) {
	var tg Tag
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, color FROM dms_tags WHERE id = $1`, id).
		Scan(&tg.ID, &tg.Name, &tg.Color)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &tg, nil
}

// getTagProcessID returns the tag's processId ("" when not found).
func (s *Service) getTagProcessID(ctx context.Context, id int64) (string, error) {
	var pid string
	err := s.db.QueryRowContext(ctx, `SELECT process_id FROM dms_tags WHERE id = $1`, id).Scan(&pid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return pid, err
}

// updateTag mirrors TagService.updateTag: only when processId matches.
func (s *Service) updateTag(ctx context.Context, id int64, name, color, processID string) (*Tag, error) {
	pid, err := s.getTagProcessID(ctx, id)
	if err != nil {
		return nil, err
	}
	if pid == "" || pid != processID {
		return nil, nil
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE dms_tags SET name = $2, color = $3 WHERE id = $1`, id, name, color); err != nil {
		return nil, err
	}
	return s.getTag(ctx, id)
}

// updateTagPartially is the PATCH /tags/{id} variant: updates only the
// provided fields (the oumma-app tag editor contract).
func (s *Service) updateTagPartially(ctx context.Context, id int64, name, color *string) (*Tag, error) {
	tag, err := s.getTag(ctx, id)
	if err != nil || tag == nil {
		return tag, err
	}
	newName, newColor := tag.Name, tag.Color
	if name != nil {
		newName = *name
	}
	if color != nil {
		newColor = *color
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE dms_tags SET name = $2, color = $3 WHERE id = $1`, id, newName, newColor); err != nil {
		return nil, err
	}
	return s.getTag(ctx, id)
}

// allTags returns every tag (GET /tags/ contract).
func (s *Service) allTags(ctx context.Context) ([]*Tag, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, color FROM dms_tags ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Tag{}
	for rows.Next() {
		var tg Tag
		if err := rows.Scan(&tg.ID, &tg.Name, &tg.Color); err != nil {
			return nil, err
		}
		out = append(out, &tg)
	}
	return out, rows.Err()
}

// tagsNameLike returns tags whose name contains query, case-insensitively.
func (s *Service) tagsNameLike(ctx context.Context, query string) ([]*Tag, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, color FROM dms_tags WHERE lower(name) LIKE $1 ORDER BY id`,
		"%"+strings.ToLower(query)+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Tag{}
	for rows.Next() {
		var tg Tag
		if err := rows.Scan(&tg.ID, &tg.Name, &tg.Color); err != nil {
			return nil, err
		}
		out = append(out, &tg)
	}
	return out, rows.Err()
}

// tagInUse reports whether any (non-deleted-agnostic, like Java) document
// references the tag.
func (s *Service) tagInUse(ctx context.Context, id int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM dms_document_tags WHERE tag_id = $1`, id).Scan(&n)
	return n > 0, err
}

// deleteTag mirrors TagService.deleteTag.
func (s *Service) deleteTag(ctx context.Context, id int64) (bool, error) {
	if _, err := s.getTag(ctx, id); err != nil {
		return false, err
	}
	inUse, err := s.tagInUse(ctx, id)
	if err != nil || inUse {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM dms_tags WHERE id = $1`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// searchTagsByProcessId mirrors TagService.searchTagsByProcessId.
func (s *Service) searchTagsByProcessID(ctx context.Context, processID, query string) ([]*ProcessTags, error) {
	q := `SELECT id, name, color, process_id FROM dms_tags WHERE process_id = $1`
	args := []any{processID}
	if strings.TrimSpace(query) != "" {
		q += ` AND lower(name) LIKE $2`
		args = append(args, "%"+strings.ToLower(strings.TrimSpace(query))+"%")
	}
	q += ` ORDER BY id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byProcess := map[string]*ProcessTags{}
	var order []string
	for rows.Next() {
		var tg Tag
		var pid string
		if err := rows.Scan(&tg.ID, &tg.Name, &tg.Color, &pid); err != nil {
			return nil, err
		}
		pt := byProcess[pid]
		if pt == nil {
			pt = &ProcessTags{ProcessID: pid, Tags: []Tag{}}
			byProcess[pid] = pt
			order = append(order, pid)
		}
		pt.Tags = append(pt.Tags, tg)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]*ProcessTags, 0, len(order))
	for _, pid := range order {
		out = append(out, byProcess[pid])
	}
	return out, nil
}

// allTagsByProcessID mirrors TagService.getAllTagsByProcessId.
func (s *Service) allTagsByProcessID(ctx context.Context, processID string) ([]*Tag, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, color FROM dms_tags WHERE process_id = $1 ORDER BY id`, processID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Tag{}
	for rows.Next() {
		var tg Tag
		if err := rows.Scan(&tg.ID, &tg.Name, &tg.Color); err != nil {
			return nil, err
		}
		out = append(out, &tg)
	}
	return out, rows.Err()
}
