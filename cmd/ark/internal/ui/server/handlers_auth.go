package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/alexjoedt/ark/cmd/ark/internal/ui/models"
	"github.com/alexjoedt/ark/cmd/ark/internal/ui/services"
	"github.com/go-chi/chi/v5"
)

// handleLogin authenticates a user and creates a session.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}

	if err := s.decodeJSON(w, r, &input, 1024); err != nil {
		s.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Authenticate user
	user, err := s.userService.Authenticate(input.Username, input.Password)
	if err != nil {
		if errors.Is(err, services.ErrInvalidCredentials) {
			s.respondError(w, http.StatusUnauthorized, "invalid credentials")
		} else {
			s.logger.Error("login failed", "error", err)
			s.respondError(w, http.StatusInternalServerError, "login failed")
		}
		return
	}

	// Create session
	if err := s.sessionManager.RenewToken(r.Context()); err != nil {
		s.logger.Error("failed to renew session token", "error", err)
		s.respondError(w, http.StatusInternalServerError, "session creation failed")
		return
	}

	s.sessionManager.Put(r.Context(), "user_id", user.ID)
	s.sessionManager.Put(r.Context(), "username", user.Username)

	s.logger.Info("user logged in", "user_id", user.ID, "username", user.Username)

	s.respondJSON(w, http.StatusOK, user)
}

// handleLogout destroys the user's session.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.sessionManager.Destroy(r.Context()); err != nil {
		s.logger.Error("failed to destroy session", "error", err)
	}

	s.respondJSON(w, http.StatusOK, map[string]string{
		"message": "logged out successfully",
	})
}

// handleGetMe returns the current user's information.
func (s *Server) handleGetMe(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	if user == nil {
		s.respondError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	s.respondJSON(w, http.StatusOK, user)
}

// handleCSRF issues (or reuses) a CSRF token for the current session.
// Clients must call this endpoint after login and include the returned token
// in the X-CSRF-Token header for every state-changing request.
func (s *Server) handleCSRF(w http.ResponseWriter, r *http.Request) {
	// Reuse existing token if one is already in the session.
	token := s.sessionManager.GetString(r.Context(), "csrf_token")
	if token == "" {
		var err error
		token, err = generateCSRFToken()
		if err != nil {
			s.logger.Error("failed to generate CSRF token", "error", err)
			s.respondError(w, http.StatusInternalServerError, "failed to generate CSRF token")
			return
		}
		s.sessionManager.Put(r.Context(), "csrf_token", token)
	}

	s.respondJSON(w, http.StatusOK, map[string]string{
		"csrf_token": token,
	})
}

// User management handlers (admin only)

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.userService.List()
	if err != nil {
		s.logger.Error("failed to list users", "error", err)
		s.respondError(w, http.StatusInternalServerError, "failed to list users")
		return
	}

	s.respondJSON(w, http.StatusOK, map[string]any{
		"users": users,
	})
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var input models.CreateUserInput
	if err := s.decodeJSON(w, r, &input, 4096); err != nil {
		s.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	user, err := s.userService.Create(input)
	if err != nil {
		switch {
		case errors.Is(err, services.ErrUserExists):
			s.respondError(w, http.StatusConflict, "user already exists")
		case errors.Is(err, services.ErrInvalidRole):
			s.respondError(w, http.StatusBadRequest, "invalid role")
		case errors.Is(err, services.ErrWeakPassword):
			s.respondError(w, http.StatusBadRequest, err.Error())
		default:
			s.logger.Error("failed to create user", "error", err)
			s.respondError(w, http.StatusInternalServerError, "failed to create user")
		}
		return
	}

	s.respondJSON(w, http.StatusCreated, map[string]any{
		"user": user,
	})
}

func (s *Server) handleGetUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, "invalid user ID")
		return
	}

	user, err := s.userService.GetByID(id)
	if err != nil {
		if errors.Is(err, services.ErrUserNotFound) {
			s.respondError(w, http.StatusNotFound, "user not found")
		} else {
			s.logger.Error("failed to get user", "id", id, "error", err)
			s.respondError(w, http.StatusInternalServerError, "failed to get user")
		}
		return
	}

	s.respondJSON(w, http.StatusOK, user)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, "invalid user ID")
		return
	}

	// Prevent self-deletion
	currentUser := userFromContext(r.Context())
	if currentUser != nil && currentUser.ID == id {
		s.respondError(w, http.StatusBadRequest, "cannot delete your own account")
		return
	}

	if err := s.userService.Delete(id); err != nil {
		if errors.Is(err, services.ErrUserNotFound) {
			s.respondError(w, http.StatusNotFound, "user not found")
		} else {
			s.logger.Error("failed to delete user", "id", id, "error", err)
			s.respondError(w, http.StatusInternalServerError, "failed to delete user")
		}
		return
	}

	s.respondJSON(w, http.StatusOK, map[string]string{
		"message": "user deleted successfully",
	})
}

func (s *Server) handleUpdatePassword(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, "invalid user ID")
		return
	}

	var input struct {
		Password string `json:"password"`
	}
	if err := s.decodeJSON(w, r, &input, 4096); err != nil {
		s.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if len(input.Password) < 8 {
		s.respondError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}

	if err := s.userService.UpdatePassword(id, input.Password); err != nil {
		if errors.Is(err, services.ErrUserNotFound) {
			s.respondError(w, http.StatusNotFound, "user not found")
		} else {
			s.logger.Error("failed to update password", "id", id, "error", err)
			s.respondError(w, http.StatusInternalServerError, "failed to update password")
		}
		return
	}

	s.respondJSON(w, http.StatusOK, map[string]string{
		"message": "password updated successfully",
	})
}
