import type { Metadata, Viewport } from 'next';
import Link from 'next/link';
import { Activity, AlertTriangle, Boxes, LayoutDashboard, ShoppingCart } from 'lucide-react';

import './globals.css';

import { TooltipProvider } from '@/components/ui/tooltip';

export const metadata: Metadata = {
  title: { default: 'SOUQ admin', template: '%s · SOUQ admin' },
  // Belt and braces with the X-Robots-Tag header in next.config.mjs. An
  // internal tool appearing in a search index is not a risk worth one layer.
  robots: { index: false, follow: false, nocache: true },
};

export const viewport: Viewport = { width: 'device-width', initialScale: 1 };

const NAV = [
  { href: '/', label: 'Overview', icon: LayoutDashboard },
  { href: '/orders', label: 'Orders', icon: ShoppingCart },
  { href: '/catalog', label: 'Catalogue', icon: Boxes },
  { href: '/dlq', label: 'Dead letters', icon: AlertTriangle },
];

export default function AdminLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en-GB">
      <body className="min-h-dvh bg-muted/20">
        <TooltipProvider delayDuration={200}>
          <div className="flex min-h-dvh">
            <aside className="hidden w-56 shrink-0 border-r bg-background md:block">
              <div className="flex h-14 items-center gap-2 border-b px-4">
                <Activity className="h-4 w-4 text-primary" aria-hidden="true" />
                <span className="text-sm font-bold tracking-tight">SOUQ admin</span>
              </div>

              <nav aria-label="Sections" className="p-2">
                <ul className="space-y-0.5">
                  {NAV.map((item) => (
                    <li key={item.href}>
                      <Link
                        href={item.href}
                        className="flex items-center gap-2.5 rounded-md px-3 py-2 text-sm text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
                      >
                        <item.icon className="h-4 w-4 shrink-0" aria-hidden="true" />
                        {item.label}
                      </Link>
                    </li>
                  ))}
                </ul>
              </nav>
            </aside>

            <div className="flex min-w-0 flex-1 flex-col">
              {/* Horizontal nav on narrow screens; the sidebar is hidden there. */}
              <nav aria-label="Sections" className="border-b bg-background md:hidden">
                <ul className="flex overflow-x-auto">
                  {NAV.map((item) => (
                    <li key={item.href}>
                      <Link
                        href={item.href}
                        className="flex items-center gap-2 whitespace-nowrap px-4 py-3 text-sm text-muted-foreground"
                      >
                        <item.icon className="h-4 w-4" aria-hidden="true" />
                        {item.label}
                      </Link>
                    </li>
                  ))}
                </ul>
              </nav>

              <main className="min-w-0 flex-1 p-6">{children}</main>
            </div>
          </div>
        </TooltipProvider>
      </body>
    </html>
  );
}
