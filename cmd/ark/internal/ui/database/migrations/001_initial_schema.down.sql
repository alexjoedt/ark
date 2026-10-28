-- Rollback initial schema

DROP TRIGGER IF EXISTS update_jobs_timestamp;
DROP TRIGGER IF EXISTS update_policies_timestamp;
DROP TRIGGER IF EXISTS update_users_timestamp;

DROP INDEX IF EXISTS idx_history_status;
DROP INDEX IF EXISTS idx_history_started;
DROP INDEX IF EXISTS idx_history_job;
DROP TABLE IF EXISTS job_history;

DROP INDEX IF EXISTS idx_jobs_policy;
DROP INDEX IF EXISTS idx_jobs_next_run;
DROP INDEX IF EXISTS idx_jobs_enabled;
DROP INDEX IF EXISTS idx_jobs_volume;
DROP TABLE IF EXISTS backup_jobs;

DROP INDEX IF EXISTS idx_policies_name;
DROP TABLE IF EXISTS backup_policies;

DROP INDEX IF EXISTS idx_sessions_expiry;
DROP TABLE IF EXISTS sessions;

DROP INDEX IF EXISTS idx_users_role;
DROP INDEX IF EXISTS idx_users_username;
DROP TABLE IF EXISTS users;
