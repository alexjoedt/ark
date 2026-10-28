// Centralized error handling for API requests

/**
 * Base API Error class
 */
export class APIError extends Error {
	statusCode?: number;
	response?: any;

	constructor(
		message: string,
		statusCode?: number,
		response?: any
	) {
		super(message);
		this.name = 'APIError';
		this.statusCode = statusCode;
		this.response = response;
	}
}

/**
 * Authentication error (401)
 */
export class AuthenticationError extends APIError {
	constructor(message: string = 'Authentication required', response?: any) {
		super(message, 401, response);
		this.name = 'AuthenticationError';
	}
}

/**
 * Authorization error (403)
 */
export class AuthorizationError extends APIError {
	constructor(message: string = 'Insufficient permissions', response?: any) {
		super(message, 403, response);
		this.name = 'AuthorizationError';
	}
}

/**
 * Not found error (404)
 */
export class NotFoundError extends APIError {
	constructor(message: string = 'Resource not found', response?: any) {
		super(message, 404, response);
		this.name = 'NotFoundError';
	}
}

/**
 * Validation error (400, 422)
 */
export class ValidationError extends APIError {
	errors?: Record<string, string[]>;

	constructor(
		message: string = 'Validation failed',
		errors?: Record<string, string[]>,
		response?: any
	) {
		super(message, 400, response);
		this.name = 'ValidationError';
		this.errors = errors;
	}
}

/**
 * Network error (no response from server)
 */
export class NetworkError extends APIError {
	originalError?: Error;

	constructor(message: string = 'Network error', originalError?: Error) {
		super(message);
		this.name = 'NetworkError';
		this.originalError = originalError;
	}
}

/**
 * Request timeout error
 */
export class TimeoutError extends APIError {
	constructor(message: string = 'Request timeout') {
		super(message);
		this.name = 'TimeoutError';
	}
}

/**
 * Parse API error from axios error
 */
export function parseAPIError(error: any): APIError {
	// Network errors (no response)
	if (!error.response) {
		if (error.code === 'ECONNABORTED') {
			return new TimeoutError('Request timeout');
		}
		return new NetworkError('Network error - could not reach server', error);
	}

	const status = error.response.status;
	const data = error.response.data;
	
	// Extract message from nested error object or fallback to string error/message
	let message: string;
	if (typeof data?.error === 'object' && data.error?.message) {
		message = data.error.message;
	} else if (typeof data?.error === 'string') {
		message = data.error;
	} else if (data?.message) {
		message = data.message;
	} else {
		message = error.message || 'An error occurred';
	}

	// Map status codes to specific error types
	switch (status) {
		case 401:
			return new AuthenticationError(message, data);
		case 403:
			return new AuthorizationError(message, data);
		case 404:
			return new NotFoundError(message, data);
		case 400:
		case 422:
			return new ValidationError(message, data?.errors, data);
		default:
			return new APIError(message, status, data);
	}
}

/**
 * Get user-friendly error message
 */
export function getErrorMessage(error: unknown): string {
	if (error instanceof APIError) {
		return error.message;
	}
	if (error instanceof Error) {
		return error.message;
	}
	return 'An unexpected error occurred';
}
