import Link from 'next/link';

import { ForgotPasswordForm } from '@/components/auth/forgot-password-form';

export const metadata = {
  title: 'Reset your password',
  robots: { index: false, follow: true },
};

export default function ForgotPasswordPage() {
  return (
    <div className="container flex max-w-md flex-col py-16">
      <h1 className="text-2xl font-bold tracking-tight">Reset your password</h1>
      <p className="mt-1 text-sm text-muted-foreground">
        We will email you a link.{' '}
        <Link href="/login" className="font-medium text-primary hover:underline">
          Back to sign in
        </Link>
      </p>

      <ForgotPasswordForm />
    </div>
  );
}
