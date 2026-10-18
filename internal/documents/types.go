package documents

import "time"

// Tag mirrors the Java TagDTO.
type Tag struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

// Document mirrors the Java DocumentResponseDTO (JSON field names included).
type Document struct {
	ID                int64     `json:"id"`
	Name              string    `json:"name"`
	Type              string    `json:"type"`
	Size              int64     `json:"size"`
	UploadDate        time.Time `json:"uploadDate"`
	Status            string    `json:"status"`
	Tags              []Tag     `json:"tags"`
	UserID            string    `json:"userId"`
	OrganizationID    *string   `json:"organizationId"`
	ProcessID         *string   `json:"processId"`
	ProcessInstanceID *string   `json:"processInstanceId"`
	Description       *string   `json:"description"`
	Version           *int64    `json:"version"`
	FilePath          string    `json:"filePath"`
}

// Version mirrors the Java DocumentVersionResponseDTO.
type Version struct {
	ID            int64     `json:"id"`
	VersionNumber int       `json:"versionNumber"`
	UploadDate    time.Time `json:"uploadDate"`
	Size          int64     `json:"size"`
	Type          string    `json:"type"`
	FileStorageID string    `json:"fileStorageId"`
}

// ProcessTags mirrors the Java ProcessTagsDTO (used by /tags/search).
type ProcessTags struct {
	ProcessID string `json:"process_id"`
	Tags      []Tag  `json:"tags"`
}
