'use client';

import { useState } from 'react';
import { useRouter } from 'next/navigation';
import { AlertTriangle, CheckCircle2, Loader2 } from 'lucide-react';
import { z } from 'zod';

import { ApiError, apiFetch } from '@/lib/api-client';
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';

/**
 * Change password.
 *
 * The current password is required even though the caller already holds a valid
 * session. A stolen access token is a 15-minute problem; a stolen token that can
 * set a new password is a permanent account takeover.
 *
 * Weak-password feedback is shown verbatim from the server. That is the one
 * place in this app where server prose reaches the user directly, and it is
 * right here: `PasswordService` produces a specific, actionable reason — "this
 * password has appeared in a known data breach" — and paraphrasing it client-side
 * would mean maintaining the same list twice.
 */
export function ChangePasswordForm() {
  const router = useRouter();

  const [current, setCurrent] = useState('');
  const [next, setNext] = useState('');
  const [confirm, setConfirm] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState(false);
  const [submitting, setSubmitting] = useState(false);

  // Checked here and nowhere else. The server has no second field to compare
  // against — it is a UI affordance against a typo, not a security control.
  const mismatch = confirm.length > 0 && next !== confirm;

  async function onSubmit(event: React.FormEvent) {
    event.preventDefault();
    if (mismatch) return;

    setError(null);
    setSubmitting(true);

    try {
      await apiFetch('/api/bff/me/password', {
        schema: z.undefined(),
        method: 'POST',
        body: { currentPassword: current, newPassword: next },
      });

      setDone(true);
      setCurrent('');
      setNext('');
      setConfirm('');
      router.refresh();
    } catch (err) {
      if (!(err instanceof ApiError)) throw err;
      setError(err.userMessage);
    } finally {
      setSubmitting(false);
    }
  }

  if (done) {
    return (
      <Alert variant="success" className="mt-6">
        <CheckCircle2 className="h-4 w-4" />
        <AlertTitle>Password changed</AlertTitle>
        <AlertDescription>
          Every other session has been signed out. You are still signed in here.
        </AlertDescription>
      </Alert>
    );
  }

  return (
    <form onSubmit={onSubmit} className="mt-6 space-y-4">
      {error && (
        <Alert variant="destructive">
          <AlertTriangle className="h-4 w-4" />
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}

      <div className="space-y-2">
        <Label htmlFor="current">Current password</Label>
        <Input
          id="current"
          type="password"
          value={current}
          onChange={(e) => setCurrent(e.target.value)}
          required
          autoComplete="current-password"
          autoFocus
        />
      </div>

      <div className="space-y-2">
        <Label htmlFor="next">New password</Label>
        <Input
          id="next"
          type="password"
          value={next}
          onChange={(e) => setNext(e.target.value)}
          required
          minLength={12}
          maxLength={256}
          autoComplete="new-password"
          aria-describedby="next-help"
        />
        <p id="next-help" className="text-xs text-muted-foreground">
          At least 12 characters, and not one you have used here before.
        </p>
      </div>

      <div className="space-y-2">
        <Label htmlFor="confirm">Confirm new password</Label>
        <Input
          id="confirm"
          type="password"
          value={confirm}
          onChange={(e) => setConfirm(e.target.value)}
          required
          autoComplete="new-password"
          aria-invalid={mismatch || undefined}
          aria-describedby={mismatch ? 'confirm-error' : undefined}
        />
        {mismatch && (
          <p id="confirm-error" className="text-xs text-destructive">
            These do not match.
          </p>
        )}
      </div>

      <Button type="submit" size="lg" disabled={submitting || mismatch}>
        {submitting && <Loader2 className="animate-spin" />}
        Change password
      </Button>
    </form>
  );
}
