import Link from 'next/link';

import { RegisterForm } from '@/components/auth/register-form';

export const metadata = {
  title: 'Create an account',
  robots: { index: false, follow: true },
};

export default function RegisterPage() {
  return (
    <div className="container flex max-w-md flex-col py-16">
      <h1 className="text-2xl font-bold tracking-tight">Create an account</h1>
      <p className="mt-1 text-sm text-muted-foreground">
        Already have one?{' '}
        <Link href="/login" className="font-medium text-primary hover:underline">
          Sign in
        </Link>
      </p>

      <RegisterForm />
    </div>
  );
}
