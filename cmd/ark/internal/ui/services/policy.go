package services

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/alexjoedt/ark/cmd/ark/internal/ui/models"
)

// Policy-related errors
var (
	ErrPolicyNotFound = errors.New("policy not found")
	ErrPolicyExists   = errors.New("policy already exists")
	ErrPolicyInUse    = errors.New("policy is in use by one or more jobs")
)

// PolicyService provides backup policy management operations.
type PolicyService struct {
	db *sql.DB
}

// NewPolicyService creates a new PolicyService instance.
func NewPolicyService(db *sql.DB) *PolicyService {
	return &PolicyService{db: db}
}

// Create creates a new backup policy.
func (s *PolicyService) Create(input models.CreatePolicyInput) (*models.BackupPolicy, error) {
	now := time.Now().Unix()
	result, err := s.db.Exec(`
		INSERT INTO backup_policies (name, description, keep_count, keep_days, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, input.Name, input.Description, input.KeepCount, input.KeepDays, now, now)
	if err != nil {
		if isUniqueConstraintError(err) {
			return nil, ErrPolicyExists
		}
		return nil, fmt.Errorf("failed to create policy: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("failed to get policy ID: %w", err)
	}

	policy := &models.BackupPolicy{
		ID:          id,
		Name:        input.Name,
		Description: input.Description,
		KeepCount:   input.KeepCount,
		KeepDays:    input.KeepDays,
		CreatedAt:   time.Unix(now, 0),
		UpdatedAt:   time.Unix(now, 0),
	}

	return policy, nil
}

// GetByID retrieves a policy by ID.
func (s *PolicyService) GetByID(id int64) (*models.BackupPolicy, error) {
	var policy models.BackupPolicy
	var createdAt, updatedAt int64
	var keepCount, keepDays sql.NullInt64

	err := s.db.QueryRow(`
		SELECT id, name, description, keep_count, keep_days, created_at, updated_at
		FROM backup_policies
		WHERE id = ?
	`, id).Scan(&policy.ID, &policy.Name, &policy.Description, &keepCount, &keepDays, &createdAt, &updatedAt)

	if err == sql.ErrNoRows {
		return nil, ErrPolicyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query policy: %w", err)
	}

	policy.CreatedAt = time.Unix(createdAt, 0)
	policy.UpdatedAt = time.Unix(updatedAt, 0)

	if keepCount.Valid {
		count := int(keepCount.Int64)
		policy.KeepCount = &count
	}
	if keepDays.Valid {
		days := int(keepDays.Int64)
		policy.KeepDays = &days
	}

	return &policy, nil
}

// GetByName retrieves a policy by name.
func (s *PolicyService) GetByName(name string) (*models.BackupPolicy, error) {
	var policy models.BackupPolicy
	var createdAt, updatedAt int64
	var keepCount, keepDays sql.NullInt64

	err := s.db.QueryRow(`
		SELECT id, name, description, keep_count, keep_days, created_at, updated_at
		FROM backup_policies
		WHERE name = ?
	`, name).Scan(&policy.ID, &policy.Name, &policy.Description, &keepCount, &keepDays, &createdAt, &updatedAt)

	if err == sql.ErrNoRows {
		return nil, ErrPolicyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query policy: %w", err)
	}

	policy.CreatedAt = time.Unix(createdAt, 0)
	policy.UpdatedAt = time.Unix(updatedAt, 0)

	if keepCount.Valid {
		count := int(keepCount.Int64)
		policy.KeepCount = &count
	}
	if keepDays.Valid {
		days := int(keepDays.Int64)
		policy.KeepDays = &days
	}

	return &policy, nil
}

// List returns all backup policies.
func (s *PolicyService) List() ([]*models.BackupPolicy, error) {
	rows, err := s.db.Query(`
		SELECT id, name, description, keep_count, keep_days, created_at, updated_at
		FROM backup_policies
		ORDER BY name
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to query policies: %w", err)
	}
	defer rows.Close()

	var policies []*models.BackupPolicy
	for rows.Next() {
		var policy models.BackupPolicy
		var createdAt, updatedAt int64
		var keepCount, keepDays sql.NullInt64

		err := rows.Scan(&policy.ID, &policy.Name, &policy.Description, &keepCount, &keepDays, &createdAt, &updatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan policy: %w", err)
		}

		policy.CreatedAt = time.Unix(createdAt, 0)
		policy.UpdatedAt = time.Unix(updatedAt, 0)

		if keepCount.Valid {
			count := int(keepCount.Int64)
			policy.KeepCount = &count
		}
		if keepDays.Valid {
			days := int(keepDays.Int64)
			policy.KeepDays = &days
		}

		policies = append(policies, &policy)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating policies: %w", err)
	}

	return policies, nil
}

// Update updates a backup policy.
func (s *PolicyService) Update(id int64, input models.CreatePolicyInput) (*models.BackupPolicy, error) {
	now := time.Now().Unix()
	result, err := s.db.Exec(`
		UPDATE backup_policies
		SET description = ?, keep_count = ?, keep_days = ?, updated_at = ?
		WHERE id = ?
	`, input.Description, input.KeepCount, input.KeepDays, now, id)
	if err != nil {
		return nil, fmt.Errorf("failed to update policy: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return nil, ErrPolicyNotFound
	}

	return s.GetByID(id)
}

// Delete deletes a backup policy by ID.
// Returns ErrPolicyInUse if the policy is referenced by any jobs.
func (s *PolicyService) Delete(id int64) error {
	// Check if policy is in use
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM backup_jobs WHERE policy_id = ?", id).Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to check policy usage: %w", err)
	}

	if count > 0 {
		return ErrPolicyInUse
	}

	// Delete policy
	result, err := s.db.Exec("DELETE FROM backup_policies WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("failed to delete policy: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return ErrPolicyNotFound
	}

	return nil
}
