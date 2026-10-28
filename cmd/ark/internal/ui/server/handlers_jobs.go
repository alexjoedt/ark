package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/alexjoedt/ark/cmd/ark/internal/ui/models"
	"github.com/alexjoedt/ark/cmd/ark/internal/ui/services"
	"github.com/go-chi/chi/v5"
)

// handleListJobs lists all backup jobs.
func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.jobService.List()
	if err != nil {
		s.logger.Error("failed to list jobs", "error", err)
		s.respondError(w, http.StatusInternalServerError, "failed to list jobs")
		return
	}

	s.respondJSON(w, http.StatusOK, map[string]any{
		"jobs": jobs,
	})
}

// handleCreateJob creates a new backup job.
func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var input models.CreateJobInput
	if err := s.decodeJSON(w, r, &input, 8192); err != nil {
		s.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	job, err := s.jobService.Create(input)
	if err != nil {
		if errors.Is(err, services.ErrJobExists) {
			s.respondError(w, http.StatusConflict, "job already exists")
		} else if errors.Is(err, services.ErrInvalidCron) {
			s.respondError(w, http.StatusBadRequest, "invalid cron expression")
		} else {
			s.logger.Error("failed to create job", "error", err)
			s.respondError(w, http.StatusInternalServerError, "failed to create job")
		}
		return
	}

	// Reload scheduler to pick up new job
	if err := s.scheduler.ReloadJobs(); err != nil {
		s.logger.Error("failed to reload scheduler", "error", err)
	}

	s.respondJSON(w, http.StatusCreated, map[string]any{
		"job": job,
	})
}

// handleGetJob retrieves a specific job.
func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, "invalid job ID")
		return
	}

	job, err := s.jobService.GetByID(id)
	if err != nil {
		if errors.Is(err, services.ErrJobNotFound) {
			s.respondError(w, http.StatusNotFound, "job not found")
		} else {
			s.logger.Error("failed to get job", "id", id, "error", err)
			s.respondError(w, http.StatusInternalServerError, "failed to get job")
		}
		return
	}

	s.respondJSON(w, http.StatusOK, map[string]any{
		"job": job,
	})
}

// handleUpdateJob updates a backup job.
func (s *Server) handleUpdateJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, "invalid job ID")
		return
	}

	var input models.UpdateJobInput
	if err := s.decodeJSON(w, r, &input, 8192); err != nil {
		s.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	job, err := s.jobService.Update(id, input)
	if err != nil {
		if errors.Is(err, services.ErrJobNotFound) {
			s.respondError(w, http.StatusNotFound, "job not found")
		} else if errors.Is(err, services.ErrInvalidCron) {
			s.respondError(w, http.StatusBadRequest, "invalid cron expression")
		} else {
			s.logger.Error("failed to update job", "id", id, "error", err)
			s.respondError(w, http.StatusInternalServerError, "failed to update job")
		}
		return
	}

	// Reload scheduler to pick up changes
	if err := s.scheduler.ReloadJobs(); err != nil {
		s.logger.Error("failed to reload scheduler", "error", err)
	}

	s.respondJSON(w, http.StatusOK, map[string]any{
		"job": job,
	})
}

// handleDeleteJob deletes a backup job.
func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, "invalid job ID")
		return
	}

	if err := s.jobService.Delete(id); err != nil {
		if errors.Is(err, services.ErrJobNotFound) {
			s.respondError(w, http.StatusNotFound, "job not found")
		} else {
			s.logger.Error("failed to delete job", "id", id, "error", err)
			s.respondError(w, http.StatusInternalServerError, "failed to delete job")
		}
		return
	}

	// Reload scheduler to remove job
	if err := s.scheduler.ReloadJobs(); err != nil {
		s.logger.Error("failed to reload scheduler", "error", err)
	}

	s.respondJSON(w, http.StatusOK, map[string]string{
		"message": "job deleted successfully",
	})
}

// handleRunJobNow triggers a job to run immediately.
func (s *Server) handleRunJobNow(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, "invalid job ID")
		return
	}

	if err := s.scheduler.RunJobNow(id); err != nil {
		s.logger.Error("failed to run job", "id", id, "error", err)
		s.respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.respondJSON(w, http.StatusAccepted, map[string]string{
		"message": "job execution started",
	})
}

// handleGetJobHistory retrieves execution history for a job.
func (s *Server) handleGetJobHistory(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, "invalid job ID")
		return
	}

	history, err := s.historyService.ListByJob(id, 50)
	if err != nil {
		s.logger.Error("failed to get job history", "id", id, "error", err)
		s.respondError(w, http.StatusInternalServerError, "failed to get job history")
		return
	}

	// Ensure we return an empty array instead of null
	if history == nil {
		history = []*models.JobHistory{}
	}

	s.respondJSON(w, http.StatusOK, history)
}

// handleListRecentHistory retrieves recent job execution history.
func (s *Server) handleListRecentHistory(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug("handleListRecentHistory called")
	// Get limit from query parameter, default to 50
	limit := 50
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			limit = l
		}
	}

	s.logger.Debug("fetching recent history", "limit", limit)
	history, err := s.historyService.ListRecent(limit)
	if err != nil {
		s.logger.Error("failed to get recent history", "error", err)
		s.respondError(w, http.StatusInternalServerError, "failed to get history")
		return
	}

	// Ensure we return an empty array instead of null
	if history == nil {
		history = []*models.JobHistory{}
	}

	s.logger.Debug("history retrieved successfully", "count", len(history))
	s.respondJSON(w, http.StatusOK, history)
}
