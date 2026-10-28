// Package models defines domain models for arkUI.
// These are separate from database representations to maintain clean separation.
package models

import (
	"time"
)

// User represents a user account in the system.
type User struct {
	ID           int64     `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"` // Never expose password hash in JSON
	Email        string    `json:"email,omitempty"`
	Role         UserRole  `json:"role"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// UserRole defines user access levels.
type UserRole string

const (
	RoleAdmin UserRole = "admin"
	RoleUser  UserRole = "user"
)

// IsAdmin checks if the user has admin privileges.
func (u *User) IsAdmin() bool {
	return u.Role == RoleAdmin
}

// BackupPolicy defines retention rules for backups.
type BackupPolicy struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	KeepCount   *int      `json:"keep_count,omitempty"` // Keep N most recent backups (nil = unlimited)
	KeepDays    *int      `json:"keep_days,omitempty"`  // Keep backups for N days (nil = unlimited)
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ShouldDeleteBackup determines if a backup should be deleted based on this policy.
// Returns true if the backup is outside the retention window. A rule of zero
// or less is no rule: it would delete every backup, the one just made included.
func (p *BackupPolicy) ShouldDeleteBackup(backupTime time.Time, backupIndex int, totalBackups int) bool {
	// Check count-based retention (backupIndex is 0-based, 0 = most recent)
	if p.KeepCount != nil && *p.KeepCount > 0 && backupIndex >= *p.KeepCount {
		return true
	}

	// Check time-based retention
	if p.KeepDays != nil && *p.KeepDays > 0 {
		cutoffTime := time.Now().AddDate(0, 0, -*p.KeepDays)
		if backupTime.Before(cutoffTime) {
			return true
		}
	}

	return false
}

// BackupJob represents a scheduled backup job.
type BackupJob struct {
	ID          int64         `json:"id"`
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	VolumeName  string        `json:"volume_name"`
	Schedule    string        `json:"schedule"` // Cron expression
	PolicyID    *int64        `json:"policy_id,omitempty"`
	Policy      *BackupPolicy `json:"policy,omitempty"` // Joined policy data
	Enabled     bool          `json:"enabled"`
	NextRun     *time.Time    `json:"next_run,omitempty"`
	LastRun     *time.Time    `json:"last_run,omitempty"`
	LastStatus  JobStatus     `json:"last_status,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
}

// JobStatus represents the status of a job execution.
type JobStatus string

const (
	JobStatusRunning JobStatus = "running"
	JobStatusSuccess JobStatus = "success"
	JobStatusFailed  JobStatus = "failed"
)

// JobHistory represents a single execution of a backup job.
type JobHistory struct {
	ID           int64      `json:"id"`
	JobID        int64      `json:"job_id"`
	Job          *BackupJob `json:"job,omitempty"` // Optional joined job data
	StartedAt    time.Time  `json:"started_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	Status       JobStatus  `json:"status"`
	ArchiveID    string     `json:"archive_id,omitempty"`    // ID of created archive
	ErrorMessage string     `json:"error_message,omitempty"` // Error details if failed
}

// Duration returns the execution duration if completed.
func (h *JobHistory) Duration() *time.Duration {
	if h.CompletedAt == nil {
		return nil
	}
	d := h.CompletedAt.Sub(h.StartedAt)
	return &d
}

// IsRunning checks if the job is currently running.
func (h *JobHistory) IsRunning() bool {
	return h.Status == JobStatusRunning && h.CompletedAt == nil
}

// CreateUserInput represents input for creating a new user.
type CreateUserInput struct {
	Username string   `json:"username"`
	Password string   `json:"password"`
	Email    string   `json:"email,omitempty"`
	Role     UserRole `json:"role"`
}

// CreatePolicyInput represents input for creating a backup policy.
type CreatePolicyInput struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	KeepCount   *int   `json:"keep_count,omitempty"`
	KeepDays    *int   `json:"keep_days,omitempty"`
}

// CreateJobInput represents input for creating a backup job.
type CreateJobInput struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	VolumeName  string `json:"volume_name"`
	Schedule    string `json:"schedule"`
	PolicyID    *int64 `json:"policy_id,omitempty"`
	Enabled     bool   `json:"enabled"`
}

// UpdateJobInput represents input for updating a backup job.
type UpdateJobInput struct {
	Name        *string    `json:"name,omitempty"`
	VolumeName  *string    `json:"volume_name,omitempty"`
	Description *string    `json:"description,omitempty"`
	Schedule    *string    `json:"schedule,omitempty"`
	PolicyID    *int64     `json:"policy_id,omitempty"`
	Enabled     *bool      `json:"enabled,omitempty"`
	NextRun     *time.Time `json:"next_run,omitempty"`
	LastRun     *time.Time `json:"last_run,omitempty"`
	LastStatus  *JobStatus `json:"last_status,omitempty"`
}
