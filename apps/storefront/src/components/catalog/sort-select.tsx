'use client';

import { usePathname, useRouter, useSearchParams } from 'next/navigation';

import { cn } from '@/lib/utils';

const OPTIONS = [
  { value: 'relevance', label: 'Most relevant' },
  { value: 'price_asc', label: 'Price: low to high' },
  { value: 'price_desc', label: 'Price: high to low' },
  { value: 'rating', label: 'Best rated' },
  { value: 'newest', label: 'Newest' },
] as const;

/**
 * The sort control.
 *
 * A native `<select>`, deliberately. A custom listbox here would need keyboard
 * navigation, typeahead, touch handling and a portal to escape the sticky
 * header's stacking context — all to reproduce what the platform already gives
 * for free, including the OS picker that mobile users expect.
 */
export function SortSelect({ value, className }: { value: string; className?: string }) {
  const router = useRouter();
  const pathname = usePathname();
  const searchParams = useSearchParams();

  function onChange(next: string) {
    const params = new URLSearchParams(searchParams.toString());
    params.set('sort', next);
    // Re-sorting invalidates the page number: page 3 of a price-ascending list
    // has nothing to do with page 3 of a relevance-ordered one.
    params.delete('page');
    router.replace(`${pathname}?${params.toString()}`, { scroll: false });
  }

  return (
    <>
      <label htmlFor="sort" className="sr-only">
        Sort results
      </label>
      <select
        id="sort"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className={cn(
          'h-9 rounded-md border border-input bg-background px-3 text-sm',
          'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring',
          className,
        )}
      >
        {OPTIONS.map((option) => (
          <option key={option.value} value={option.value}>
            {option.label}
          </option>
        ))}
      </select>
    </>
  );
}
