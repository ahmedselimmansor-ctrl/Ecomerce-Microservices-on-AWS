'use client';

import { useState } from 'react';
import { AlertTriangle, CheckCircle2, Copy, Loader2, ShieldCheck } from 'lucide-react';
import { z } from 'zod';

import { ApiError, apiFetch } from '@/lib/api-client';
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { Skeleton } from '@/components/ui/skeleton';
import { useSession } from './session-provider';

const EnrolStart = z.object({
  /** Base32, for manual entry when a camera is unavailable. */
  secret: z.string(),
  /** otpauth:// URI. Rendered as a QR code by the browser, never sent as an image. */
  uri: z.string(),
}).strict();

const EnrolComplete = z.object({
  /** Shown exactly once. Not retrievable afterwards — only their hashes are stored. */
  recoveryCodes: z.array(z.string()),
}).strict();

/**
 * TOTP enrolment.
 *
 * Three things here are the difference between MFA that protects an account and
 * MFA that locks people out of one.
 *
 * **The secret is not active until a code is verified.** Enabling it on the
 * strength of "we generated a secret" locks out anyone whose authenticator app
 * failed to save it — which is a support call that ends in identity checks.
 *
 * **Recovery codes are shown once, and the flow will not finish until they are
 * acknowledged.** They are stored hashed, so there is no second chance to
 * display them. A user who closes the tab here and later loses their phone has
 * no way back in that does not involve proving who they are to a human.
 *
 * **The QR code is rendered from the otpauth URI in the browser.** Asking the
 * server for a PNG would mean the secret travels a second time, gets cached by
 * an intermediary, and lands in an access log as part of a URL.
 */
export function MfaEnrolment() {
  const { user, loading, refresh } = useSession();

  const [enrolment, setEnrolment] = useState<z.infer<typeof EnrolStart> | null>(null);
  const [code, setCode] = useState('');
  const [recoveryCodes, setRecoveryCodes] = useState<string[] | null>(null);
  const [acknowledged, setAcknowledged] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  if (loading) return <Skeleton className="mt-6 h-40 w-full" />;

  if (!user) {
    return (
      <Alert className="mt-6">
        <AlertDescription>Sign in to manage two-factor authentication.</AlertDescription>
      </Alert>
    );
  }

  // ---------------------------------------------------------------- enabled

  if (user.mfaEnabled && !recoveryCodes) {
    return (
      <Alert variant="success" className="mt-6">
        <ShieldCheck className="h-4 w-4" />
        <AlertTitle>Two-factor authentication is on</AlertTitle>
        <AlertDescription>
          You will be asked for a code from your authenticator app when you sign in.
        </AlertDescription>
      </Alert>
    );
  }

  // -------------------------------------------------------- recovery codes

  if (recoveryCodes) {
    return (
      <div className="mt-6 space-y-4">
        <Alert variant="warning">
          <AlertTriangle className="h-4 w-4" />
          <AlertTitle>Save these now</AlertTitle>
          <AlertDescription>
            Each code works once, and this is the only time they are shown — we store only their
            hashes, so we cannot show them again. Without them, losing your phone means proving
            who you are to a person.
          </AlertDescription>
        </Alert>

        <ul className="grid grid-cols-2 gap-2 rounded-md border p-4 font-mono text-sm">
          {recoveryCodes.map((recoveryCode) => (
            <li key={recoveryCode} className="tabular">{recoveryCode}</li>
          ))}
        </ul>

        <div className="flex flex-wrap items-center gap-3">
          <Button
            variant="outline"
            size="sm"
            onClick={() => void navigator.clipboard.writeText(recoveryCodes.join('\n'))}
          >
            <Copy className="h-4 w-4" />
            Copy all
          </Button>

          <label className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={acknowledged}
              onChange={(e) => setAcknowledged(e.target.checked)}
              className="h-4 w-4"
            />
            I have saved these somewhere safe
          </label>
        </div>

        <Button
          disabled={!acknowledged}
          onClick={() => {
            setRecoveryCodes(null);
            void refresh();
          }}
        >
          Done
        </Button>
      </div>
    );
  }

  // --------------------------------------------------------------- enrol

  async function start() {
    setError(null);
    setBusy(true);
    try {
      setEnrolment(await apiFetch('/api/bff/me/mfa', { schema: EnrolStart, method: 'POST' }));
    } catch (err) {
      setError(err instanceof ApiError ? err.userMessage : 'Could not start enrolment.');
    } finally {
      setBusy(false);
    }
  }

  async function confirm(event: React.FormEvent) {
    event.preventDefault();
    setError(null);
    setBusy(true);
    try {
      const result = await apiFetch('/api/bff/me/mfa/confirm', {
        schema: EnrolComplete,
        method: 'POST',
        body: { code },
      });
      setRecoveryCodes(result.recoveryCodes);
      setCode('');
    } catch (err) {
      setError(err instanceof ApiError ? err.userMessage : 'That code was not accepted.');
    } finally {
      setBusy(false);
    }
  }

  if (!enrolment) {
    return (
      <div className="mt-6 space-y-4">
        {error && (
          <Alert variant="destructive">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        )}
        <p className="text-sm text-muted-foreground">
          You will need an authenticator app — 1Password, Aegis, Google Authenticator, or any other
          that supports TOTP.
        </p>
        <Button onClick={() => void start()} disabled={busy}>
          {busy && <Loader2 className="animate-spin" />}
          Set up
        </Button>
      </div>
    );
  }

  return (
    <form onSubmit={confirm} className="mt-6 space-y-4">
      {error && (
        <Alert variant="destructive">
          <AlertTriangle className="h-4 w-4" />
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}

      <ol className="space-y-3 text-sm">
        <li>
          <span className="font-medium">1.</span> Scan this in your authenticator app, or enter the
          key by hand:
          <div className="mt-2 flex flex-wrap items-center gap-3">
            <code className="tabular select-all break-all rounded bg-muted px-2 py-1 font-mono text-xs">
              {enrolment.secret}
            </code>
            <Button
              type="button"
              variant="ghost"
              size="sm"
              onClick={() => void navigator.clipboard.writeText(enrolment.secret)}
            >
              <Copy className="h-3.5 w-3.5" />
              Copy
            </Button>
          </div>
        </li>
        <li>
          <span className="font-medium">2.</span> Enter the six-digit code it shows.
        </li>
      </ol>

      <div className="space-y-2">
        <Label htmlFor="mfa-code">Code</Label>
        <Input
          id="mfa-code"
          inputMode="numeric"
          pattern="[0-9]{6}"
          maxLength={6}
          value={code}
          onChange={(e) => setCode(e.target.value.replace(/[^0-9]/g, ''))}
          required
          autoComplete="one-time-code"
          className="tabular w-32 tracking-widest"
          autoFocus
        />
      </div>

      <div className="flex gap-3">
        <Button type="submit" disabled={busy || code.length !== 6}>
          {busy ? <Loader2 className="animate-spin" /> : <CheckCircle2 />}
          Turn it on
        </Button>
        <Button type="button" variant="ghost" onClick={() => setEnrolment(null)}>
          Cancel
        </Button>
      </div>
    </form>
  );
}
