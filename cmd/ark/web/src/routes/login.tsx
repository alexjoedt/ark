import { useEffect, useState } from 'react';
import { useNavigate, useLocation } from 'react-router';
import { useForm } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import { loginSchema, type LoginFormData } from '../lib/schemas';
import { api } from '../lib/api';
import { getSessionManager } from '../lib/auth/session-manager';
import { ROUTES } from '../lib/constants';
import { Button } from '../components/ui/button';
import { Input } from '../components/ui/input';
import { Label } from '../components/ui/label';
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '../components/ui/card';
import { Alert, AlertDescription } from '../components/ui/alert';
import { AlertCircle } from 'lucide-react';

export function Login() {
  const navigate = useNavigate();
  const location = useLocation();
  const [apiError, setApiError] = useState<string | null>(null);
  const sessionManager = getSessionManager();

  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
    setFocus,
  } = useForm<LoginFormData>({
    resolver: zodResolver(loginSchema),
  });

  // Get redirect parameter from URL
  const searchParams = new URLSearchParams(location.search);
  const redirectTo = searchParams.get('redirect') || ROUTES.DASHBOARD;

  // Clear any stale session state on mount (user was redirected here due to invalid session)
  useEffect(() => {
    sessionManager.clearSession();
  }, [sessionManager]);

  // Focus on first error field
  useEffect(() => {
    if (errors.username) {
      setFocus('username');
    } else if (errors.password) {
      setFocus('password');
    }
  }, [errors, setFocus]);

  const onSubmit = async (data: LoginFormData) => {
    setApiError(null);
    try {
      await api.login(data.username, data.password);
      // Mark session as authenticated
      sessionManager.setAuthenticated();
      // After successful login, navigate to protected route
      // The rootLoader will fetch user data
      navigate(redirectTo, { replace: true });
    } catch (error: any) {
      const message =
        error?.message ||
        'Invalid credentials. Please try again.';
      setApiError(message);
    }
  };

  return (
    <div className="flex h-screen items-center justify-center bg-muted/40">
      <Card className="w-full max-w-md mx-4">
        <CardHeader>
          <CardTitle>Login to arkUI</CardTitle>
          <CardDescription>
            Enter your credentials to access the Docker volume backup dashboard
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleSubmit(onSubmit)} className="space-y-4">
            {apiError && (
              <Alert variant="destructive">
                <AlertCircle />
                <AlertDescription role="alert">{apiError}</AlertDescription>
              </Alert>
            )}

            <div className="space-y-2">
              <Label htmlFor="username">Username</Label>
              <Input
                id="username"
                type="text"
                autoComplete="username"
                autoFocus
                {...register('username')}
                aria-invalid={!!errors.username}
                aria-describedby={errors.username ? 'username-error' : undefined}
              />
              {errors.username && (
                <p
                  id="username-error"
                  className="text-sm text-destructive"
                  role="alert"
                >
                  {errors.username.message}
                </p>
              )}
            </div>

            <div className="space-y-2">
              <Label htmlFor="password">Password</Label>
              <Input
                id="password"
                type="password"
                autoComplete="current-password"
                {...register('password')}
                aria-invalid={!!errors.password}
                aria-describedby={errors.password ? 'password-error' : undefined}
              />
              {errors.password && (
                <p
                  id="password-error"
                  className="text-sm text-destructive"
                  role="alert"
                >
                  {errors.password.message}
                </p>
              )}
            </div>

            <Button type="submit" className="w-full" disabled={isSubmitting}>
              {isSubmitting ? 'Logging in...' : 'Login'}
            </Button>
          </form>
        </CardContent>
      </Card>
    </div>
  );
}
