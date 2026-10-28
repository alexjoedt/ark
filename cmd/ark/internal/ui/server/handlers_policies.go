package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/alexjoedt/ark/cmd/ark/internal/ui/models"
	"github.com/alexjoedt/ark/cmd/ark/internal/ui/services"
	"github.com/go-chi/chi/v5"
)

// handleListPolicies lists all backup policies.
func (s *Server) handleListPolicies(w http.ResponseWriter, r *http.Request) {
	policies, err := s.policyService.List()
	if err != nil {
		s.logger.Error("failed to list policies", "error", err)
		s.respondError(w, http.StatusInternalServerError, "failed to list policies")
		return
	}

	s.respondJSON(w, http.StatusOK, map[string]any{
		"policies": policies,
	})
}

// handleCreatePolicy creates a new backup policy.
func (s *Server) handleCreatePolicy(w http.ResponseWriter, r *http.Request) {
	var input models.CreatePolicyInput
	if err := s.decodeJSON(w, r, &input, 4096); err != nil {
		s.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	policy, err := s.policyService.Create(input)
	if err != nil {
		if errors.Is(err, services.ErrPolicyExists) {
			s.respondError(w, http.StatusConflict, "policy already exists")
		} else {
			s.logger.Error("failed to create policy", "error", err)
			s.respondError(w, http.StatusInternalServerError, "failed to create policy")
		}
		return
	}

	s.respondJSON(w, http.StatusCreated, map[string]any{
		"policy": policy,
	})
}

// handleGetPolicy retrieves a specific policy.
func (s *Server) handleGetPolicy(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, "invalid policy ID")
		return
	}

	policy, err := s.policyService.GetByID(id)
	if err != nil {
		if errors.Is(err, services.ErrPolicyNotFound) {
			s.respondError(w, http.StatusNotFound, "policy not found")
		} else {
			s.logger.Error("failed to get policy", "id", id, "error", err)
			s.respondError(w, http.StatusInternalServerError, "failed to get policy")
		}
		return
	}

	s.respondJSON(w, http.StatusOK, map[string]any{
		"policy": policy,
	})
}

// handleUpdatePolicy updates a backup policy.
func (s *Server) handleUpdatePolicy(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, "invalid policy ID")
		return
	}

	var input models.CreatePolicyInput
	if err := s.decodeJSON(w, r, &input, 4096); err != nil {
		s.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	policy, err := s.policyService.Update(id, input)
	if err != nil {
		if errors.Is(err, services.ErrPolicyNotFound) {
			s.respondError(w, http.StatusNotFound, "policy not found")
		} else {
			s.logger.Error("failed to update policy", "id", id, "error", err)
			s.respondError(w, http.StatusInternalServerError, "failed to update policy")
		}
		return
	}

	s.respondJSON(w, http.StatusOK, map[string]any{
		"policy": policy,
	})
}

// handleDeletePolicy deletes a backup policy.
func (s *Server) handleDeletePolicy(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, "invalid policy ID")
		return
	}

	if err := s.policyService.Delete(id); err != nil {
		if errors.Is(err, services.ErrPolicyNotFound) {
			s.respondError(w, http.StatusNotFound, "policy not found")
		} else if errors.Is(err, services.ErrPolicyInUse) {
			s.respondError(w, http.StatusConflict, "policy is in use by jobs")
		} else {
			s.logger.Error("failed to delete policy", "id", id, "error", err)
			s.respondError(w, http.StatusInternalServerError, "failed to delete policy")
		}
		return
	}

	s.respondJSON(w, http.StatusOK, map[string]string{
		"message": "policy deleted successfully",
	})
}
