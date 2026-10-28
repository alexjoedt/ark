// API Types for arkUI

// User types
export interface User {
	id: number;
	username: string;
	email: string;
	role: 'admin' | 'user';
	created_at: string;
}

export interface CreateUserInput {
	username: string;
	email: string;
	password: string;
	role: 'admin' | 'user';
}

// Volume types
export interface Volume {
	name: string;
	driver: string;
	mountpoint: string;
	created_at: string;
	size?: number;
}

// Snapshot types (formerly Archive)
export interface Snapshot {
	id: string;
	volume: string;
	source_type: string;
	created_at: string;
	size_bytes: number;
	stored_bytes: number;
	comment?: string;
	file_count: number;
}

// Job types
export interface BackupJob {
	id: number;
	name: string;
	description?: string;
	volume_name: string;
	schedule: string;
	policy_id?: number;
	policy?: BackupPolicy;
	enabled: boolean;
	next_run?: string;
	last_run?: string;
	last_status?: 'running' | 'success' | 'failed';
	created_at: string;
	updated_at: string;
}

export interface CreateJobInput {
	name: string;
	volume_name: string;
	schedule: string;
	policy_id?: number;
	enabled: boolean;
}

// Policy types
export interface BackupPolicy {
	id: number;
	name: string;
	description?: string;
	keep_days?: number;
	keep_count?: number;
	created_at: string;
	updated_at: string;
}

export interface CreatePolicyInput {
	name: string;
	description?: string;
	keep_days?: number;
	keep_count?: number;
}

// Job History types
export interface JobHistory {
	id: number;
	job_id: number;
	job?: BackupJob;
	started_at: string;
	completed_at?: string;
	status: 'running' | 'success' | 'failed';
	snapshot_id?: string;
	error_message?: string;
}

// Status types
export interface EngineStatus {
	status: string;
	version: string;
	repo_dir: string;
}

// WebSocket Event types
export interface BaseWebSocketEvent {
	type: string;
	timestamp: string;
}

export interface BackupStartedEvent extends BaseWebSocketEvent {
	type: 'backup.started';
	data: {
		volume: string;
		job_id?: number;
		job_name?: string;
	};
}

export interface BackupProgressEvent extends BaseWebSocketEvent {
	type: 'backup.progress';
	data: {
		volume: string;
		progress: number;
		status: string;
		message?: string;
		bytes_processed?: number;
		total_bytes?: number;
	};
}

export interface BackupCompletedEvent extends BaseWebSocketEvent {
	type: 'backup.completed';
	data: {
		volume: string;
		snapshot_id: string;
		size_bytes: number;
		duration_seconds: number;
		job_id?: number;
	};
}

export interface BackupFailedEvent extends BaseWebSocketEvent {
	type: 'backup.failed';
	data: {
		volume: string;
		error: string;
		job_id?: number;
	};
}

export interface RestoreStartedEvent extends BaseWebSocketEvent {
	type: 'restore.started';
	data: {
		volume: string;
		snapshot_id: string;
	};
}

export interface RestoreProgressEvent extends BaseWebSocketEvent {
	type: 'restore.progress';
	data: {
		volume: string;
		snapshot_id: string;
		progress: number;
		status: string;
		message?: string;
	};
}

export interface RestoreCompletedEvent extends BaseWebSocketEvent {
	type: 'restore.completed';
	data: {
		volume: string;
		snapshot_id: string;
		duration_seconds: number;
	};
}

export interface RestoreFailedEvent extends BaseWebSocketEvent {
	type: 'restore.failed';
	data: {
		volume: string;
		snapshot_id: string;
		error: string;
	};
}

export interface JobScheduledEvent extends BaseWebSocketEvent {
	type: 'job.scheduled';
	data: {
		job_id: number;
		job_name: string;
		next_run: string;
	};
}

export interface SystemStatusEvent extends BaseWebSocketEvent {
	type: 'system.status';
	data: {
		status: string;
		message?: string;
	};
}

// Discriminated union of all WebSocket events
export type WebSocketEvent =
	| BackupStartedEvent
	| BackupProgressEvent
	| BackupCompletedEvent
	| BackupFailedEvent
	| RestoreStartedEvent
	| RestoreProgressEvent
	| RestoreCompletedEvent
	| RestoreFailedEvent
	| JobScheduledEvent
	| SystemStatusEvent
	| BaseWebSocketEvent; // Fallback for unknown events

export interface BackupProgress {
	volume: string;
	progress: number;
	status: string;
	message?: string;
}

export interface RestoreProgress {
	volume: string;
	snapshot_id: string;
	progress: number;
	status: string;
	message?: string;
}

// API Response types
export interface APIError {
	error: string;
	details?: string;
}

export interface APIResponse<T> {
	data?: T;
	error?: string;
}
