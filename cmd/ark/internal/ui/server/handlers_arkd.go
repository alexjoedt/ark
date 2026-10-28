package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/cmd/ark/internal/version"
)

// apiSnapshot is the snapshot shape the web UI reads (web/src/lib/types.ts).
type apiSnapshot struct {
	ID          string    `json:"id"`
	Volume      string    `json:"volume"`
	SourceType  string    `json:"source_type"`
	CreatedAt   time.Time `json:"created_at"`
	SizeBytes   int64     `json:"size_bytes"`
	StoredBytes int64     `json:"stored_bytes"`
	Comment     string    `json:"comment,omitempty"`
	FileCount   int       `json:"file_count"`
}

func newAPISnapshot(s *ark.Snapshot) apiSnapshot {
	return apiSnapshot{
		ID:          s.ID,
		Volume:      s.SourceName,
		SourceType:  string(s.SourceType),
		CreatedAt:   s.CreatedAt,
		SizeBytes:   s.Stats.TotalSize,
		StoredBytes: s.Stats.StoredSize,
		Comment:     s.Comment,
		FileCount:   s.Stats.FileCount,
	}
}

// apiSnapshots never yields null, the UI iterates the list.
func apiSnapshots(list []*ark.Snapshot) []apiSnapshot {
	res := make([]apiSnapshot, 0, len(list))
	for _, s := range list {
		res = append(res, newAPISnapshot(s))
	}
	return res
}

// handleListVolumes lists all Docker volumes.
func (s *Server) handleListVolumes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	volumes, err := s.docker.ListVolumes(ctx)
	if err != nil {
		// Check if it's a Docker connection error
		if isDockerUnavailableError(err) {
			s.logger.Warn("docker unavailable, returning empty volumes", "error", err)
			w.Header().Set("X-Docker-Available", "false")
			s.respondJSON(w, http.StatusOK, []any{})
			return
		}
		s.logger.Error("failed to list volumes", "error", err)
		s.respondError(w, http.StatusInternalServerError, "failed to list volumes")
		return
	}

	w.Header().Set("X-Docker-Available", "true")
	s.respondJSON(w, http.StatusOK, volumes)
}

// handleGetVolume retrieves a specific volume.
func (s *Server) handleGetVolume(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	volume, err := s.docker.GetVolume(r.Context(), name)
	if err != nil {
		s.logger.Error("failed to get volume", "name", name, "error", err)
		s.respondError(w, http.StatusNotFound, "volume not found")
		return
	}

	s.respondJSON(w, http.StatusOK, volume)
}

// handleListSnapshots lists snapshots for a specific volume.
func (s *Server) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	snapshots, err := s.repo.Snapshots(r.Context(), ark.SnapshotFilter{Name: name})
	if err != nil {
		s.logger.Error("failed to list snapshots", "volume", name, "error", err)
		s.respondError(w, http.StatusInternalServerError, "failed to list snapshots")
		return
	}

	s.respondJSON(w, http.StatusOK, map[string]any{
		"snapshots": apiSnapshots(snapshots),
	})
}

// handleListAllSnapshots lists all backup snapshots.
func (s *Server) handleListAllSnapshots(w http.ResponseWriter, r *http.Request) {
	snapshots, err := s.repo.Snapshots(r.Context(), ark.SnapshotFilter{})
	if err != nil {
		s.logger.Error("failed to list snapshots", "error", err)
		s.respondError(w, http.StatusInternalServerError, "failed to list snapshots")
		return
	}

	s.respondJSON(w, http.StatusOK, apiSnapshots(snapshots))
}

// handleGetSnapshot retrieves a specific snapshot by ID.
func (s *Server) handleGetSnapshot(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	snap, err := s.repo.Snapshot(r.Context(), id)
	if err != nil {
		s.logger.Error("failed to get snapshot", "id", id, "error", err)
		s.respondError(w, http.StatusNotFound, "snapshot not found")
		return
	}

	s.respondJSON(w, http.StatusOK, newAPISnapshot(snap))
}

// handleDeleteSnapshot removes a snapshot by ID.
func (s *Server) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	if _, err := s.repo.Forget(r.Context(), ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: []string{id}}); err != nil {
		s.logger.Error("failed to delete snapshot", "id", id, "error", err)
		s.respondError(w, http.StatusInternalServerError, "failed to delete snapshot")
		return
	}

	s.respondJSON(w, http.StatusOK, map[string]string{
		"message": "snapshot deleted successfully",
	})
}

// handleBackup triggers a backup.
func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	var input struct {
		VolumeName string `json:"volume_name"`
		Comment    string `json:"comment"`
	}

	if err := s.decodeJSON(w, r, &input, 4096); err != nil {
		s.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()

		start := time.Now()
		snapshot, err := s.docker.BackupVolume(ctx, input.VolumeName, ark.WithComment(input.Comment))
		elapsed := time.Since(start)
		if err != nil {
			s.logger.Error("backup failed", "volume", input.VolumeName, "error", err)
			s.hub.Broadcast("backup.failed", map[string]any{
				"volume": input.VolumeName,
				"error":  err.Error(),
			})
			return
		}
		s.hub.Broadcast("backup.completed", map[string]any{
			"volume":           input.VolumeName,
			"snapshot_id":      snapshot.ID,
			"size_bytes":       snapshot.Stats.TotalSize,
			"duration_seconds": elapsed.Seconds(),
		})
	}()

	s.respondJSON(w, http.StatusAccepted, nil)
}

// handleRestore triggers a restore.
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	var input struct {
		SnapshotID   string `json:"snapshot_id"`
		TargetVolume string `json:"target_volume"`
		Force        bool   `json:"force"`
	}

	if err := s.decodeJSON(w, r, &input, 4096); err != nil {
		s.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()

		start := time.Now()
		_, err := s.docker.RestoreVolume(ctx, input.SnapshotID, input.TargetVolume)
		elapsed := time.Since(start)
		if err != nil {
			s.logger.Error("restore failed", "snapshot_id", input.SnapshotID, "error", err)
			s.hub.Broadcast("restore.failed", map[string]any{
				"snapshot_id": input.SnapshotID,
				"volume":      input.TargetVolume,
				"error":       err.Error(),
			})
			return
		}
		s.hub.Broadcast("restore.completed", map[string]any{
			"snapshot_id":      input.SnapshotID,
			"volume":           input.TargetVolume,
			"duration_seconds": elapsed.Seconds(),
		})
	}()

	s.respondJSON(w, http.StatusAccepted, nil)
}

// handleRestoreDryRun performs a restore dry-run.
func (s *Server) handleRestoreDryRun(w http.ResponseWriter, r *http.Request) {
	var input struct {
		SnapshotID   string `json:"snapshot_id"`
		TargetVolume string `json:"target_volume"`
		Force        bool   `json:"force,omitempty"`
	}

	if err := s.decodeJSON(w, r, &input, 4096); err != nil {
		s.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	result, err := s.docker.DryRunVolume(r.Context(), input.SnapshotID, input.TargetVolume)
	if err != nil {
		s.logger.Error("dry-run failed", "snapshot_id", input.SnapshotID, "error", err)
		s.respondError(w, http.StatusInternalServerError, "dry-run failed")
		return
	}

	s.respondJSON(w, http.StatusOK, result)
}

// handleStatus returns engine status information.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	type statusResponse struct {
		Status  string `json:"status"`
		Version string `json:"version"`
		RepoDir string `json:"repo_dir"`
	}

	resp := statusResponse{
		Status:  "ok",
		Version: version.GetVersion(),
		RepoDir: s.repoDir,
	}

	s.respondJSON(w, http.StatusOK, resp)
}

// handleDockerHealth checks Docker availability.
func (s *Server) handleDockerHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	_, err := s.docker.ListVolumes(ctx)
	available := err == nil || (!isDockerUnavailableError(err) && !errors.Is(err, context.DeadlineExceeded))

	s.respondJSON(w, http.StatusOK, map[string]any{
		"docker_available": available,
		"error":            getErrorMessage(err),
	})
}

// isDockerUnavailableError checks if an error is due to Docker daemon being unavailable.
func isDockerUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	errMsg := err.Error()
	return strings.Contains(errMsg, "Cannot connect to the Docker daemon") ||
		strings.Contains(errMsg, "docker daemon not reachable") ||
		strings.Contains(errMsg, "connection refused") ||
		(strings.Contains(errMsg, "no such file or directory") && strings.Contains(errMsg, "docker.sock"))
}

func getErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
