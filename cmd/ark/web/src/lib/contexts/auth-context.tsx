import { createContext, useContext, useState, useCallback, useEffect, type ReactNode } from 'react';
import { api } from '../api';
import { getSessionManager } from '../auth/session-manager';
import type { User } from '../types';

interface AuthContextValue {
  user: User | null;
  isAuthenticated: boolean;
  login: (username: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
  setUser: (user: User | null) => void;
}

const AuthContext = createContext<AuthContextValue | undefined>(undefined);

interface AuthProviderProps {
  children: ReactNode;
  initialUser?: User | null;
}

/**
 * AuthProvider - Manages authentication state
 * 
 * In this loader-first architecture:
 * 1. Receives initial user data from root loader
 * 2. Provides login/logout actions that update both API and local state
 * 3. Uses session manager to track auth state for loaders
 */
export function AuthProvider({ children, initialUser = null }: AuthProviderProps) {
  const [user, setUser] = useState<User | null>(initialUser);
  const sessionManager = getSessionManager();

  // Sync session manager with initial user state
  useEffect(() => {
    if (initialUser) {
      sessionManager.setAuthenticated();
    }
  }, [initialUser, sessionManager]);

  const login = useCallback(async (username: string, password: string) => {
    try {
      const loggedInUser = await api.login(username, password);
      sessionManager.setAuthenticated();
      setUser(loggedInUser);
    } catch (error) {
      // Clear session state on login failure
      sessionManager.clearSession();
      setUser(null);
      throw error;
    }
  }, [sessionManager]);

  const logout = useCallback(async () => {
    try {
      await api.logout();
    } catch (error) {
      console.error('Logout API call failed:', error);
      // Continue with logout even if API call fails
    } finally {
      // Always clear local state and session
      sessionManager.clearSession();
      setUser(null);
    }
  }, [sessionManager]);

  const value: AuthContextValue = {
    user,
    isAuthenticated: !!user,
    login,
    logout,
    setUser,
  };

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const context = useContext(AuthContext);
  if (!context) {
    throw new Error('useAuth must be used within AuthProvider');
  }
  return context;
}
