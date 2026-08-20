import { MfaEnrolment } from '@/components/auth/mfa-enrolment';

export const metadata = {
  title: 'Two-factor authentication',
  robots: { index: false, follow: false },
};

export default function SecurityPage() {
  return (
    <div className="max-w-lg">
      <h2 className="text-lg font-semibold">Two-factor authentication</h2>
      <p className="mt-1 text-sm text-muted-foreground">
        A code from your phone, in addition to your password. It is the single largest reduction in
        account-takeover risk available to you.
      </p>
      <MfaEnrolment />
    </div>
  );
}
