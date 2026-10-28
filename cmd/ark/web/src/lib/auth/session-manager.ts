/**
 * Session Manager - Client-side session state tracker
 * 
 * Note: Actual session is stored in HTTP-only cookies by the backend.
 * This manager only tracks whether we believe the user is authenticated
 * to avoid unnecessary API calls and handle auth state in loaders.
 */

interface SessionState {
  isAuthenticated: boolean;
  lastValidated: number | null;
  csrfToken: string | null;
}

const SESSION_KEY = 'ark_session_state';
const VALIDATION_TIMEOUT = 5 * 60 * 1000; // 5 minutes

class SessionManager {
  private state: SessionState;

  constructor() {
    this.state = this.loadState();
  }

  /**
   * Load session state from sessionStorage
   * Uses sessionStorage (not localStorage) so it clears on browser close
   */
  private loadState(): SessionState {
    try {
      const stored = sessionStorage.getItem(SESSION_KEY);
      if (stored) {
        const parsed = JSON.parse(stored);
        return {
          isAuthenticated: parsed.isAuthenticated || false,
          lastValidated: parsed.lastValidated || null,
          csrfToken: parsed.csrfToken || null,
        };
      }
    } catch (error) {
      console.error('[SessionManager] Failed to load session state:', error);
    }

    return {
      isAuthenticated: false,
      lastValidated: null,
      csrfToken: null,
    };
  }

  /**
   * Save session state to sessionStorage
   */
  private saveState(): void {
    try {
      sessionStorage.setItem(SESSION_KEY, JSON.stringify(this.state));
    } catch (error) {
      console.error('[SessionManager] Failed to save session state:', error);
    }
  }

  /**
   * Mark session as authenticated
   * Call this after successful login or session validation
   */
  setAuthenticated(): void {
    this.state = {
      isAuthenticated: true,
      lastValidated: Date.now(),
      csrfToken: this.state.csrfToken,
    };
    this.saveState();
  }

  /**
   * Clear session state
   * Call this on logout or 401 responses
   */
  clearSession(): void {
    this.state = {
      isAuthenticated: false,
      lastValidated: null,
      csrfToken: null,
    };
    this.saveState();
  }

  /**
   * Store the CSRF token received from the server.
   * Call this after GET /api/auth/csrf.
   */
  setCsrfToken(token: string): void {
    this.state.csrfToken = token;
    this.saveState();
  }

  /**
   * Return the stored CSRF token, or null if not yet fetched.
   */
  getCsrfToken(): string | null {
    return this.state.csrfToken;
  }

  /**
   * Check if we believe the user is authenticated
   * This is a best-guess based on recent validation, not authoritative
   */
  isAuthenticated(): boolean {
    return this.state.isAuthenticated;
  }

  /**
   * Check if session needs revalidation
   * Returns true if never validated or last validation was too long ago
   */
  needsValidation(): boolean {
    if (!this.state.isAuthenticated) {
      return false; // Not authenticated, no need to validate
    }

    if (this.state.lastValidated === null) {
      return true; // Authenticated but never validated
    }

    const timeSinceValidation = Date.now() - this.state.lastValidated;
    return timeSinceValidation > VALIDATION_TIMEOUT;
  }

  /**
   * Update last validated timestamp
   * Call after successful API calls that verify the session is still valid
   */
  updateLastValidated(): void {
    if (this.state.isAuthenticated) {
      this.state.lastValidated = Date.now();
      this.saveState();
    }
  }
}

// Global singleton instance
let sessionManager: SessionManager | null = null;

/**
 * Get the global session manager instance
 */
export function getSessionManager(): SessionManager {
  if (!sessionManager) {
    sessionManager = new SessionManager();
  }
  return sessionManager;
}
