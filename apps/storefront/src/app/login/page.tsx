import Link from 'next/link';
import { Suspense } from 'react';

import { LoginForm } from '@/components/auth/login-form';
import { Skeleton } from '@/components/ui/skeleton';

export const metadata = {
  title: 'Sign in',
  robots: { index: false, follow: true },
};

export default function LoginPage() {
  return (
    <div className="container flex max-w-md flex-col py-16">
      <h1 className="text-2xl font-bold tracking-tight">Sign in</h1>
      <p className="mt-1 text-sm text-muted-foreground">
        New here?{' '}
        <Link href="/register" className="font-medium text-primary hover:underline">
          Create an account
        </Link>
      </p>

      {/* LoginForm reads `?next=` from the URL, which makes it a dynamic subtree. */}
      <Suspense fallback={<Skeleton className="mt-8 h-64 w-full" />}>
        <LoginForm />
      </Suspense>
    </div>
  );
}
