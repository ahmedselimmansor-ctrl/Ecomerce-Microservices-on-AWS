import { notFound } from 'next/navigation';

import { LEGAL, findContent } from '@/lib/content';
import { ContentBody } from '@/components/content-body';

/**
 * Pre-rendered at build time. The copy is in the repository, so there is
 * nothing per-request about these pages and no reason to render them on demand.
 */
export function generateStaticParams() {
  return LEGAL.map((page) => ({ slug: page.slug }));
}

/** Anything not listed is a 404, not an empty page. */
export const dynamicParams = false;

export async function generateMetadata({ params }: { params: Promise<{ slug: string }> }) {
  const page = findContent(LEGAL, (await params).slug);
  if (!page) return {};

  return {
    title: page.title,
    description: page.summary,
    alternates: { canonical: `/legal/${page.slug}` },
  };
}

export default async function ContentPageRoute({ params }: { params: Promise<{ slug: string }> }) {
  const page = findContent(LEGAL, (await params).slug);
  if (!page) notFound();

  return <ContentBody page={page} />;
}
