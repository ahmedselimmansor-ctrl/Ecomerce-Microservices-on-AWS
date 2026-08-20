'use client';

import { useEffect } from 'react';
import { AlertTriangle } from 'lucide-react';

import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert';
import { Button } from '@/components/ui/button';

export default function AdminError({
  error,
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  useEffect(() => {
    console.error('[admin] unhandled error', error);
  }, [error]);

  return (
    <Alert variant="destructive" className="mx-auto max-w-xl">
      <AlertTriangle className="h-4 w-4" />
      <AlertTitle>This page failed to load</AlertTitle>
      <AlertDescription className="space-y-4">
        <p>
          {/*
            Unlike the storefront, the raw message is shown. The audience here
            is the team that operates the platform — withholding "connection
            refused to payment-service:8086" from an on-call engineer helps
            nobody, and the page is behind an MFA-gated role check.
          */}
          <code className="block break-all rounded bg-muted px-2 py-1 font-mono text-xs">
            {error.message}
          </code>
        </p>
        {error.digest && (
          <p className="text-xs">
            Digest <code className="font-mono">{error.digest}</code> — matches the server log line.
          </p>
        )}
        <Button onClick={reset} variant="outline" size="sm">
          Retry
        </Button>
      </AlertDescription>
    </Alert>
  );
}
