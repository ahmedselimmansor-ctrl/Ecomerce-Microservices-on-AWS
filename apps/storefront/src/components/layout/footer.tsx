import Link from 'next/link';

const SECTIONS = [
  {
    title: 'Shop',
    links: [
      { href: '/search', label: 'All products' },
      { href: '/search?sort=newest', label: 'New arrivals' },
      { href: '/search?inStockOnly=true', label: 'In stock' },
    ],
  },
  {
    title: 'Your account',
    links: [
      { href: '/account', label: 'Profile' },
      { href: '/account/orders', label: 'Orders' },
      { href: '/cart', label: 'Basket' },
    ],
  },
  {
    title: 'Help',
    links: [
      { href: '/help/delivery', label: 'Delivery' },
      { href: '/help/returns', label: 'Returns' },
      { href: '/help/contact', label: 'Contact us' },
    ],
  },
];

export function Footer() {
  return (
    <footer className="mt-16 border-t bg-muted/30">
      <div className="container grid grid-cols-2 gap-8 py-12 md:grid-cols-4">
        <div className="col-span-2 md:col-span-1">
          <p className="text-lg font-bold tracking-tight">SOUQ</p>
          <p className="mt-2 text-sm text-muted-foreground">
            A distributed commerce platform.
          </p>
        </div>

        {SECTIONS.map((section) => (
          <nav key={section.title} aria-labelledby={`footer-${section.title}`}>
            <h2 id={`footer-${section.title}`} className="text-sm font-semibold">
              {section.title}
            </h2>
            <ul className="mt-3 space-y-2">
              {section.links.map((link) => (
                <li key={link.href}>
                  <Link
                    href={link.href}
                    className="text-sm text-muted-foreground hover:text-foreground"
                  >
                    {link.label}
                  </Link>
                </li>
              ))}
            </ul>
          </nav>
        ))}
      </div>

      <div className="border-t">
        <div className="container flex flex-col gap-2 py-6 text-xs text-muted-foreground sm:flex-row sm:items-center sm:justify-between">
          {/*
            A fixed year, injected at build time, not `new Date().getFullYear()`.
            Calling that during render makes the server and client HTML differ
            the instant a build straddles midnight on 31 December, and React
            reports it as a hydration mismatch.
          */}
          <p>© {process.env.NEXT_PUBLIC_BUILD_YEAR ?? '2026'} SOUQ. All rights reserved.</p>
          <ul className="flex gap-4">
            <li><Link href="/legal/privacy" className="hover:text-foreground">Privacy</Link></li>
            <li><Link href="/legal/terms" className="hover:text-foreground">Terms</Link></li>
          </ul>
        </div>
      </div>
    </footer>
  );
}
