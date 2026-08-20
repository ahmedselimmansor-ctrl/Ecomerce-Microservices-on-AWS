'use client';

import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react';

import { UserProfile } from '@souq/contracts';

import { ApiError, apiFetch } from '@/lib/api-client';

/**
 * Who is signed in.
 *
 * There is no token here, and that is the point. The access token lives on the
 * server and the refresh token is an HttpOnly cookie the browser cannot read
 * (docs/CONTRACTS.md §8). This context holds a *profile* — a display name and a
 * set of roles — and nothing that could be exfiltrated by an XSS and replayed.
 *
 * `roles` is here for rendering decisions only. Every authorisation check that
 * matters happens server-side; hiding a link is a courtesy, not a control.
 */

interface SessionContextValue {
  user: UserProfile | null;
  loading: boolean;
  signOut: () => Promise<void>;
  refresh: () => Promise<void>;
}

const SessionContext = createContext<SessionContextValue | null>(null);

export function SessionProvider({
  children,
  initialUser = null,
}: {
  children: React.ReactNode;
  initialUser?: UserProfile | null;
}) {
  const [user, setUser] = useState<UserProfile | null>(initialUser);
  // If the server already resolved the session, do not flash a loading state.
  const [loading, setLoading] = useState(initialUser === null);

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      setUser(await apiFetch('/api/bff/me', { schema: UserProfile }));
    } catch (err) {
      // 401 is the normal state for a signed-out visitor, not a failure worth
      // logging or reporting.
      if (!(err instanceof ApiError) || err.status !== 401) {
        console.error('[session] could not load the profile', err);
      }
      setUser(null);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    if (initialUser === null) void refresh();
  }, [initialUser, refresh]);

  const signOut = useCallback(async () => {
    try {
      await apiFetch('/api/bff/auth/logout', { schema: (await import('zod')).z.undefined(), method: 'POST' });
    } catch {
      // Logout is idempotent and must not be able to fail visibly. A user who
      // cannot complete it is left believing they are still signed in.
    }
    setUser(null);
    // A full reload, not a router refresh: it clears every cached server
    // component payload, and leaving one of those holding the previous user's
    // data on a shared machine is the whole risk.
    window.location.href = '/';
  }, []);

  const value = useMemo<SessionContextValue>(
    () => ({ user, loading, signOut, refresh }),
    [user, loading, signOut, refresh],
  );

  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>;
}

export function useSession(): SessionContextValue {
  const context = useContext(SessionContext);
  if (!context) {
    throw new Error('useSession must be used inside a SessionProvider');
  }
  return context;
}
