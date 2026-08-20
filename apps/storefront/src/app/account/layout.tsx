import Link from 'next/link';

import { Separator } from '@/components/ui/separator';

/**
 * Account section shell.
 *
 * No authentication check here. A layout renders before its page and cannot
 * read the page's data, so a redirect from here would fire on a stale session
 * and bounce a signed-in user to the login screen. Each page checks for itself,
 * and the BFF returns 401 regardless — the client check is a courtesy, not a
 * control.
 */
export default function AccountLayout({ children }: { children: React.ReactNode }) {
  return (
    <div className="container py-8">
      <h1 className="text-2xl font-bold tracking-tight">Your account</h1>

      <nav aria-label="Account" className="mt-4">
        <ul className="flex gap-1">
          {[
            { href: '/account', label: 'Profile' },
            { href: '/account/orders', label: 'Orders' },
          ].map((item) => (
            <li key={item.href}>
              <Link
                href={item.href}
                className="rounded-md px-3 py-2 text-sm text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
              >
                {item.label}
              </Link>
            </li>
          ))}
        </ul>
      </nav>

      <Separator className="mt-2" />

      <div className="mt-6">{children}</div>
    </div>
  );
}
