'use client';

import { useState } from 'react';
import Link from 'next/link';
import { AlertTriangle, CheckCircle2, Loader2 } from 'lucide-react';

import { z } from 'zod';

import { ApiError, apiFetch } from '@/lib/api-client';
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';

const AcceptedResponse = z.object({ status: z.string(), message: z.string() }).passthrough();

/** The version the copy on this page corresponds to. Recorded against the account. */
const TERMS_VERSION = 'v1';

/**
 * Create an account.
 *
 * The success state is deliberately uninformative: "if that address can receive
 * mail, a message is on its way". identity-service returns the same 202 whether
 * the account was created or already existed, because anything else turns an
 * unauthenticated endpoint into an account-enumeration oracle. Reconstructing a
 * distinction here — "welcome!" versus "check your email" — would give the
 * oracle straight back.
 *
 * Password strength IS reported, in detail. That is about this request, not
 * about whether an account exists, so it leaks nothing.
 */
export function RegisterForm() {
  const [fullName, setFullName] = useState('');
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [submitted, setSubmitted] = useState(false);
  const [submitting, setSubmitting] = useState(false);

  async function onSubmit(event: React.FormEvent) {
    event.preventDefault();
    setError(null);
    setSubmitting(true);

    try {
      await apiFetch('/api/bff/auth/register', {
        schema: AcceptedResponse,
        method: 'POST',
        body: {
          email,
          password,
          fullName,
          locale: 'en-GB',
          acceptedTermsVersion: TERMS_VERSION,
        },
      });
      setSubmitted(true);
    } catch (err) {
      if (!(err instanceof ApiError)) throw err;
      setError(err.userMessage);
    } finally {
      setSubmitting(false);
    }
  }

  if (submitted) {
    return (
      <Alert variant="success" className="mt-8">
        <CheckCircle2 className="h-4 w-4" />
        <AlertTitle>Check your email</AlertTitle>
        <AlertDescription className="space-y-3">
          <p>
            If <span className="font-medium">{email}</span> can receive mail, we have sent it a
            link to finish setting up.
          </p>
          <Button variant="outline" size="sm" asChild>
            <Link href="/login">Back to sign in</Link>
          </Button>
        </AlertDescription>
      </Alert>
    );
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
        <Label htmlFor="fullName">Full name</Label>
        <Input
          id="fullName"
          value={fullName}
          onChange={(e) => setFullName(e.target.value)}
          required
          maxLength={200}
          autoComplete="name"
          autoFocus
        />
      </div>

      <div className="space-y-2">
        <Label htmlFor="email">Email</Label>
        <Input
          id="email"
          type="email"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          required
          maxLength={254}
          autoComplete="email"
        />
      </div>

      <div className="space-y-2">
        <Label htmlFor="password">Password</Label>
        <Input
          id="password"
          type="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          required
          minLength={12}
          maxLength={256}
          autoComplete="new-password"
          aria-describedby="password-help"
        />
        {/*
          Length, and nothing else. NIST SP 800-63B has advised against
          composition rules since 2017: they mostly produce Passw0rd!, and the
          server does the work that matters by rejecting known-breached
          passwords through a k-anonymity range query.
        */}
        <p id="password-help" className="text-xs text-muted-foreground">
          At least 12 characters. A memorable phrase is stronger than a short complex password.
        </p>
      </div>

      <p className="text-xs text-muted-foreground">
        By creating an account you agree to our{' '}
        <Link href="/legal/terms" className="underline hover:text-foreground">terms</Link> and{' '}
        <Link href="/legal/privacy" className="underline hover:text-foreground">privacy policy</Link>.
      </p>

      <Button type="submit" size="lg" className="w-full" disabled={submitting}>
        {submitting && <Loader2 className="animate-spin" />}
        Create account
      </Button>
    </form>
  );
}
