-- Initial schema for arkUI
-- Creates tables for users, sessions, backup jobs, policies, and job history

-- Users table
CREATE TABLE IF NOT EXISTS users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    email TEXT,
    role TEXT NOT NULL DEFAULT 'user' CHECK(role IN ('admin', 'user')),
    created_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now')),
    updated_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now'))
);

CREATE INDEX idx_users_username ON users(username);
CREATE INDEX idx_users_role ON users(role);

-- Sessions table (for scs session manager)
CREATE TABLE IF NOT EXISTS sessions (
    token TEXT PRIMARY KEY,
    data BLOB NOT NULL,
    expiry INTEGER NOT NULL
);

CREATE INDEX idx_sessions_expiry ON sessions(expiry);

-- Backup policies table
CREATE TABLE IF NOT EXISTS backup_policies (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    description TEXT,
    keep_count INTEGER,           -- Keep N most recent backups (NULL = unlimited)
    keep_days INTEGER,             -- Keep backups for N days (NULL = unlimited)
    created_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now')),
    updated_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now'))
);

CREATE INDEX idx_policies_name ON backup_policies(name);

-- Backup jobs table (scheduled backups)
CREATE TABLE IF NOT EXISTS backup_jobs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    description TEXT,
    volume_name TEXT NOT NULL,     -- Docker volume to backup
    schedule TEXT NOT NULL,         -- Cron expression (e.g., "0 2 * * *")
    policy_id INTEGER,              -- Foreign key to backup_policies (NULL = no policy)
    enabled INTEGER NOT NULL DEFAULT 1, -- 0 = disabled, 1 = enabled
    next_run INTEGER,               -- Unix timestamp of next scheduled run
    last_run INTEGER,               -- Unix timestamp of last run
    last_status TEXT,               -- 'success', 'failed', 'running'
    created_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now')),
    updated_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now')),
    FOREIGN KEY (policy_id) REFERENCES backup_policies(id) ON DELETE SET NULL
);

CREATE INDEX idx_jobs_volume ON backup_jobs(volume_name);
CREATE INDEX idx_jobs_enabled ON backup_jobs(enabled);
CREATE INDEX idx_jobs_next_run ON backup_jobs(next_run);
CREATE INDEX idx_jobs_policy ON backup_jobs(policy_id);

-- Job execution history table
CREATE TABLE IF NOT EXISTS job_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id INTEGER NOT NULL,
    started_at INTEGER NOT NULL,
    completed_at INTEGER,
    status TEXT NOT NULL,           -- 'running', 'success', 'failed'
    archive_id TEXT,                -- ID of created archive (if successful)
    error_message TEXT,             -- Error details (if failed)
    FOREIGN KEY (job_id) REFERENCES backup_jobs(id) ON DELETE CASCADE
);

CREATE INDEX idx_history_job ON job_history(job_id);
CREATE INDEX idx_history_started ON job_history(started_at);
CREATE INDEX idx_history_status ON job_history(status);

-- Trigger to update updated_at timestamp on users
CREATE TRIGGER update_users_timestamp 
AFTER UPDATE ON users
FOR EACH ROW
BEGIN
    UPDATE users SET updated_at = strftime('%s', 'now') WHERE id = OLD.id;
END;

-- Trigger to update updated_at timestamp on backup_policies
CREATE TRIGGER update_policies_timestamp 
AFTER UPDATE ON backup_policies
FOR EACH ROW
BEGIN
    UPDATE backup_policies SET updated_at = strftime('%s', 'now') WHERE id = OLD.id;
END;

-- Trigger to update updated_at timestamp on backup_jobs
CREATE TRIGGER update_jobs_timestamp 
AFTER UPDATE ON backup_jobs
FOR EACH ROW
BEGIN
    UPDATE backup_jobs SET updated_at = strftime('%s', 'now') WHERE id = OLD.id;
END;
