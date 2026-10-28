package services

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/alexjoedt/ark/cmd/ark/internal/ui/models"
)

// JobHistoryService provides job execution history operations.
type JobHistoryService struct {
	db *sql.DB
}

// NewJobHistoryService creates a new JobHistoryService instance.
func NewJobHistoryService(db *sql.DB) *JobHistoryService {
	return &JobHistoryService{db: db}
}

// Create records the start of a job execution.
func (s *JobHistoryService) Create(jobID int64) (*models.JobHistory, error) {
	now := time.Now().Unix()
	result, err := s.db.Exec(`
		INSERT INTO job_history (job_id, started_at, status)
		VALUES (?, ?, ?)
	`, jobID, now, string(models.JobStatusRunning))
	if err != nil {
		return nil, fmt.Errorf("failed to create job history: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("failed to get history ID: %w", err)
	}

	return &models.JobHistory{
		ID:        id,
		JobID:     jobID,
		StartedAt: time.Unix(now, 0),
		Status:    models.JobStatusRunning,
	}, nil
}

// Complete marks a job execution as completed (success or failure).
func (s *JobHistoryService) Complete(id int64, status models.JobStatus, archiveID, errorMsg string) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`
		UPDATE job_history
		SET completed_at = ?, status = ?, archive_id = ?, error_message = ?
		WHERE id = ?
	`, now, string(status), archiveID, errorMsg, id)
	if err != nil {
		return fmt.Errorf("failed to complete job history: %w", err)
	}
	return nil
}

// GetByID retrieves a job history entry by ID.
func (s *JobHistoryService) GetByID(id int64) (*models.JobHistory, error) {
	var history models.JobHistory
	var startedAt int64
	var completedAt sql.NullInt64
	var archiveID, errorMsg sql.NullString

	err := s.db.QueryRow(`
		SELECT id, job_id, started_at, completed_at, status, archive_id, error_message
		FROM job_history
		WHERE id = ?
	`, id).Scan(&history.ID, &history.JobID, &startedAt, &completedAt, &history.Status,
		&archiveID, &errorMsg)

	if err == sql.ErrNoRows {
		return nil, errors.New("job history not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query job history: %w", err)
	}

	history.StartedAt = time.Unix(startedAt, 0)
	if completedAt.Valid {
		completedTime := time.Unix(completedAt.Int64, 0)
		history.CompletedAt = &completedTime
	}
	if archiveID.Valid {
		history.ArchiveID = archiveID.String
	}
	if errorMsg.Valid {
		history.ErrorMessage = errorMsg.String
	}

	return &history, nil
}

// ListByJob returns all history entries for a specific job.
func (s *JobHistoryService) ListByJob(jobID int64, limit int) ([]*models.JobHistory, error) {
	if limit <= 0 {
		limit = 100 // sensible default
	}

	rows, err := s.db.Query(`
		SELECT id, job_id, started_at, completed_at, status, archive_id, error_message
		FROM job_history
		WHERE job_id = ?
		ORDER BY started_at DESC
		LIMIT ?
	`, jobID, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query job history: %w", err)
	}
	defer rows.Close()

	var histories []*models.JobHistory
	for rows.Next() {
		var history models.JobHistory
		var startedAt int64
		var completedAt sql.NullInt64
		var archiveID, errorMsg sql.NullString

		err := rows.Scan(&history.ID, &history.JobID, &startedAt, &completedAt, &history.Status,
			&archiveID, &errorMsg)
		if err != nil {
			return nil, fmt.Errorf("failed to scan job history: %w", err)
		}

		history.StartedAt = time.Unix(startedAt, 0)
		if completedAt.Valid {
			completedTime := time.Unix(completedAt.Int64, 0)
			history.CompletedAt = &completedTime
		}
		if archiveID.Valid {
			history.ArchiveID = archiveID.String
		}
		if errorMsg.Valid {
			history.ErrorMessage = errorMsg.String
		}

		histories = append(histories, &history)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating job history: %w", err)
	}

	return histories, nil
}

// ListRecent returns recent job history entries across all jobs.
func (s *JobHistoryService) ListRecent(limit int) ([]*models.JobHistory, error) {
	if limit <= 0 {
		limit = 50 // sensible default
	}

	rows, err := s.db.Query(`
		SELECT h.id, h.job_id, h.started_at, h.completed_at, h.status, h.archive_id, h.error_message,
		       j.name, j.volume_name
		FROM job_history h
		INNER JOIN backup_jobs j ON h.job_id = j.id
		ORDER BY h.started_at DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query job history: %w", err)
	}
	defer rows.Close()

	var histories []*models.JobHistory
	for rows.Next() {
		var history models.JobHistory
		var job models.BackupJob
		var startedAt int64
		var completedAt sql.NullInt64
		var archiveID, errorMsg sql.NullString

		err := rows.Scan(&history.ID, &history.JobID, &startedAt, &completedAt, &history.Status,
			&archiveID, &errorMsg, &job.Name, &job.VolumeName)
		if err != nil {
			return nil, fmt.Errorf("failed to scan job history: %w", err)
		}

		history.StartedAt = time.Unix(startedAt, 0)
		if completedAt.Valid {
			completedTime := time.Unix(completedAt.Int64, 0)
			history.CompletedAt = &completedTime
		}
		if archiveID.Valid {
			history.ArchiveID = archiveID.String
		}
		if errorMsg.Valid {
			history.ErrorMessage = errorMsg.String
		}

		// Attach minimal job info
		history.Job = &job

		histories = append(histories, &history)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating job history: %w", err)
	}

	return histories, nil
}
