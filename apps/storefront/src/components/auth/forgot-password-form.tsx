'use client';

import { useState } from 'react';
import { CheckCircle2, Loader2 } from 'lucide-react';
import { z } from 'zod';

import { ApiError, apiFetch } from '@/lib/api-client';
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';

const AcceptedResponse = z.object({ status: z.string(), message: z.string() }).passthrough();

/**
 * Request a password-reset link.
 *
 * Always shows the same confirmation, even for an address with no account.
 * identity-service returns an identical 202 either way, and this is one of the
 * few places where an unauthenticated caller can probe for the existence of an
 * account — so the UI must not undo it by rendering "no such user".
 *
 * Even a transport failure resolves to the same message. A visible error only
 * for real addresses would be the oracle again, wearing a different hat.
 */
export function ForgotPasswordForm() {
  const [email, setEmail] = useState('');
  const [sent, setSent] = useState(false);
  const [submitting, setSubmitting] = useState(false);

  async function onSubmit(event: React.FormEvent) {
    event.preventDefault();
    setSubmitting(true);

    try {
      await apiFetch('/api/bff/auth/forgot-password', {
        schema: AcceptedResponse,
        method: 'POST',
        body: { email },
      });
    } catch (err) {
      // Logged, not shown. See above.
      if (err instanceof ApiError) {
        console.warn('[auth] password reset request failed', err.code, err.requestId);
      }
    } finally {
      setSubmitting(false);
      setSent(true);
    }
  }

  if (sent) {
    return (
      <Alert variant="success" className="mt-8">
        <CheckCircle2 className="h-4 w-4" />
        <AlertTitle>Check your email</AlertTitle>
        <AlertDescription>
          If <span className="font-medium">{email}</span> has an account with us, a reset link is
          on its way. The link is valid for 30 minutes.
        </AlertDescription>
      </Alert>
    );
  }

  return (
    <form onSubmit={onSubmit} className="mt-8 space-y-4">
      <div className="space-y-2">
        <Label htmlFor="email">Email</Label>
        <Input
          id="email"
          type="email"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          required
          autoComplete="email"
          autoFocus
        />
      </div>

      <Button type="submit" size="lg" className="w-full" disabled={submitting}>
        {submitting && <Loader2 className="animate-spin" />}
        Send reset link
      </Button>
    </form>
  );
}
