import { ProfilePanel } from '@/components/auth/profile-panel';

export const metadata = {
  title: 'Profile',
  robots: { index: false, follow: false },
};

export default function AccountPage() {
  return <ProfilePanel />;
}
