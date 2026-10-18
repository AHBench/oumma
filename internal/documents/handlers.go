package documents

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// apiErr is a status-code + plain-text body error, matching the Java
// service's Response.status(...).entity("...") responses.
type apiErr struct {
	status int
	msg    string
}

func (e *apiErr) Error() string { return e.msg }

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func writeText(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg))
}

func writeErr(w http.ResponseWriter, e *apiErr) { writeText(w, e.status, e.msg) }

func writeInternal(w http.ResponseWriter, err error) {
	writeText(w, http.StatusInternalServerError, err.Error())
}

// randomUUID returns a random RFC 4122 version 4 UUID string.
func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// RegisterRoutes mounts the DMS endpoints on the given mux under prefix
// (e.g. "/dms-api"), mirroring the Java @Path resources /documents and /tags.
func (s *Service) RegisterRoutes(mux *http.ServeMux, prefix string) {
	p := strings.TrimSuffix(prefix, "/")
	mux.HandleFunc("POST "+p+"/documents/upload", s.handleUpload)
	mux.HandleFunc("GET "+p+"/documents", s.handleList)
	mux.HandleFunc("GET "+p+"/documents/search", s.handleSearch)
	mux.HandleFunc("GET "+p+"/documents/{id}", s.handleGet)
	mux.HandleFunc("PUT "+p+"/documents/{id}", s.handleUpdate)
	mux.HandleFunc("DELETE "+p+"/documents/{id}", s.handleDelete)
	mux.HandleFunc("GET "+p+"/documents/{id}/versions", s.handleListVersions)
	mux.HandleFunc("GET "+p+"/documents/{id}/download", s.handleDownload)
	mux.HandleFunc("POST "+p+"/tags", s.handleCreateTag)
	mux.HandleFunc("GET "+p+"/tags/{id}", s.handleGetTag)
	mux.HandleFunc("PUT "+p+"/tags/{id}", s.handleUpdateTag)
	mux.HandleFunc("DELETE "+p+"/tags/{id}", s.handleDeleteTag)
	mux.HandleFunc("GET "+p+"/tags/search", s.handleSearchTags)
	mux.HandleFunc("GET "+p+"/tags/all", s.handleAllTags)

	// Trailing-slash variants: the oumma-app builds URLs like
	// /documents/{id}/, /documents/{id}/download/ and /tags/{id}/.
	mux.HandleFunc("GET "+p+"/documents/{id}/", s.handleGet)
	mux.HandleFunc("PUT "+p+"/documents/{id}/", s.handleUpdate)
	mux.HandleFunc("DELETE "+p+"/documents/{id}/", s.handleDelete)
	mux.HandleFunc("GET "+p+"/documents/{id}/download/", s.handleDownload)

	// Tag collection endpoints used by the oumma-app tag editor:
	// GET /tags/ (and /tags/?search=) → {"results": [...]},
	// POST /tags/ {name, color?}, PATCH /tags/{id}/ {name?, color?}.
	mux.HandleFunc("GET "+p+"/tags/", s.handleTagsCollection)
	mux.HandleFunc("POST "+p+"/tags/", s.handleCreateTag)
	mux.HandleFunc("PATCH "+p+"/tags/{id}/", s.handlePatchTag)
	mux.HandleFunc("DELETE "+p+"/tags/{id}/", s.handleDeleteTag)
}

// groupAccess mirrors the Java group-membership checks. When required is
// true, the caller must pass at least one group; whenever groups are passed
// the caller must belong to at least one of them.
func (s *Service) groupAccess(ctx context.Context, requested []string, required bool) *apiErr {
	_, groups, ok := s.claims(ctx)
	if !ok {
		return &apiErr{http.StatusUnauthorized, "Invalid or expired token"}
	}
	if required && len(requested) == 0 {
		return &apiErr{http.StatusForbidden, "User has no group to do this operation."}
	}
	if len(requested) > 0 && !intersects(groups, requested) {
		return &apiErr{http.StatusForbidden, "User does not belong to required group."}
	}
	return nil
}

func intersects(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

func pathID(r *http.Request) (int64, *apiErr) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, &apiErr{http.StatusNotFound, ""}
	}
	return id, nil
}

// handleUpload mirrors DocumentResource.uploadFile (multipart upload).
func (s *Service) handleUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if err := r.ParseMultipartForm(2 << 20); err != nil {
		writeText(w, http.StatusBadRequest, "Invalid or missing file.")
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeText(w, http.StatusBadRequest, "Invalid or missing file.")
		return
	}
	defer file.Close()

	sizeStr := r.FormValue("size")
	size, err := strconv.ParseInt(sizeStr, 10, 64)
	docType := r.FormValue("type")
	if sizeStr == "" || err != nil || size > MaxUploadSize {
		writeText(w, http.StatusBadRequest, "Invalid or missing file.")
		return
	}
	if !allowedTypes[docType] {
		writeText(w, http.StatusBadRequest, "Unsupported file type.")
		return
	}

	// Group check against the form's groups field.
	if e := s.groupAccess(ctx, r.Form["groups"], true); e != nil {
		writeErr(w, e)
		return
	}

	// Directory structure: uploadDir/{userId}/{organizationId}/{processId}/{processInstanceId}/{uploadDate}
	userID := orDefault(r.FormValue("userId"), "unknown_user")
	org := orDefault(r.FormValue("organizationId"), "unknown_org")
	proc := orDefault(r.FormValue("processId"), "unknown_process")
	inst := orDefault(r.FormValue("processInstanceId"), "unknown_instance")
	dir := filepath.Join(s.uploadDir, userID, org, proc, inst, time.Now().Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeText(w, http.StatusInternalServerError, "Failed to create directory: "+err.Error())
		return
	}

	fileName := r.FormValue("fileName")
	if strings.TrimSpace(fileName) == "" {
		fileName = "uploaded_file"
	}
	uniqueName := fmt.Sprintf("%s-%s.%s", fileName, randomUUID(), docType)
	filePath := filepath.Join(dir, uniqueName)

	out, err := os.Create(filePath)
	if err != nil {
		writeText(w, http.StatusInternalServerError, "File upload failed: "+err.Error())
		return
	}
	if _, err := io.Copy(out, file); err != nil {
		out.Close()
		os.Remove(filePath)
		writeText(w, http.StatusInternalServerError, "File upload failed: "+err.Error())
		return
	}
	_ = out.Close()

	var tagIDs []int64
	for _, t := range r.Form["tags"] {
		if id, err := strconv.ParseInt(t, 10, 64); err == nil {
			tagIDs = append(tagIDs, id)
		}
	}

	// DocumentService.createDocument always stores status ACTIVE.
	doc, err := s.createDocument(ctx, uniqueName, docType, size, "ACTIVE",
		r.FormValue("userId"), r.FormValue("organizationId"), r.FormValue("processId"),
		r.FormValue("processInstanceId"), r.FormValue("description"), filePath, tagIDs)
	if err != nil {
		s.log.Error("document create failed", "error", err)
		writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, doc)
}

// handleGet mirrors DocumentResource.get.
func (s *Service) handleGet(w http.ResponseWriter, r *http.Request) {
	id, e := pathID(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	if e := s.groupAccess(r.Context(), r.URL.Query()["groups"], false); e != nil {
		writeErr(w, e)
		return
	}
	doc, err := s.getDocument(r.Context(), id)
	if err != nil && !errors.Is(err, errNotFound) {
		writeInternal(w, err)
		return
	}
	if doc == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// handleList mirrors DocumentResource.list.
func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := s.groupAccess(r.Context(), q["groups"], true); e != nil {
		writeErr(w, e)
		return
	}
	page, _ := strconv.Atoi(orDefault(q.Get("page"), "0"))
	size, _ := strconv.Atoi(orDefault(q.Get("size"), "10"))
	docs, err := s.listDocuments(r.Context(), q.Get("userId"), page, size)
	if err != nil {
		writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, docs)
}

// handleSearch mirrors DocumentResource.searchDocuments.
func (s *Service) handleSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	if e := s.groupAccess(ctx, q["groups"], false); e != nil {
		writeErr(w, e)
		return
	}

	organizationID := q.Get("organizationId")
	processID := q.Get("processId")
	processInstanceID := q.Get("processInstanceId")
	tags := q["tags"]
	keyword := strings.TrimSpace(q.Get("keyword"))

	if organizationID == "" && processID == "" && processInstanceID == "" &&
		len(nonBlank(tags)) == 0 && keyword == "" {
		writeText(w, http.StatusBadRequest,
			"At least one search criteria (organizationId, processId, processInstanceId, tags or keyword) must be provided.")
		return
	}

	page, _ := strconv.Atoi(orDefault(q.Get("page"), "0"))
	size, _ := strconv.Atoi(orDefault(q.Get("size"), "10"))

	docs, err := s.searchDocuments(ctx, organizationID, processID, processInstanceID,
		tags, keyword, page, size, q.Get("sort"))
	if err != nil {
		writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, docs)
}

func nonBlank(in []string) []string {
	out := []string{}
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// handleUpdate mirrors DocumentResource.update.
func (s *Service) handleUpdate(w http.ResponseWriter, r *http.Request) {
	id, e := pathID(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	if e := s.groupAccess(r.Context(), r.URL.Query()["groups"], false); e != nil {
		writeErr(w, e)
		return
	}
	var dto struct {
		Name              string   `json:"name"`
		Description       string   `json:"description"`
		ProcessInstanceID string   `json:"processInstanceId"`
		TagIDs            []int64  `json:"tagIds"`
		Status            string   `json:"status"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&dto); err != nil {
		writeText(w, http.StatusBadRequest, "Invalid document data.")
		return
	}
	doc, err := s.updateDocument(r.Context(), id, dto.Name, dto.Description,
		dto.ProcessInstanceID, dto.TagIDs, dto.Status)
	if err != nil {
		writeInternal(w, err)
		return
	}
	if doc == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// handleDelete mirrors DocumentResource.delete.
func (s *Service) handleDelete(w http.ResponseWriter, r *http.Request) {
	id, e := pathID(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	if e := s.groupAccess(r.Context(), r.URL.Query()["groups"], true); e != nil {
		writeErr(w, e)
		return
	}
	deleted, err := s.softDeleteDocument(r.Context(), id)
	if err != nil {
		writeInternal(w, err)
		return
	}
	if !deleted {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListVersions mirrors DocumentResource.listVersions.
func (s *Service) handleListVersions(w http.ResponseWriter, r *http.Request) {
	id, e := pathID(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	if e := s.groupAccess(r.Context(), r.URL.Query()["groups"], true); e != nil {
		writeErr(w, e)
		return
	}
	versions, err := s.listVersions(r.Context(), id)
	if err != nil {
		writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, versions)
}

// handleDownload mirrors DocumentResource.downloadFile.
func (s *Service) handleDownload(w http.ResponseWriter, r *http.Request) {
	id, e := pathID(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	doc, err := s.getDocument(r.Context(), id)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeText(w, http.StatusNotFound, "Document not found")
			return
		}
		writeInternal(w, err)
		return
	}
	f, err := os.Open(doc.FilePath)
	if err != nil {
		writeText(w, http.StatusNotFound, "File not found")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Disposition", `attachment; filename="`+doc.Name+`"`)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

// --- Tag handlers (mirror TagResource) ---

func (s *Service) handleCreateTag(w http.ResponseWriter, r *http.Request) {
	var dto struct {
		Name      string `json:"name"`
		Color     string `json:"color"`
		ProcessID string `json:"processId"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&dto); err != nil || strings.TrimSpace(dto.Name) == "" {
		writeText(w, http.StatusBadRequest, "Invalid tag data")
		return
	}
	// color and processId are optional (the oumma-app tag editor sends only
	// name/color); empty defaults keep the NOT NULL columns happy.
	tag, err := s.createTag(r.Context(), dto.Name, dto.Color, dto.ProcessID)
	if err != nil {
		writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, tag)
}

// handleTagsCollection serves GET /tags/ and GET /tags/?search= — the
// oumma-app contract: 200 {"results": [tag, ...]}.
func (s *Service) handleTagsCollection(w http.ResponseWriter, r *http.Request) {
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	var tags []*Tag
	var err error
	if search != "" {
		tags, err = s.tagsNameLike(r.Context(), search)
	} else {
		tags, err = s.allTags(r.Context())
	}
	if err != nil {
		writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": tags})
}

// handlePatchTag serves PATCH /tags/{id}/ with a partial {name?, color?} body.
func (s *Service) handlePatchTag(w http.ResponseWriter, r *http.Request) {
	id, e := pathID(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	var dto struct {
		Name  *string `json:"name"`
		Color *string `json:"color"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&dto); err != nil {
		writeText(w, http.StatusBadRequest, "Invalid tag data")
		return
	}
	tag, err := s.updateTagPartially(r.Context(), id, dto.Name, dto.Color)
	if err != nil {
		writeInternal(w, err)
		return
	}
	if tag == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, tag)
}

func (s *Service) handleGetTag(w http.ResponseWriter, r *http.Request) {
	id, e := pathID(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	tag, err := s.getTag(r.Context(), id)
	if err != nil {
		writeInternal(w, err)
		return
	}
	if tag == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, tag)
}

func (s *Service) handleUpdateTag(w http.ResponseWriter, r *http.Request) {
	id, e := pathID(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	var dto struct {
		Name      string `json:"name"`
		Color     string `json:"color"`
		ProcessID string `json:"processId"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&dto); err != nil ||
		dto.Name == "" || dto.Color == "" || dto.ProcessID == "" {
		writeText(w, http.StatusBadRequest, "Invalid tag data")
		return
	}
	tag, err := s.updateTag(r.Context(), id, dto.Name, dto.Color, dto.ProcessID)
	if err != nil {
		writeInternal(w, err)
		return
	}
	if tag == nil {
		existing, _ := s.getTag(r.Context(), id)
		if existing == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeText(w, http.StatusForbidden, "Process ID does not match the tag's process ID.")
		return
	}
	writeJSON(w, http.StatusOK, tag)
}

func (s *Service) handleDeleteTag(w http.ResponseWriter, r *http.Request) {
	id, e := pathID(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	if _, err := s.getTag(r.Context(), id); err != nil {
		writeInternal(w, err)
		return
	}
	inUse, err := s.tagInUse(r.Context(), id)
	if err != nil {
		writeInternal(w, err)
		return
	}
	if inUse {
		writeText(w, http.StatusConflict, "Tag is used in one or more documents and cannot be deleted.")
		return
	}
	if _, err := s.deleteTag(r.Context(), id); err != nil {
		writeInternal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleSearchTags(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	processID := q.Get("processId")
	if processID == "" {
		writeText(w, http.StatusBadRequest, "Process ID is required.")
		return
	}
	data, err := s.searchTagsByProcessID(r.Context(), processID, q.Get("query"))
	if err != nil {
		writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

func (s *Service) handleAllTags(w http.ResponseWriter, r *http.Request) {
	processID := r.URL.Query().Get("processId")
	if processID == "" {
		writeText(w, http.StatusBadRequest, "Process ID is required.")
		return
	}
	tags, err := s.allTagsByProcessID(r.Context(), processID)
	if err != nil {
		writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tags)
}
