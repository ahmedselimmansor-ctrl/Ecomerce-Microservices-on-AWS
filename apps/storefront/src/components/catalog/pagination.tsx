'use client';

import { usePathname, useSearchParams } from 'next/navigation';
import Link from 'next/link';
import { ChevronLeft, ChevronRight } from 'lucide-react';

import { Button } from '@/components/ui/button';

/**
 * Page navigation.
 *
 * Real `<a>` elements, not buttons calling `router.push`. Middle-click, ctrl-click
 * and "copy link address" all work, the crawler can follow them, and the page
 * still paginates with JavaScript disabled.
 *
 * Deep pages are capped. Elasticsearch refuses `from + size` beyond
 * `index.max_result_window` (10,000 by default) and returns an error rather
 * than an empty page — so offering page 900 is offering a link to a 500.
 */
const MAX_RESULT_WINDOW = 10_000;

export function Pagination({
  page,
  size,
  total,
  totalIsLowerBound = false,
}: {
  page: number;
  size: number;
  total: number;
  totalIsLowerBound?: boolean;
}) {
  const pathname = usePathname();
  const searchParams = useSearchParams();

  const reachable = Math.min(total, MAX_RESULT_WINDOW);
  const lastPage = Math.max(1, Math.ceil(reachable / size));

  if (lastPage <= 1) return null;

  function href(target: number): string {
    const params = new URLSearchParams(searchParams.toString());
    // Page 1 is the canonical URL — `?page=1` and no page parameter are the
    // same page, and emitting both splits the ranking between two URLs.
    if (target <= 1) params.delete('page');
    else params.set('page', String(target));

    const query = params.toString();
    return query ? `${pathname}?${query}` : pathname;
  }

  return (
    <nav aria-label="Pagination" className="mt-8 flex items-center justify-center gap-2">
      <Button variant="outline" size="sm" asChild disabled={page <= 1}>
        {page <= 1 ? (
          <span aria-disabled="true" className="pointer-events-none opacity-50">
            <ChevronLeft className="h-4 w-4" />
            Previous
          </span>
        ) : (
          // rel="prev"/"next" tells a crawler these are one paginated series
          // rather than unrelated near-duplicate pages.
          <Link href={href(page - 1)} rel="prev">
            <ChevronLeft className="h-4 w-4" />
            Previous
          </Link>
        )}
      </Button>

      <span className="tabular px-3 text-sm text-muted-foreground">
        Page {page} of {totalIsLowerBound && lastPage === Math.ceil(MAX_RESULT_WINDOW / size) ? `${lastPage}+` : lastPage}
      </span>

      <Button variant="outline" size="sm" asChild disabled={page >= lastPage}>
        {page >= lastPage ? (
          <span aria-disabled="true" className="pointer-events-none opacity-50">
            Next
            <ChevronRight className="h-4 w-4" />
          </span>
        ) : (
          <Link href={href(page + 1)} rel="next">
            Next
            <ChevronRight className="h-4 w-4" />
          </Link>
        )}
      </Button>
    </nav>
  );
}
