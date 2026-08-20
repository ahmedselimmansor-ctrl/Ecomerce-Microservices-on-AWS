import Link from 'next/link';
import { KeyRound, ShieldAlert, ShieldX } from 'lucide-react';

import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert';
import { Button } from '@/components/ui/button';

/**
 * The three ways a caller can fail the admin check, told apart.
 *
 * Unusually for this platform, the distinction IS made here. Everywhere else —
 * login, registration, password reset — the responses are deliberately
 * identical to avoid an enumeration oracle. This is different: the caller has
 * already authenticated, so nothing is being disclosed about whether an account
 * exists, and "you need MFA" versus "you need a role" is the difference between
 * a two-minute fix and a support ticket.
 */
export function Unauthorised({ reason }: { reason: 'unauthenticated' | 'no-role' | 'no-mfa' }) {
  if (reason === 'unauthenticated') {
    return (
      <Alert className="mx-auto max-w-lg">
        <KeyRound className="h-4 w-4" />
        <AlertTitle>Sign in</AlertTitle>
        <AlertDescription className="space-y-3">
          <p>This tool requires a signed-in staff account.</p>
          <Button size="sm" asChild>
            <a href={`${process.env.NEXT_PUBLIC_STOREFRONT_URL ?? 'http://localhost:3000'}/login?next=/`}>
              Go to sign in
            </a>
          </Button>
        </AlertDescription>
      </Alert>
    );
  }

  if (reason === 'no-mfa') {
    return (
      <Alert variant="warning" className="mx-auto max-w-lg">
        <ShieldAlert className="h-4 w-4" />
        <AlertTitle>Two-factor authentication required</AlertTitle>
        <AlertDescription className="space-y-3">
          <p>
            Your account has the right role, but the admin tool requires a second factor
            (docs/CONTRACTS.md §7). A role survives a stolen password; a second factor does not.
          </p>
          <Button size="sm" variant="outline" asChild>
            <a href={`${process.env.NEXT_PUBLIC_STOREFRONT_URL ?? 'http://localhost:3000'}/account/security`}>
              Set up two-factor
            </a>
          </Button>
        </AlertDescription>
      </Alert>
    );
  }

  return (
    <Alert variant="destructive" className="mx-auto max-w-lg">
      <ShieldX className="h-4 w-4" />
      <AlertTitle>Not permitted</AlertTitle>
      <AlertDescription>
        This tool requires the ADMIN or OPS role. Ask a platform administrator if you need access.
      </AlertDescription>
    </Alert>
  );
}

export function PanelUnavailable({ name }: { name: string }) {
  return (
    <Alert variant="warning">
      <ShieldAlert className="h-4 w-4" />
      <AlertTitle>{name} is unavailable</AlertTitle>
      <AlertDescription className="text-xs">
        The rest of this page is still accurate. This panel could not be loaded — check the
        service&rsquo;s own health before treating the gap as a data problem.
      </AlertDescription>
    </Alert>
  );
}
