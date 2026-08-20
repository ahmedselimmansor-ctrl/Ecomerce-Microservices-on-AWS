import { ChangePasswordForm } from '@/components/auth/change-password-form';

export const metadata = {
  title: 'Change your password',
  robots: { index: false, follow: false },
};

export default function ChangePasswordPage() {
  return (
    <div className="max-w-lg">
      <h2 className="text-lg font-semibold">Change your password</h2>
      <p className="mt-1 text-sm text-muted-foreground">
        You will stay signed in on this device. Every other session is ended.
      </p>
      <ChangePasswordForm />
    </div>
  );
}
