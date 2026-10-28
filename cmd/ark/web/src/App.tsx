import { createBrowserRouter, RouterProvider, Navigate, redirect } from 'react-router';
import { ThemeProvider } from 'next-themes';
import { Toaster } from './components/ui/sonner';
import { RootLayout } from './components/layout/root-layout';
import { ErrorPage } from './routes/error';
import { Login } from './routes/login';
import { Dashboard } from './routes/DashboardPage';
import VolumesPage from './routes/volumes/VolumesPage';
import VolumeDetailPage from './routes/volumes/VolumeDetailPage';
import SnapshotsPage from './routes/snapshots/SnapshotsPage';
import SnapshotDetailPage from './routes/snapshots/SnapshotDetailPage';
import JobsPage, { jobsLoader, jobsAction } from './routes/jobs/JobsPage';
import JobNewPage, { createJobAction } from './routes/jobs/new';
import JobDetailPage from './routes/jobs/detail';
import PoliciesPage, { policiesLoader, policiesAction } from './routes/policies/PoliciesPage';
import PolicyNewPage from './routes/policies/new';
import PolicyDetailPage from './routes/policies/detail';
import UsersPage, { usersLoader } from './routes/users/UsersPage';
import UserNewPage from './routes/users/new';
import { ErrorBoundary } from './components/features/error-boundary';
import { ROUTES } from './lib/constants';
import { api } from './lib/api';
import { getSessionManager } from './lib/auth/session-manager';

/**
 * Root loader - validates authentication before any protected route loads
 * 
 * This is the single source of truth for authentication.
 * On auth failure, redirects to login (does NOT throw error for error boundary).
 */
async function rootLoader() {
  const sessionManager = getSessionManager();

  try {
    // Validate session with backend
    const user = await api.getMe();
    
    // Mark session as authenticated and validated
    sessionManager.setAuthenticated();
    sessionManager.updateLastValidated();

    // Ensure CSRF token is available for mutating requests.
    // This is a no-op if the token is already cached.
    if (!sessionManager.getCsrfToken()) {
      await api.fetchCsrfToken();
    }
    
    return { user };
  } catch (error: any) {
    // Check if it's an authentication error (401)
    const isAuthError = error?.statusCode === 401 || 
                       error?.response?.status === 401 || 
                       error?.status === 401 ||
                       error?.name === 'AuthenticationError';
    
    if (isAuthError) {
      // Clear session state
      sessionManager.clearSession();
      
      // Redirect to login with return URL
      const currentPath = window.location.pathname + window.location.search;
      const redirectParam = currentPath !== '/' && currentPath !== ROUTES.DASHBOARD
        ? `?redirect=${encodeURIComponent(currentPath)}`
        : '';
      
      // Use redirect() to navigate, not throw error
      return redirect(`${ROUTES.LOGIN}${redirectParam}`);
    }
    
    // Non-auth errors should be handled by error boundary
    throw error;
  }
}

// Admin-only loader wrapper
export async function requireAdmin() {
  const data = await rootLoader();
  
  // If it's a redirect response, return it
  if (data instanceof Response) {
    return data;
  }
  
  if (data.user.role !== 'admin') {
    throw new Response('Forbidden: Admin access required', { status: 403 });
  }
  return data;
}

/**
 * Login loader - redirect to dashboard if already authenticated
 * 
 * Uses session manager to avoid unnecessary API calls.
 */
async function loginLoader() {
  const sessionManager = getSessionManager();
  
  // Quick check: if we think we're not authenticated, allow login page
  if (!sessionManager.isAuthenticated()) {
    return null;
  }
  
  // We think we're authenticated - verify with backend
  try {
    await api.getMe();
    sessionManager.updateLastValidated();
    // User is authenticated, redirect to dashboard
    return redirect(ROUTES.DASHBOARD);
  } catch (error: any) {
    // Session invalid - clear and allow login page
    sessionManager.clearSession();
    return null;
  }
}

const router = createBrowserRouter([
  {
    path: ROUTES.LOGIN,
    element: <Login />,
    loader: loginLoader,
    errorElement: <ErrorPage />,
  },
  {
    path: '/',
    element: <RootLayout />,
    loader: rootLoader,
    errorElement: <ErrorPage />,
    children: [
      {
        index: true,
        element: <Dashboard />,
      },
      {
        path: ROUTES.VOLUMES,
        element: <VolumesPage />,
      },
      {
        path: `${ROUTES.VOLUMES}/:name`,
        element: <VolumeDetailPage />,
      },
      {
        path: ROUTES.SNAPSHOTS,
        element: <SnapshotsPage />,
      },
      {
        path: `${ROUTES.SNAPSHOTS}/:id`,
        element: <SnapshotDetailPage />,
      },
      {
        path: ROUTES.JOBS,
        element: <JobsPage />,
        loader: jobsLoader,
        action: jobsAction,
      },
      {
        path: ROUTES.JOB_NEW,
        element: <JobNewPage />,
        action: createJobAction,
      },
      {
        path: `${ROUTES.JOBS}/:id`,
        element: <JobDetailPage />,
      },
      {
        path: ROUTES.POLICIES,
        element: <PoliciesPage />,
        loader: policiesLoader,
        action: policiesAction,
      },
      {
        path: ROUTES.POLICY_NEW,
        element: <PolicyNewPage />,
      },
      {
        path: `${ROUTES.POLICIES}/:id`,
        element: <PolicyDetailPage />,
      },
      {
        path: ROUTES.USERS,
        element: <UsersPage />,
        loader: usersLoader,
      },
      {
        path: ROUTES.USER_NEW,
        element: <UserNewPage />,
        loader: requireAdmin,
      },
      {
        path: '*',
        element: <Navigate to={ROUTES.DASHBOARD} replace />,
      },
    ],
  },
]);

export function App() {
  return (
    <ErrorBoundary>
      <ThemeProvider attribute="class" defaultTheme="system" enableSystem>
        <RouterProvider router={router} />
        <Toaster />
      </ThemeProvider>
    </ErrorBoundary>
  );
}

export default App;