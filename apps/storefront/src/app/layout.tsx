import type { Metadata, Viewport } from 'next';
import { Suspense } from 'react';

import './globals.css';

import { Category } from '@souq/contracts';

import { call } from '@/lib/bff';
import { CartProvider } from '@/components/cart/cart-provider';
import { SessionProvider } from '@/components/auth/session-provider';
import { Footer } from '@/components/layout/footer';
import { Header, HeaderFallback } from '@/components/layout/header';
import { Toaster } from '@/components/ui/toaster';

export const metadata: Metadata = {
  title: { default: 'SOUQ', template: '%s · SOUQ' },
  description: 'A distributed commerce platform.',
  // Required for Open Graph tags to resolve relative image paths, and Next
  // warns loudly without it during a production build.
  metadataBase: new URL(process.env.NEXT_PUBLIC_SITE_URL ?? 'http://localhost:3000'),
  robots: { index: true, follow: true },
};

export const viewport: Viewport = {
  width: 'device-width',
  initialScale: 1,
  // No maximumScale and no userScalable:false. Blocking pinch-zoom is a WCAG
  // 1.4.4 failure and it is the accessibility mistake most often shipped by
  // accident, copied from a boilerplate that predates the guideline.
  themeColor: [
    { media: '(prefers-color-scheme: light)', color: '#ffffff' },
    { media: '(prefers-color-scheme: dark)', color: '#0a0f1e' },
  ],
};

/**
 * Categories for the navigation.
 *
 * Fetched with a five-minute revalidate and tagged, so a catalogue change can
 * purge it without waiting the TTL out. Failing to load them must not take the
 * whole site down — a header with no category links is a degraded site; a 500
 * is no site.
 */
async function loadCategories(): Promise<{ slug: string; name: string }[]> {
  try {
    const categories = await call({
      service: 'catalog',
      path: '/v1/categories',
      schema: Category.array(),
      revalidate: 300,
      tags: ['categories'],
    });

    // Top level only. `path` has one element exactly when a category is a root.
    return categories
      .filter((category) => category.path.length === 1)
      .map(({ slug, name }) => ({ slug, name }));
  } catch (error) {
    console.error('[layout] category navigation unavailable, degrading', error);
    return [];
  }
}

export default async function RootLayout({ children }: { children: React.ReactNode }) {
  const categories = await loadCategories();

  return (
    <html lang="en-GB" suppressHydrationWarning>
      <body className="flex min-h-dvh flex-col">
        <SessionProvider>
          <CartProvider>
            {/*
              The header reads searchParams through SearchBox, which makes it a
              dynamic subtree. Without this Suspense boundary that would opt the
              entire route out of static rendering.
            */}
            <Suspense fallback={<HeaderFallback />}>
              <Header categories={categories} />
            </Suspense>

            <main id="main" className="flex-1">
              {children}
            </main>

            <Footer />
            <Toaster />
          </CartProvider>
        </SessionProvider>
      </body>
    </html>
  );
}
