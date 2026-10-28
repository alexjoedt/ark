import { useRouteError, isRouteErrorResponse, useNavigate } from 'react-router';
import { AlertCircle, Home, ArrowLeft, Copy, Check, ShieldAlert } from 'lucide-react';
import { useState, useEffect } from 'react';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';
import { Alert, AlertDescription } from '@/components/ui/alert';
import { Container } from '@/components/layout/container';
import { ROUTES } from '@/lib/constants';

export function ErrorPage() {
  const error = useRouteError();
  const navigate = useNavigate();
  const [copied, setCopied] = useState(false);

  // Note: 401 errors should be handled by loaders (rootLoader redirects to /login)
  // If we see a 401 here, something went wrong with loader error handling.
  // We'll still gracefully handle it by redirecting.
  useEffect(() => {
    if (isRouteErrorResponse(error) && error.status === 401) {
      console.warn('[ErrorPage] Unexpected 401 in error boundary - redirecting to login');
      const currentPath = window.location.pathname + window.location.search;
      const redirectParam = currentPath !== '/' && currentPath !== ROUTES.DASHBOARD
        ? `?redirect=${encodeURIComponent(currentPath)}`
        : '';
      navigate(`${ROUTES.LOGIN}${redirectParam}`, { replace: true });
    }
  }, [error, navigate]);

  // Don't render anything for 401 - redirect is handling it
  if (isRouteErrorResponse(error) && error.status === 401) {
    return null;
  }

  // Special handling for 403 Forbidden (admin-only routes)
  if (isRouteErrorResponse(error) && error.status === 403) {
    return (
      <div className="min-h-screen bg-background flex items-center justify-center p-6">
        <Container className="max-w-2xl">
          <div className="flex flex-col items-center text-center gap-6">
            <div className="h-20 w-20 rounded-2xl bg-amber-500/10 flex items-center justify-center">
              <ShieldAlert className="h-10 w-10 text-amber-600" />
            </div>
            <div className="space-y-2">
              <h1 className="text-3xl font-semibold">Access Denied</h1>
              <p className="text-muted-foreground text-lg">
                You don't have permission to access this page.
              </p>
            </div>
            <div className="flex items-center gap-3">
              <Button onClick={() => navigate(-1)} variant="outline" className="gap-2">
                <ArrowLeft className="h-4 w-4" />
                Go Back
              </Button>
              <Button onClick={() => navigate(ROUTES.DASHBOARD)} className="gap-2">
                <Home className="h-4 w-4" />
                Go to Dashboard
              </Button>
            </div>
          </div>
        </Container>
      </div>
    );
  }

  // Extract error information
  const errorInfo = getErrorInfo(error);

  const handleCopyError = async () => {
    const errorText = formatErrorForClipboard(errorInfo);
    try {
      await navigator.clipboard.writeText(errorText);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    } catch (err) {
      console.error('Failed to copy error:', err);
    }
  };

  const handleGoBack = () => {
    if (window.history.length > 1) {
      navigate(-1);
    } else {
      navigate('/');
    }
  };

  return (
    <div className="min-h-screen bg-background flex items-center justify-center p-6">
      <Container className="max-w-3xl">
        <div className="flex flex-col gap-6">
          {/* Error Icon and Title */}
          <div className="flex items-center gap-4">
            <div className="h-16 w-16 rounded-2xl bg-destructive/10 flex items-center justify-center flex-shrink-0">
              <AlertCircle className="h-8 w-8 text-destructive" />
            </div>
            <div>
              <h1 className="text-3xl font-semibold">Something went wrong</h1>
              <p className="text-muted-foreground mt-1">
                {errorInfo.status
                  ? `Error ${errorInfo.status}: ${errorInfo.statusText}`
                  : 'An unexpected error occurred'}
              </p>
            </div>
          </div>

          {/* Action Buttons */}
          <div className="flex items-center gap-3">
            <Button onClick={handleGoBack} variant="outline" className="gap-2">
              <ArrowLeft className="h-4 w-4" />
              Go Back
            </Button>
            <Button onClick={() => navigate('/')} className="gap-2">
              <Home className="h-4 w-4" />
              Go to Dashboard
            </Button>
          </div>

          {/* Error Message */}
          {errorInfo.message && (
            <Alert variant="destructive" className="border-destructive/50">
              <AlertCircle className="h-4 w-4" />
              <AlertDescription className="font-medium">
                {errorInfo.message}
              </AlertDescription>
            </Alert>
          )}

          {/* Error Details Card */}
          <Card>
            <CardHeader className="border-b">
              <div className="flex items-center justify-between">
                <CardTitle className="text-lg">Error Details</CardTitle>
                <Button
                  size="sm"
                  variant="outline"
                  onClick={handleCopyError}
                  className="h-8 gap-2"
                >
                  {copied ? (
                    <>
                      <Check className="h-3.5 w-3.5" />
                      Copied
                    </>
                  ) : (
                    <>
                      <Copy className="h-3.5 w-3.5" />
                      Copy Details
                    </>
                  )}
                </Button>
              </div>
            </CardHeader>
            <CardContent className="p-6">
              <div className="space-y-4">
                {/* Error Type */}
                {/* <div>
                  <p className="text-sm font-medium text-muted-foreground mb-1">
                    Error Type
                  </p>
                  <code className="text-sm bg-muted px-2 py-1 rounded">
                    {errorInfo.type}
                  </code>
                </div> */}

                {/* {errorInfo.stack ? (
                  <div>
                    <p className="text-sm font-medium text-muted-foreground mb-2">
                      Stack Trace
                    </p>
                    <div className="relative">
                      <pre className="text-xs bg-muted p-4 rounded-lg overflow-x-auto max-h-96 overflow-y-auto border">
                        <code className="text-muted-foreground font-mono">
                          {errorInfo.stack ?? ''}
                        </code>
                      </pre>
                    </div>
                  </div>
                ) : null} */}

                {/* Additional Data */}
                {/* {errorInfo.data && (
                  <div>
                    <p className="text-sm font-medium text-muted-foreground mb-2">
                      Additional Information
                    </p>
                    <pre className="text-xs bg-muted p-4 rounded-lg overflow-x-auto border">
                      <code className="text-muted-foreground font-mono">
                        {typeof errorInfo.data === 'string' 
                          ? errorInfo.data 
                          : JSON.stringify(errorInfo.data, null, 2) ?? ''}
                      </code>
                    </pre>
                  </div>
                )} */}
              </div>
            </CardContent>
          </Card>
        </div>
      </Container>
    </div>
  );
}

// Helper functions

interface ErrorInfo {
  type: string;
  message: string;
  status?: number;
  statusText?: string;
  stack?: string;
  data?: unknown;
}

function getErrorInfo(error: unknown): ErrorInfo {
  // React Router error response
  if (isRouteErrorResponse(error)) {
    return {
      type: 'RouteErrorResponse',
      message: error.statusText || error.data?.message || 'Route error occurred',
      status: error.status,
      statusText: error.statusText,
      data: error.data,
      stack: undefined,
    };
  }

  // Standard Error object
  if (error instanceof Error) {
    return {
      type: error.name || 'Error',
      message: error.message,
      stack: error.stack,
      data: undefined,
    };
  }

  // String error
  if (typeof error === 'string') {
    return {
      type: 'String Error',
      message: error,
      stack: undefined,
      data: undefined,
    };
  }

  // Unknown error type
  return {
    type: 'Unknown Error',
    message: 'An unknown error occurred',
    stack: undefined,
    data: error,
  };
}

function formatErrorForClipboard(errorInfo: ErrorInfo): string {
  const lines: string[] = [];

  lines.push('=== Error Report ===');
  lines.push('');
  lines.push(`Timestamp: ${new Date().toISOString()}`);
  lines.push(`Type: ${errorInfo.type}`);

  if (errorInfo.status) {
    lines.push(`Status: ${errorInfo.status} ${errorInfo.statusText || ''}`);
  }

  lines.push('');
  lines.push('Message:');
  lines.push(errorInfo.message);

  if (errorInfo.stack) {
    lines.push('');
    lines.push('Stack Trace:');
    lines.push(errorInfo.stack);
  }

  if (errorInfo.data) {
    lines.push('');
    lines.push('Additional Data:');
    lines.push(JSON.stringify(errorInfo.data, null, 2));
  }

  lines.push('');
  lines.push('=== End of Error Report ===');

  return lines.join('\n');
}
