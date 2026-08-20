'use client';

import { useEffect } from 'react';
import { AlertTriangle } from 'lucide-react';

import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert';
import { Button } from '@/components/ui/button';

/**
 * The route-level error boundary.
 *
 * `digest` is the only thing worth showing. Next replaces the real message with
 * it in production precisely so an internal error string cannot reach a
 * browser, and it is the value that matches the server log line — so it is the
 * one useful thing a customer can quote to support.
 */
export default function RouteError({
  error,
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  useEffect(() => {
    console.error('[route] unhandled error', error);
  }, [error]);

  return (
    <div className="container py-16">
      <Alert variant="destructive" className="mx-auto max-w-xl">
        <AlertTriangle className="h-4 w-4" />
        <AlertTitle>Something went wrong</AlertTitle>
        <AlertDescription className="space-y-4">
          <p>
            We could not load this page. It is usually temporary.
            {error.digest && (
              <>
                {' '}Quote reference <code className="font-mono text-xs">{error.digest}</code> if
                you contact us.
              </>
            )}
          </p>
          <Button onClick={reset} variant="outline" size="sm">
            Try again
          </Button>
        </AlertDescription>
      </Alert>
    </div>
  );
}
