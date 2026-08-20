'use client';

import { useState } from 'react';
import { useRouter, useSearchParams } from 'next/navigation';
import Link from 'next/link';
import { AlertTriangle, Loader2 } from 'lucide-react';

import { LoginResponse } from '@souq/contracts';

import { ApiError, apiFetch } from '@/lib/api-client';
import { Alert, AlertDescription } from '@/components/ui/alert';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { useSession } from './session-provider';

/**
 * Sign in.
 *
 * The MFA field appears only after the server says it is needed. Asking every
 * user for a code they may not have is worse than a second round trip, and the
 * password has already been verified by the time `MFA_REQUIRED` comes back — so
 * revealing that the account exists at that point discloses nothing new.
 *
 * Every other failure renders the same message. identity-service deliberately
 * returns an identical 401 for an unknown email, a wrong password and a
 * disabled account, and reconstructing a distinction in the UI would undo that.
 */
export function LoginForm() {
  const router = useRouter();
  const searchParams = useSearchParams();
  const { refresh } = useSession();

  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [mfaCode, setMfaCode] = useState('');
  const [mfaRequired, setMfaRequired] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  async function onSubmit(event: React.FormEvent) {
    event.preventDefault();
    setError(null);
    setSubmitting(true);

    try {
      await apiFetch('/api/bff/auth/login', {
        schema: LoginResponse,
        method: 'POST',
        body: { email, password, ...(mfaCode ? { mfaCode } : {}) },
      });

      await refresh();
      router.replace(safeNext(searchParams.get('next')));
      // Ensure server components re-render with the new session rather than
      // serving the signed-out versions from the router cache.
      router.refresh();
    } catch (err) {
      if (!(err instanceof ApiError)) throw err;

      if (err.code === 'MFA_REQUIRED') {
        setMfaRequired(true);
        setError(null);
      } else {
        setError(err.userMessage);
      }
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <form onSubmit={onSubmit} className="mt-8 space-y-4">
      {error && (
        <Alert variant="destructive">
          <AlertTriangle className="h-4 w-4" />
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}

      <div className="space-y-2">
        <Label htmlFor="email">Email</Label>
        <Input
          id="email"
          type="email"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          required
          autoComplete="email"
          // Browsers and password managers key off these. Getting them wrong is
          // the main reason a password manager fails to offer to fill a form.
          autoFocus
        />
      </div>

      <div className="space-y-2">
        <div className="flex items-baseline justify-between">
          <Label htmlFor="password">Password</Label>
          <Link href="/forgot-password" className="text-xs text-muted-foreground hover:underline">
            Forgotten it?
          </Link>
        </div>
        <Input
          id="password"
          type="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          required
          autoComplete="current-password"
        />
      </div>

      {mfaRequired && (
        <div className="space-y-2">
          <Label htmlFor="mfaCode">Authentication code</Label>
          <Input
            id="mfaCode"
            inputMode="numeric"
            pattern="[0-9]{6}"
            maxLength={6}
            value={mfaCode}
            onChange={(e) => setMfaCode(e.target.value.replace(/[^0-9]/g, ''))}
            required
            // Lets iOS and Android offer the code straight from the SMS or
            // authenticator, which is the difference between one tap and
            // switching apps.
            autoComplete="one-time-code"
            autoFocus
            className="tabular tracking-widest"
          />
          <p className="text-xs text-muted-foreground">
            Six digits from your authenticator app.
          </p>
        </div>
      )}

      <Button type="submit" size="lg" className="w-full" disabled={submitting}>
        {submitting && <Loader2 className="animate-spin" />}
        Sign in
      </Button>
    </form>
  );
}

/**
 * Sanitise the post-login redirect.
 *
 * Only same-origin, path-only destinations. `?next=https://evil.example` is an
 * open redirect, and an open redirect on a login page is the standard way to
 * make a phishing link look like it comes from us. The protocol-relative form
 * `//evil.example` is the variant that a naive `startsWith('/')` check misses.
 */
function safeNext(next: string | null): string {
  if (!next) return '/';
  if (!next.startsWith('/')) return '/';
  if (next.startsWith('//')) return '/';
  return next;
}
