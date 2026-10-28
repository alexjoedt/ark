// Package services provides business logic for arkUI.
package services

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/alexjoedt/ark/cmd/ark/internal/ui/models"
	"golang.org/x/crypto/bcrypt"
)

// Common errors
var (
	ErrUserNotFound       = errors.New("user not found")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrUserExists         = errors.New("user already exists")
	ErrInvalidRole        = errors.New("invalid role")
	// ErrWeakPassword is returned when a supplied password is too short.
	// Minimum length mirrors the frontend Zod schema (8 characters).
	ErrWeakPassword = errors.New("password must be at least 8 characters")
)

// minPasswordLen is the minimum allowed password length.
const minPasswordLen = 8

// UserService provides user management operations.
// It's a deep module - simple interface, complex implementation hidden.
type UserService struct {
	db *sql.DB
}

// NewUserService creates a new UserService instance.
func NewUserService(db *sql.DB) *UserService {
	return &UserService{db: db}
}

// Create creates a new user with hashed password.
func (s *UserService) Create(input models.CreateUserInput) (*models.User, error) {
	// Validate role
	if input.Role != models.RoleAdmin && input.Role != models.RoleUser {
		return nil, ErrInvalidRole
	}

	// Validate password strength.
	if len(input.Password) < minPasswordLen {
		return nil, ErrWeakPassword
	}

	// Hash password
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("failed to hash password: %w", err)
	}

	now := time.Now().Unix()

	// Insert user
	result, err := s.db.Exec(`
		INSERT INTO users (username, password_hash, email, role, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, input.Username, string(hashedPassword), input.Email, string(input.Role), now, now)
	if err != nil {
		if isUniqueConstraintError(err) {
			return nil, ErrUserExists
		}
		return nil, fmt.Errorf("failed to create user: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("failed to get user ID: %w", err)
	}

	return &models.User{
		ID:        id,
		Username:  input.Username,
		Email:     input.Email,
		Role:      input.Role,
		CreatedAt: time.Unix(now, 0),
		UpdatedAt: time.Unix(now, 0),
	}, nil
}

// Authenticate verifies username and password, returns user if valid.
func (s *UserService) Authenticate(username, password string) (*models.User, error) {
	var user models.User
	var passwordHash string
	var createdAt, updatedAt int64
	var role string

	err := s.db.QueryRow(`
		SELECT id, username, password_hash, email, role, created_at, updated_at
		FROM users
		WHERE username = ?
	`, username).Scan(&user.ID, &user.Username, &passwordHash, &user.Email, &role, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query user: %w", err)
	}

	// Verify password
	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)); err != nil {
		return nil, ErrInvalidCredentials
	}

	user.Role = models.UserRole(role)
	user.CreatedAt = time.Unix(createdAt, 0)
	user.UpdatedAt = time.Unix(updatedAt, 0)

	return &user, nil
}

// GetByID retrieves a user by ID.
func (s *UserService) GetByID(id int64) (*models.User, error) {
	var user models.User
	var createdAt, updatedAt int64
	var role string

	err := s.db.QueryRow(`
		SELECT id, username, email, role, created_at, updated_at
		FROM users
		WHERE id = ?
	`, id).Scan(&user.ID, &user.Username, &user.Email, &role, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query user: %w", err)
	}

	user.Role = models.UserRole(role)
	user.CreatedAt = time.Unix(createdAt, 0)
	user.UpdatedAt = time.Unix(updatedAt, 0)

	return &user, nil
}

// GetByUsername retrieves a user by username.
func (s *UserService) GetByUsername(username string) (*models.User, error) {
	var user models.User
	var createdAt, updatedAt int64
	var role string

	err := s.db.QueryRow(`
		SELECT id, username, email, role, created_at, updated_at
		FROM users
		WHERE username = ?
	`, username).Scan(&user.ID, &user.Username, &user.Email, &role, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query user: %w", err)
	}

	user.Role = models.UserRole(role)
	user.CreatedAt = time.Unix(createdAt, 0)
	user.UpdatedAt = time.Unix(updatedAt, 0)

	return &user, nil
}

// List returns all users.
func (s *UserService) List() ([]*models.User, error) {
	rows, err := s.db.Query(`
		SELECT id, username, email, role, created_at, updated_at
		FROM users
		ORDER BY username
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to query users: %w", err)
	}
	defer rows.Close()

	var users []*models.User
	for rows.Next() {
		var user models.User
		var createdAt, updatedAt int64
		var role string

		if err := rows.Scan(&user.ID, &user.Username, &user.Email, &role, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan user: %w", err)
		}

		user.Role = models.UserRole(role)
		user.CreatedAt = time.Unix(createdAt, 0)
		user.UpdatedAt = time.Unix(updatedAt, 0)
		users = append(users, &user)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating users: %w", err)
	}

	return users, nil
}

// Delete deletes a user by ID.
func (s *UserService) Delete(id int64) error {
	result, err := s.db.Exec("DELETE FROM users WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("failed to delete user: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return ErrUserNotFound
	}

	return nil
}

// UpdatePassword updates a user's password.
func (s *UserService) UpdatePassword(id int64, newPassword string) error {
	// Validate password strength.
	if len(newPassword) < minPasswordLen {
		return ErrWeakPassword
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("failed to hash password: %w", err)
	}

	result, err := s.db.Exec(`
		UPDATE users
		SET password_hash = ?
		WHERE id = ?
	`, string(hashedPassword), id)
	if err != nil {
		return fmt.Errorf("failed to update password: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return ErrUserNotFound
	}

	return nil
}

// EnsureAdminExists creates an admin user if no users exist.
// This is used during initial setup to create the first admin account.
func (s *UserService) EnsureAdminExists(username, password string) error {
	// Check if any users exist
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to count users: %w", err)
	}

	// If users exist, don't create admin
	if count > 0 {
		return nil
	}

	// Create admin user
	_, err = s.Create(models.CreateUserInput{
		Username: username,
		Password: password,
		Role:     models.RoleAdmin,
	})
	if err != nil {
		return fmt.Errorf("failed to create admin user: %w", err)
	}

	return nil
}
