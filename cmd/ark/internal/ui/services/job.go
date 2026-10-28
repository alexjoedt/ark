package services

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/alexjoedt/ark/cmd/ark/internal/ui/models"
	"github.com/robfig/cron/v3"
)

// Job-related errors
var (
	ErrJobNotFound = errors.New("job not found")
	ErrJobExists   = errors.New("job already exists")
	ErrInvalidCron = errors.New("invalid cron expression")
)

// JobService provides backup job management operations.
type JobService struct {
	db     *sql.DB
	parser cron.Parser
}

// NewJobService creates a new JobService instance.
func NewJobService(db *sql.DB) *JobService {
	return &JobService{
		db:     db,
		parser: cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow),
	}
}

// Create creates a new backup job.
func (s *JobService) Create(input models.CreateJobInput) (*models.BackupJob, error) {
	// Validate cron expression
	schedule, err := s.parser.Parse(input.Schedule)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidCron, err)
	}

	// Calculate next run if enabled
	var nextRun *int64
	if input.Enabled {
		next := schedule.Next(time.Now())
		nextRunUnix := next.Unix()
		nextRun = &nextRunUnix
	}

	// Insert job
	now := time.Now().Unix()
	result, err := s.db.Exec(`
		INSERT INTO backup_jobs (name, description, volume_name, schedule, policy_id, enabled, next_run, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, input.Name, input.Description, input.VolumeName, input.Schedule, input.PolicyID, input.Enabled, nextRun, now, now)
	if err != nil {
		if isUniqueConstraintError(err) {
			return nil, ErrJobExists
		}
		return nil, fmt.Errorf("failed to create job: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("failed to get job ID: %w", err)
	}

	job := &models.BackupJob{
		ID:          id,
		Name:        input.Name,
		Description: input.Description,
		VolumeName:  input.VolumeName,
		Schedule:    input.Schedule,
		PolicyID:    input.PolicyID,
		Enabled:     input.Enabled,
		CreatedAt:   time.Unix(now, 0),
		UpdatedAt:   time.Unix(now, 0),
	}

	if nextRun != nil {
		nextRunTime := time.Unix(*nextRun, 0)
		job.NextRun = &nextRunTime
	}

	return job, nil
}

// GetByID retrieves a job by ID with optional policy data.
func (s *JobService) GetByID(id int64) (*models.BackupJob, error) {
	var job models.BackupJob
	var createdAt, updatedAt int64
	var nextRun, lastRun sql.NullInt64
	var lastStatus sql.NullString
	var policyID sql.NullInt64

	err := s.db.QueryRow(`
		SELECT id, name, description, volume_name, schedule, policy_id, enabled, 
		       next_run, last_run, last_status, created_at, updated_at
		FROM backup_jobs
		WHERE id = ?
	`, id).Scan(&job.ID, &job.Name, &job.Description, &job.VolumeName, &job.Schedule,
		&policyID, &job.Enabled, &nextRun, &lastRun, &lastStatus, &createdAt, &updatedAt)

	if err == sql.ErrNoRows {
		return nil, ErrJobNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query job: %w", err)
	}

	job.CreatedAt = time.Unix(createdAt, 0)
	job.UpdatedAt = time.Unix(updatedAt, 0)

	if policyID.Valid {
		job.PolicyID = &policyID.Int64
	}
	if nextRun.Valid {
		nextRunTime := time.Unix(nextRun.Int64, 0)
		job.NextRun = &nextRunTime
	}
	if lastRun.Valid {
		lastRunTime := time.Unix(lastRun.Int64, 0)
		job.LastRun = &lastRunTime
	}
	if lastStatus.Valid {
		status := models.JobStatus(lastStatus.String)
		job.LastStatus = status
	}

	return &job, nil
}

// List returns all backup jobs with optional policy data.
func (s *JobService) List() ([]*models.BackupJob, error) {
	rows, err := s.db.Query(`
		SELECT j.id, j.name, j.description, j.volume_name, j.schedule, j.policy_id, j.enabled,
		       j.next_run, j.last_run, j.last_status, j.created_at, j.updated_at,
		       p.id, p.name, p.description, p.keep_count, p.keep_days
		FROM backup_jobs j
		LEFT JOIN backup_policies p ON j.policy_id = p.id
		ORDER BY j.name
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to query jobs: %w", err)
	}
	defer rows.Close()

	var jobs []*models.BackupJob
	for rows.Next() {
		var job models.BackupJob
		var policy models.BackupPolicy
		var createdAt, updatedAt int64
		var nextRun, lastRun sql.NullInt64
		var lastStatus sql.NullString
		var policyID, policyIDJoin sql.NullInt64
		var policyName, policyDesc sql.NullString
		var keepCount, keepDays sql.NullInt64

		err := rows.Scan(&job.ID, &job.Name, &job.Description, &job.VolumeName, &job.Schedule,
			&policyID, &job.Enabled, &nextRun, &lastRun, &lastStatus, &createdAt, &updatedAt,
			&policyIDJoin, &policyName, &policyDesc, &keepCount, &keepDays)
		if err != nil {
			return nil, fmt.Errorf("failed to scan job: %w", err)
		}

		job.CreatedAt = time.Unix(createdAt, 0)
		job.UpdatedAt = time.Unix(updatedAt, 0)

		if policyID.Valid {
			job.PolicyID = &policyID.Int64
		}
		if nextRun.Valid {
			nextRunTime := time.Unix(nextRun.Int64, 0)
			job.NextRun = &nextRunTime
		}
		if lastRun.Valid {
			lastRunTime := time.Unix(lastRun.Int64, 0)
			job.LastRun = &lastRunTime
		}
		if lastStatus.Valid {
			status := models.JobStatus(lastStatus.String)
			job.LastStatus = status
		}

		// Add policy if joined
		if policyIDJoin.Valid {
			policy.ID = policyIDJoin.Int64
			policy.Name = policyName.String
			policy.Description = policyDesc.String
			if keepCount.Valid {
				count := int(keepCount.Int64)
				policy.KeepCount = &count
			}
			if keepDays.Valid {
				days := int(keepDays.Int64)
				policy.KeepDays = &days
			}
			job.Policy = &policy
		}

		jobs = append(jobs, &job)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating jobs: %w", err)
	}

	return jobs, nil
}

// Update updates a backup job.
func (s *JobService) Update(id int64, input models.UpdateJobInput) (*models.BackupJob, error) {
	// Validate cron if provided
	if input.Schedule != nil {
		if _, err := s.parser.Parse(*input.Schedule); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidCron, err)
		}
	}

	// Build UPDATE query dynamically based on provided fields
	query := "UPDATE backup_jobs SET "
	args := []any{}
	updates := []string{}

	if input.Name != nil {
		updates = append(updates, "name = ?")
		args = append(args, *input.Name)
	}
	if input.VolumeName != nil {
		updates = append(updates, "volume_name = ?")
		args = append(args, *input.VolumeName)
	}
	if input.Description != nil {
		updates = append(updates, "description = ?")
		args = append(args, *input.Description)
	}
	if input.Schedule != nil {
		updates = append(updates, "schedule = ?")
		args = append(args, *input.Schedule)
	}
	if input.PolicyID != nil {
		updates = append(updates, "policy_id = ?")
		args = append(args, *input.PolicyID)
	}
	if input.Enabled != nil {
		updates = append(updates, "enabled = ?")
		args = append(args, *input.Enabled)
	}
	if input.NextRun != nil {
		updates = append(updates, "next_run = ?")
		args = append(args, input.NextRun.Unix())
	}
	if input.LastRun != nil {
		updates = append(updates, "last_run = ?")
		args = append(args, input.LastRun.Unix())
	}
	if input.LastStatus != nil {
		updates = append(updates, "last_status = ?")
		args = append(args, string(*input.LastStatus))
	}

	if len(updates) == 0 {
		return s.GetByID(id) // No updates, just return existing
	}

	// Always update the updated_at timestamp
	updates = append(updates, "updated_at = ?")
	args = append(args, time.Now().Unix())

	//nolint:gosec // updates holds fixed column assignments, values go through args
	query += strings.Join(updates, ", ")
	query += " WHERE id = ?"
	args = append(args, id)

	result, err := s.db.Exec(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to update job: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return nil, ErrJobNotFound
	}

	return s.GetByID(id)
}

// Delete deletes a backup job by ID.
func (s *JobService) Delete(id int64) error {
	result, err := s.db.Exec("DELETE FROM backup_jobs WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("failed to delete job: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return ErrJobNotFound
	}

	return nil
}

// ListDue returns all enabled jobs that are due to run.
func (s *JobService) ListDue() ([]*models.BackupJob, error) {
	now := time.Now().Unix()
	rows, err := s.db.Query(`
		SELECT id, name, description, volume_name, schedule, policy_id, enabled,
		       next_run, last_run, last_status, created_at, updated_at
		FROM backup_jobs
		WHERE enabled = 1 AND next_run IS NOT NULL AND next_run <= ?
		ORDER BY next_run
	`, now)
	if err != nil {
		return nil, fmt.Errorf("failed to query due jobs: %w", err)
	}
	defer rows.Close()

	var jobs []*models.BackupJob
	for rows.Next() {
		var job models.BackupJob
		var createdAt, updatedAt int64
		var nextRun, lastRun sql.NullInt64
		var lastStatus sql.NullString
		var policyID sql.NullInt64

		err := rows.Scan(&job.ID, &job.Name, &job.Description, &job.VolumeName, &job.Schedule,
			&policyID, &job.Enabled, &nextRun, &lastRun, &lastStatus, &createdAt, &updatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan job: %w", err)
		}

		job.CreatedAt = time.Unix(createdAt, 0)
		job.UpdatedAt = time.Unix(updatedAt, 0)

		if policyID.Valid {
			job.PolicyID = &policyID.Int64
		}
		if nextRun.Valid {
			nextRunTime := time.Unix(nextRun.Int64, 0)
			job.NextRun = &nextRunTime
		}
		if lastRun.Valid {
			lastRunTime := time.Unix(lastRun.Int64, 0)
			job.LastRun = &lastRunTime
		}
		if lastStatus.Valid {
			status := models.JobStatus(lastStatus.String)
			job.LastStatus = status
		}

		jobs = append(jobs, &job)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating jobs: %w", err)
	}

	return jobs, nil
}

// CalculateNextRun calculates the next run time for a job's cron schedule.
func (s *JobService) CalculateNextRun(cronExpr string, from time.Time) (time.Time, error) {
	schedule, err := s.parser.Parse(cronExpr)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %w", ErrInvalidCron, err)
	}
	return schedule.Next(from), nil
}
