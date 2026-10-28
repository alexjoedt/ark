// API Client for arkUI - Axios-based implementation
import { get, post, put, del } from './api/client';
import { getSessionManager } from './auth/session-manager';
import type { AxiosRequestConfig } from 'axios';
import type {
	User,
	CreateUserInput,
	Volume,
	Snapshot,
	BackupJob,
	CreateJobInput,
	BackupPolicy,
	CreatePolicyInput,
	JobHistory,
	EngineStatus
} from './types';

// Re-export API_HOST for backward compatibility
export const API_HOST = import.meta.env.VITE_API_URL || '';
const API_BASE = `${API_HOST}/api`;

class APIClient {
	// Auth endpoints
	async login(username: string, password: string): Promise<User> {
		const user = await post<User>('/auth/login', { username, password });
		// Fetch and cache the CSRF token immediately after login so that
		// subsequent mutating requests can include the X-CSRF-Token header.
		await this.fetchCsrfToken();
		return user;
	}

	async logout(): Promise<void> {
		await post<void>('/auth/logout');
		getSessionManager().clearSession();
	}

	async getMe(): Promise<User> {
		return get<User>('/auth/me');
	}

	/**
	 * Fetches a CSRF token from the server and stores it in the session manager.
	 * Must be called once after login (or on page load when already authenticated)
	 * before any state-changing request is made.
	 */
	async fetchCsrfToken(): Promise<void> {
		try {
			const result = await get<{ csrf_token: string }>('/auth/csrf');
			getSessionManager().setCsrfToken(result.csrf_token);
		} catch (error) {
			console.error('[API] Failed to fetch CSRF token:', error);
		}
	}

	// User management
	async listUsers(): Promise<User[]> {
		const result = await get<{users: User[]}>('/users');
		return result.users || [];
	}

	async createUser(input: CreateUserInput): Promise<User> {
		return post<User>('/users', input);
	}

	async deleteUser(id: number): Promise<void> {
		await del<void>(`/users/${id}`);
	}

	async updatePassword(id: number, newPassword: string): Promise<void> {
		await put<void>(`/users/${id}/password`, { password: newPassword });
	}

	// Engine status
	async getStatus(): Promise<EngineStatus> {
		const res =  get<EngineStatus>('/status');
		console.log(res);
		return res
	}

	// Volumes
	async listVolumes(): Promise<Volume[]> {
		const result = await get<Volume[]>('/volumes');
		return Array.isArray(result) ? result : [];
	}

	// Snapshots
	async listSnapshots(): Promise<Snapshot[]> {
		const result = await get<Snapshot[]>('/snapshots');
		return Array.isArray(result) ? result : [];
	}

	async getSnapshot(id: string): Promise<Snapshot> {
		return get<Snapshot>(`/snapshots/${encodeURIComponent(id)}`);
	}

	async deleteSnapshot(id: string): Promise<void> {
		await del<void>(`/snapshots/${encodeURIComponent(id)}`);
	}

	exportSnapshotUrl(id: string): string {
		return `${API_BASE}/snapshots/${encodeURIComponent(id)}/export`;
	}

	async importSnapshot(
		formData: FormData,
		onProgress?: (progress: number) => void
	): Promise<void> {
		const config: AxiosRequestConfig = {
			headers: {
				'Content-Type': 'multipart/form-data'
			}
		};

		if (onProgress) {
			config.onUploadProgress = (progressEvent) => {
				if (progressEvent.total) {
					const progress = (progressEvent.loaded / progressEvent.total) * 100;
					onProgress(progress);
				}
			};
		}

		await post<void>('/import', formData, config);
	}

	// Backup operations
	async backup(volumeName: string): Promise<void> {
		await post<void>('/backup', { volume_name: volumeName });
	}

	async restore(snapshotId: string, targetVolume: string, force: boolean = false): Promise<void> {
		await post<void>('/restore', {
			snapshot_id: snapshotId,
			target_volume: targetVolume,
			force
		});
	}

	// Jobs
	async listJobs(): Promise<BackupJob[]> {
		const result = await get<{jobs: BackupJob[]}>('/jobs');
		return result.jobs || [];
	}

	async getJob(id: number): Promise<BackupJob> {
		const result = await get<{job: BackupJob}>(`/jobs/${id}`);
		return result.job;
	}

	async createJob(input: CreateJobInput): Promise<BackupJob> {
		const result = await post<{job: BackupJob}>('/jobs', input);
		return result.job;
	}

	async updateJob(id: number, input: Partial<CreateJobInput>): Promise<BackupJob> {
		const result = await put<{job: BackupJob}>(`/jobs/${id}`, input);
		return result.job;
	}

	async deleteJob(id: number): Promise<void> {
		await del<void>(`/jobs/${id}`);
	}

	async toggleJob(id: number, enabled: boolean): Promise<BackupJob> {
		return this.updateJob(id, { enabled });
	}

	async runJobNow(id: number): Promise<void> {
		await post<void>(`/jobs/${id}/run`);
	}

	async getJobHistory(id: number): Promise<JobHistory[]> {
		const result = await get<JobHistory[]>(`/jobs/${id}/history`);
		return Array.isArray(result) ? result : [];
	}

	async listRecentHistory(limit: number = 50): Promise<JobHistory[]> {
		const result = await get<JobHistory[]>(`/history?limit=${limit}`);
		return Array.isArray(result) ? result : [];
	}

	// Policies
	async listPolicies(): Promise<BackupPolicy[]> {
		const result = await get<{policies: BackupPolicy[]}>('/policies');
		return result.policies || [];
	}

	async getPolicy(id: number): Promise<BackupPolicy> {
		const result = await get<{policy: BackupPolicy}>(`/policies/${id}`);
		return result.policy;
	}

	async createPolicy(input: CreatePolicyInput): Promise<BackupPolicy> {
		const result = await post<{policy: BackupPolicy}>('/policies', input);
		return result.policy;
	}

	async updatePolicy(id: number, input: Partial<CreatePolicyInput>): Promise<BackupPolicy> {
		const result = await put<{policy: BackupPolicy}>(`/policies/${id}`, input);
		return result.policy;
	}

	async deletePolicy(id: number): Promise<void> {
		await del<void>(`/policies/${id}`);
	}
}

export const api = new APIClient();
