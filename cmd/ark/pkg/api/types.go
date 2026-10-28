// Package api provides shared types for the arkd API used by both server and client.
package api

// BackupRequest is the request body for POST /v1/backup
type BackupRequest struct {
	Volume  string `json:"volume"`
	Comment string `json:"comment,omitempty"`
}

// BackupResponse is the response for POST /v1/backup
type BackupResponse struct {
	ID                string  `json:"id"` // Unique snapshot ID (26 chars, ULID-style)
	SizeBytes         int64   `json:"size_bytes"`
	DurationSeconds   float64 `json:"duration_seconds"`
	ContainersStopped int     `json:"containers_stopped"`
}

// RestoreRequest is the request body for POST /v1/restore
type RestoreRequest struct {
	SnapshotID   string `json:"snapshot_id"`
	TargetVolume string `json:"target_volume"`
	Force        bool   `json:"force"`
	BackupBefore bool   `json:"backup_before,omitempty"`
}

// RestoreResponse is the response for POST /v1/restore
type RestoreResponse struct {
	TargetVolume      string   `json:"target_volume"`
	SnapshotID        string   `json:"snapshot_id"`
	PreRestoreBackup  string   `json:"pre_restore_backup,omitempty"`
	DurationSeconds   float64  `json:"duration_seconds"`
	FilesRestored     int      `json:"files_restored"`
	BytesRestored     int64    `json:"bytes_restored"`
	ContainersStopped int      `json:"containers_stopped"`
	Warnings          []string `json:"warnings,omitempty"`
}

// DryRunRequest is the request body for POST /v1/restore/dry-run
type DryRunRequest struct {
	SnapshotID   string `json:"snapshot_id"`
	TargetVolume string `json:"target_volume"`
	Force        bool   `json:"force,omitempty"`
}

// DryRunResponse is the response for POST /v1/restore/dry-run
type DryRunResponse struct {
	TargetVolume       string   `json:"target_volume"`
	SnapshotID         string   `json:"snapshot_id"`
	SourceVolume       string   `json:"source_volume"`
	ContainersAffected []string `json:"containers_affected"`
	WillEraseData      bool     `json:"will_erase_data"`
	Warnings           []string `json:"warnings"`
}

// ArchiveInfo represents snapshot information in API responses.
// The name "ArchiveInfo" is kept for backwards compatibility.
type ArchiveInfo struct {
	ID          string `json:"id"` // Unique snapshot ID (26 chars, ULID-style)
	Volume      string `json:"volume"`
	SizeBytes   int64  `json:"size_bytes"`
	StoredBytes int64  `json:"stored_bytes,omitempty"`
	CreatedAt   string `json:"created_at"`
	Comment     string `json:"comment,omitempty"`
	FileCount   int    `json:"file_count,omitempty"`
}

// VolumeInfo represents Docker volume information
type VolumeInfo struct {
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	Mountpoint string            `json:"mountpoint"`
	CreatedAt  string            `json:"created_at,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
}

// HealthResponse is the response for GET /v1/health
type HealthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// ReadyResponse is the response for GET /v1/ready
type ReadyResponse struct {
	Ready   bool              `json:"ready"`
	Checks  map[string]string `json:"checks"`
	Version string            `json:"version"`
}

// StatusResponse is the response for GET /v1/status
type StatusResponse struct {
	Status         string `json:"status"`
	Version        string `json:"version"`
	BackupDir      string `json:"backup_dir"`
	SocketPath     string `json:"socket_path"`
	SnapshotsCount int    `json:"snapshots_count"`
	OperationLock  bool   `json:"operation_lock"`
	LockOperation  string `json:"lock_operation,omitempty"`
	LockResource   string `json:"lock_resource,omitempty"`
}

// ArchiveFilter is used to filter archives in list operations
type ArchiveFilter struct {
	Volume    string
	StartTime string
	EndTime   string
}

// PaginationParams contains pagination parameters for list operations
type PaginationParams struct {
	Page     int    `json:"page"`      // 1-based page number (default: 1)
	PageSize int    `json:"page_size"` // Number of items per page (default: 25, max: 100)
	Cursor   string `json:"cursor"`    // Opaque cursor for cursor-based pagination (optional)
}

// DefaultPageSize is the default number of items per page
const DefaultPageSize = 25

// MaxPageSize is the maximum allowed page size
const MaxPageSize = 100

// Pagination contains pagination information in responses
type Pagination struct {
	CurrentPage  int  `json:"current_page"`  // Current page number (1-based)
	PageSize     int  `json:"page_size"`     // Number of items per page
	TotalPages   int  `json:"total_pages"`   // Total number of pages
	TotalRecords int  `json:"total_records"` // Total number of records across all pages
	HasNextPage  bool `json:"has_next_page"` // Whether there is a next page
	HasPrevPage  bool `json:"has_prev_page"` // Whether there is a previous page
}

// PaginatedArchivesResponse wraps archive list with pagination metadata
type PaginatedArchivesResponse struct {
	Archives   []ArchiveInfo `json:"archives"`
	Pagination Pagination    `json:"pagination"`
}

// PaginatedVolumesResponse wraps volume list with pagination metadata
type PaginatedVolumesResponse struct {
	Volumes    []VolumeInfo `json:"volumes"`
	Pagination Pagination   `json:"pagination"`
}

// DeleteArchiveResponse is the response for DELETE /v1/archives/{name}
type DeleteArchiveResponse struct {
	Deleted  bool   `json:"deleted"`
	Filename string `json:"filename"`
	Message  string `json:"message"`
}

// PruneRequest is the request body for POST /v1/archives/prune
type PruneRequest struct {
	// DryRun previews what would be removed without actually deleting.
	DryRun bool `json:"dry_run,omitempty"`
}

// PruneResponse is the response for POST /v1/archives/prune
type PruneResponse struct {
	ScannedChunks int   `json:"scanned_chunks"`
	LiveChunks    int   `json:"live_chunks"`
	DeletedChunks int   `json:"deleted_chunks"`
	FreedBytes    int64 `json:"freed_bytes"`
	DryRun        bool  `json:"dry_run"`
}

// VerifyArchiveResponse is the response for POST /v1/archives/{id}/verify
type VerifyArchiveResponse struct {
	// OK is true when all chunks are present and intact.
	OK bool `json:"ok"`
	// SnapshotID is the snapshot that was verified.
	SnapshotID string `json:"snapshot_id"`
	// Volume is the volume the snapshot belongs to.
	Volume string `json:"volume"`
	// TotalChunks is the number of unique chunks referenced by the snapshot.
	TotalChunks int `json:"total_chunks"`
	// VerifiedChunks is the number of chunks that passed integrity checks.
	VerifiedChunks int `json:"verified_chunks"`
	// MissingChunks contains IDs of chunks that were not found on disk.
	MissingChunks []string `json:"missing_chunks,omitempty"`
	// CorruptChunks contains IDs of chunks whose hash did not match.
	CorruptChunks []string `json:"corrupt_chunks,omitempty"`
	// Message contains a human-readable summary.
	Message string `json:"message"`
}

// ImportResponse is the response for POST /v1/archives/import.
// The request body is raw application/octet-stream; filename is passed as ?filename= query param.
type ImportResponse struct {
	ID        string `json:"id"`
	Filename  string `json:"filename"`
	SizeBytes int64  `json:"size_bytes"`
	Volume    string `json:"volume"`
}
