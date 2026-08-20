'use client';

import Link from 'next/link';
import { BadgeCheck, Lock, ShieldAlert } from 'lucide-react';

import { Alert, AlertDescription } from '@/components/ui/alert';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent } from '@/components/ui/card';
import { Skeleton } from '@/components/ui/skeleton';
import { useSession } from './session-provider';

export function ProfilePanel() {
  const { user, loading, signOut } = useSession();

  if (loading) {
    return (
      <Card>
        <CardContent className="space-y-3 p-6">
          <Skeleton className="h-5 w-40" />
          <Skeleton className="h-4 w-64" />
          <Skeleton className="h-4 w-32" />
        </CardContent>
      </Card>
    );
  }

  if (!user) {
    return (
      <Alert>
        <Lock className="h-4 w-4" />
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>Sign in to see your account.</span>
          <Button size="sm" asChild>
            <Link href="/login?next=/account">Sign in</Link>
          </Button>
        </AlertDescription>
      </Alert>
    );
  }

  return (
    <div className="max-w-2xl space-y-4">
      <Card>
        <CardContent className="space-y-4 p-6">
          <dl className="space-y-3 text-sm">
            <Row label="Name" value={user.fullName} />
            <Row
              label="Email"
              value={
                <span className="flex flex-wrap items-center gap-2">
                  {user.email}
                  {user.emailVerified ? (
                    <Badge variant="success" className="gap-1">
                      <BadgeCheck className="h-3 w-3" />
                      Verified
                    </Badge>
                  ) : (
                    <Badge variant="warning">Unverified</Badge>
                  )}
                </span>
              }
            />
            <Row label="Language" value={user.locale} />
            <Row
              label="Two-factor"
              value={user.mfaEnabled ? 'On' : 'Off'}
            />
          </dl>
        </CardContent>
      </Card>

      {/*
        A nudge, not a nag, and only when it is actionable. Two-factor is the
        single largest reduction in account-takeover risk available to a
        customer, and the access token's 15-minute TTL is what makes it
        meaningful — a stolen session dies quickly, so the credential is the
        thing worth protecting.
      */}
      {!user.mfaEnabled && (
        <Alert variant="warning">
          <ShieldAlert className="h-4 w-4" />
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>Turn on two-factor authentication to protect your orders and addresses.</span>
            <Button size="sm" variant="outline" asChild>
              <Link href="/account/security">Set it up</Link>
            </Button>
          </AlertDescription>
        </Alert>
      )}

      <div className="flex gap-3">
        <Button variant="outline" asChild>
          <Link href="/account/password">Change password</Link>
        </Button>
        <Button variant="ghost" onClick={() => void signOut()}>
          Sign out
        </Button>
      </div>
    </div>
  );
}

function Row({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="grid gap-1 sm:grid-cols-[8rem_1fr]">
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="font-medium">{value}</dd>
    </div>
  );
}
