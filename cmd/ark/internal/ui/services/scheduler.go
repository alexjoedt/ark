package services

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/alexjoedt/log"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkdocker"
	"github.com/alexjoedt/ark/cmd/ark/internal/ui/models"
	"github.com/alexjoedt/ark/cmd/ark/internal/ui/websocket"
	"github.com/robfig/cron/v3"
)

// Scheduler manages scheduled backup jobs using cron.
// It's a deep module - simple Start/Stop interface, complex scheduling hidden.
type Scheduler struct {
	cron           *cron.Cron
	jobService     *JobService
	docker         *arkdocker.Engine
	repo           *ark.Repository
	policyService  *PolicyService
	historyService *JobHistoryService
	hub            *websocket.Hub
	logger         *log.Logger
	running        map[int64]bool // Track running jobs to prevent concurrent execution
	mu             sync.Mutex
}

// NewScheduler creates a new Scheduler instance.
func NewScheduler(
	jobService *JobService,
	docker *arkdocker.Engine,
	repo *ark.Repository,
	policyService *PolicyService,
	historyService *JobHistoryService,
	hub *websocket.Hub,
	logger *log.Logger,
) *Scheduler {
	return &Scheduler{
		cron:           cron.New(),
		jobService:     jobService,
		docker:         docker,
		repo:           repo,
		policyService:  policyService,
		historyService: historyService,
		hub:            hub,
		logger:         logger,
		running:        make(map[int64]bool),
	}
}

// Start starts the scheduler and loads all enabled jobs.
func (s *Scheduler) Start() error {
	// Load all enabled jobs
	jobs, err := s.jobService.List()
	if err != nil {
		return fmt.Errorf("failed to load jobs: %w", err)
	}

	// Schedule each enabled job
	for _, job := range jobs {
		if job.Enabled {
			if err := s.scheduleJob(job); err != nil {
				s.logger.Error("failed to schedule job", "job_id", job.ID, "job_name", job.Name, "error", err)
				// Continue scheduling other jobs even if one fails
			}
		}
	}

	// Start the cron scheduler
	s.cron.Start()
	s.logger.Info("scheduler started", "scheduled_jobs", len(s.cron.Entries()))

	return nil
}

// Stop stops the scheduler gracefully.
func (s *Scheduler) Stop() {
	ctx := s.cron.Stop()
	<-ctx.Done()
	s.logger.Info("scheduler stopped")
}

// scheduleJob adds a job to the cron scheduler.
func (s *Scheduler) scheduleJob(job *models.BackupJob) error {
	// Parse cron expression
	schedule, err := s.jobService.parser.Parse(job.Schedule)
	if err != nil {
		return fmt.Errorf("invalid cron expression: %w", err)
	}

	// Add to cron
	_ = s.cron.Schedule(schedule, cron.FuncJob(func() {
		s.executeJob(job.ID)
	}))

	// Calculate and update next run time
	now := time.Now()
	nextRun := schedule.Next(now)
	_, err = s.jobService.Update(job.ID, models.UpdateJobInput{
		NextRun: &nextRun,
	})
	if err != nil {
		s.logger.Error("failed to update next run time", "job_id", job.ID, "error", err)
	}

	s.logger.Info("job scheduled", "job_id", job.ID, "job_name", job.Name, "next_run", nextRun, "now", now)
	return nil
}

// executeJob executes a backup job.
func (s *Scheduler) executeJob(jobID int64) {
	// Check if job is already running
	s.mu.Lock()
	if s.running[jobID] {
		s.logger.Warn("job already running, skipping", "job_id", jobID)
		s.mu.Unlock()
		return
	}
	s.running[jobID] = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.running, jobID)
		s.mu.Unlock()
	}()

	// Get job details
	job, err := s.jobService.GetByID(jobID)
	if err != nil {
		s.logger.Error("failed to get job", "job_id", jobID, "error", err)
		return
	}

	s.logger.Info("executing job", "job_id", jobID, "job_name", job.Name, "volume", job.VolumeName)

	// Broadcast job start
	s.hub.Broadcast(websocket.MessageTypeJobStart, map[string]any{
		"job_id":      jobID,
		"job_name":    job.Name,
		"volume_name": job.VolumeName,
	})

	// Create history entry
	history, err := s.historyService.Create(jobID)
	if err != nil {
		s.logger.Error("failed to create history entry", "job_id", jobID, "error", err)
		return
	}

	// Execute backup
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	comment := fmt.Sprintf("Scheduled backup by job: %s", job.Name)
	start := time.Now()
	snapshot, err := s.docker.BackupVolume(ctx, job.VolumeName, ark.WithComment(comment))
	elapsed := time.Since(start)

	// Update job last run
	now := time.Now()
	nextRun, _ := s.jobService.CalculateNextRun(job.Schedule, now)

	var status models.JobStatus
	var archiveID, errorMsg string

	if err != nil {
		s.logger.Error("backup failed", "job_id", jobID, "error", err)
		status = models.JobStatusFailed
		errorMsg = err.Error()

		// Broadcast backup failure
		s.hub.Broadcast(websocket.MessageTypeBackupFailed, map[string]any{
			"volume": job.VolumeName,
			"error":  errorMsg,
			"job_id": jobID,
		})

		// Broadcast job failed
		s.hub.Broadcast(websocket.MessageTypeJobFailed, map[string]any{
			"job_id":   jobID,
			"job_name": job.Name,
			"error":    errorMsg,
		})
	} else {
		s.logger.Info("backup completed", "job_id", jobID, "archive", snapshot.ID)
		status = models.JobStatusSuccess
		archiveID = snapshot.ID

		// Broadcast backup completion
		s.hub.Broadcast(websocket.MessageTypeBackupComplete, map[string]any{
			"volume":           job.VolumeName,
			"snapshot_id":      snapshot.ID,
			"size_bytes":       snapshot.Stats.TotalSize,
			"duration_seconds": elapsed.Seconds(),
			"job_id":           jobID,
		})

		// Broadcast job complete
		s.hub.Broadcast(websocket.MessageTypeJobComplete, map[string]any{
			"job_id":     jobID,
			"job_name":   job.Name,
			"archive_id": archiveID,
		})

		// Enforce retention policy if configured
		if job.PolicyID != nil {
			if err := s.enforcePolicy(job.VolumeName, *job.PolicyID); err != nil {
				s.logger.Error("failed to enforce policy", "job_id", jobID, "policy_id", *job.PolicyID, "error", err)
			}
		}
	}

	// Complete history entry
	if err := s.historyService.Complete(history.ID, status, archiveID, errorMsg); err != nil {
		s.logger.Error("failed to complete history entry", "history_id", history.ID, "error", err)
	}

	// Update job status
	jobStatus := status
	_, err = s.jobService.Update(jobID, models.UpdateJobInput{
		LastRun:    &now,
		LastStatus: &jobStatus,
		NextRun:    &nextRun,
	})
	if err != nil {
		s.logger.Error("failed to update job status", "job_id", jobID, "error", err)
	}
}

// enforcePolicy enforces retention policy by deleting old backups.
func (s *Scheduler) enforcePolicy(volumeName string, policyID int64) error {
	// Get policy
	policy, err := s.policyService.GetByID(policyID)
	if err != nil {
		return fmt.Errorf("failed to get policy: %w", err)
	}

	// If policy has no retention rules, skip
	if policy.KeepCount == nil && policy.KeepDays == nil {
		return nil
	}

	// List snapshots for this volume
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	snapshots, err := s.repo.Snapshots(ctx, ark.SnapshotFilter{Name: volumeName})
	if err != nil {
		return fmt.Errorf("failed to list snapshots: %w", err)
	}

	// Sort by creation time descending (newest first)
	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].CreatedAt.After(snapshots[j].CreatedAt)
	})

	// Determine which snapshots to delete
	var toDelete []string
	for idx, snap := range snapshots {
		if policy.ShouldDeleteBackup(snap.CreatedAt, idx, len(snapshots)) {
			toDelete = append(toDelete, snap.ID)
		}
	}

	// Delete snapshots per retention policy
	for _, id := range toDelete {
		s.logger.Info("deleting snapshot per retention policy", "id", id, "policy_id", policyID)
		delCtx, delCancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, delErr := s.repo.Forget(delCtx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: []string{id}})
		delCancel()
		if delErr != nil {
			s.logger.Error("failed to delete snapshot", "id", id, "error", delErr)
			// Continue deleting other snapshots even if one fails
			continue
		}
	}

	if len(toDelete) > 0 {
		s.logger.Info("retention policy enforced", "policy_id", policyID, "deleted", len(toDelete))
	}

	return nil
}

// ReloadJobs reloads all jobs from the database and reschedules them.
// This should be called when jobs are added/updated/deleted.
func (s *Scheduler) ReloadJobs() error {
	// Stop all scheduled jobs
	entries := s.cron.Entries()
	for _, entry := range entries {
		s.cron.Remove(entry.ID)
	}

	// Reload jobs
	jobs, err := s.jobService.List()
	if err != nil {
		return fmt.Errorf("failed to load jobs: %w", err)
	}

	// Schedule each enabled job
	for _, job := range jobs {
		if job.Enabled {
			if err := s.scheduleJob(job); err != nil {
				s.logger.Error("failed to schedule job", "job_id", job.ID, "error", err)
			}
		}
	}

	s.logger.Info("jobs reloaded", "scheduled_jobs", len(s.cron.Entries()))
	return nil
}

// RunJobNow executes a job immediately, outside of its schedule.
func (s *Scheduler) RunJobNow(jobID int64) error {
	// Check if job is already running
	s.mu.Lock()
	if s.running[jobID] {
		s.mu.Unlock()
		return errors.New("job is already running")
	}
	s.mu.Unlock()

	// Execute in goroutine to return immediately
	go s.executeJob(jobID)

	return nil
}
