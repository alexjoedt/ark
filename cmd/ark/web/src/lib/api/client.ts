// Axios-based API client with interceptors and error handling
import axios, { type AxiosInstance, type AxiosRequestConfig, type AxiosResponse } from 'axios';
import { parseAPIError } from './errors';
import { getSessionManager } from '../auth/session-manager';

// Use VITE_API_URL for development (e.g., http://localhost:8080)
// In production, this will be empty and use relative URLs
export const API_HOST = import.meta.env.VITE_API_URL || '';
const API_BASE = `${API_HOST}/api`;

/**
 * Create configured axios instance
 */
export function createAPIClient(): AxiosInstance {
	const client = axios.create({
		baseURL: API_BASE,
		timeout: 30000, // 30 seconds
		headers: {
			'Content-Type': 'application/json'
		},
		withCredentials: true // Include cookies for session auth
	});

	// Request interceptor - add CSRF token and request tracking
	client.interceptors.request.use(
		(config) => {
			// Add timestamp for performance tracking
			(config as any).requestStartTime = Date.now();

			// Inject CSRF token for state-changing requests
			const safeMethods = ['get', 'head', 'options', 'trace'];
			if (config.method && !safeMethods.includes(config.method.toLowerCase())) {
				const csrfToken = getSessionManager().getCsrfToken();
				if (csrfToken) {
					config.headers['X-CSRF-Token'] = csrfToken;
				}
			}

			return config;
		},
		(error) => {
			console.error('[API] Request error:', error);
			return Promise.reject(error);
		}
	);

	// Response interceptor - handle errors
	client.interceptors.response.use(
		(response: AxiosResponse) => response,
		(error) => {
			// Parse error into typed API error
			const apiError = parseAPIError(error);
			
			// Handle 401 globally - clear session state
			if (error.response?.status === 401) {
				const sessionManager = getSessionManager();
				sessionManager.clearSession();
				
				// DON'T redirect here - let React Router loaders handle navigation
				// This prevents conflicts with loader redirects and error boundaries
				console.log('[API] Session invalidated (401), loaders will handle redirect');
			}
			
			// Log detailed error info
			if (error.response) {
				console.error(
					`[API] ${error.response.status} ${error.config?.method?.toUpperCase()} ${error.config?.url}:`,
					apiError.message
				);
			} else {
				console.error('[API] Network error:', apiError.message);
			}
			
			return Promise.reject(apiError);
		}
	);

	return client;
}

/**
 * Global API client instance
 */
export const apiClient = createAPIClient();

/**
 * Create a cancellable request with AbortController
 */
export function createCancellableRequest<T>(
	requestFn: (signal: AbortSignal) => Promise<T>
): {
	promise: Promise<T>;
	cancel: () => void;
} {
	const controller = new AbortController();
	
	return {
		promise: requestFn(controller.signal),
		cancel: () => controller.abort()
	};
}

/**
 * Helper to make GET request
 */
export async function get<T>(url: string, config?: AxiosRequestConfig): Promise<T> {
	const response = await apiClient.get<T>(url, config);
	return response.data;
}

/**
 * Helper to make POST request
 */
export async function post<T>(url: string, data?: any, config?: AxiosRequestConfig): Promise<T> {
	const response = await apiClient.post<T>(url, data, config);
	return response.data;
}

/**
 * Helper to make PUT request
 */
export async function put<T>(url: string, data?: any, config?: AxiosRequestConfig): Promise<T> {
	const response = await apiClient.put<T>(url, data, config);
	return response.data;
}

/**
 * Helper to make DELETE request
 */
export async function del<T>(url: string, config?: AxiosRequestConfig): Promise<T> {
	const response = await apiClient.delete<T>(url, config);
	return response.data;
}
