'use client';

import { useCallback, useTransition } from 'react';
import { usePathname, useRouter, useSearchParams } from 'next/navigation';
import { Loader2, X } from 'lucide-react';

import type { SearchFacet } from '@souq/contracts';

import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from '@/components/ui/accordion';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Checkbox } from '@/components/ui/checkbox';
import { Label } from '@/components/ui/label';

/**
 * Faceted filtering.
 *
 * Every filter is in the URL and nowhere else. That is the whole design, and it
 * buys four things that component state cannot: the back button works, a
 * filtered listing can be shared or bookmarked, a reload keeps the filters, and
 * the server component above can render the results without a client round
 * trip.
 *
 * `useTransition` keeps the old results on screen while the new ones load
 * instead of flashing a skeleton. Someone ticking three boxes in a row should
 * see the counts settle, not the page blink three times.
 */
export function Facets({
  facets,
  className,
}: {
  facets: SearchFacet[];
  className?: string;
}) {
  const router = useRouter();
  const pathname = usePathname();
  const searchParams = useSearchParams();
  const [pending, startTransition] = useTransition();

  const toggle = useCallback(
    (field: string, value: string, next: boolean) => {
      const params = new URLSearchParams(searchParams.toString());
      const current = params.getAll(field);

      params.delete(field);
      const updated = next ? [...current, value] : current.filter((v) => v !== value);
      for (const v of updated) params.append(field, v);

      // Any filter change resets to page 1. Staying on page 7 after narrowing
      // to 40 results shows an empty page, and the user reads that as "no
      // matches" rather than as "wrong page".
      params.delete('page');

      startTransition(() => {
        // scroll: false — the filter panel is beside the results on desktop,
        // and jumping to the top of the document moves the checkbox out from
        // under the cursor mid-click.
        router.replace(`${pathname}?${params.toString()}`, { scroll: false });
      });
    },
    [pathname, router, searchParams],
  );

  const clearAll = useCallback(() => {
    const params = new URLSearchParams();
    // The query itself survives. "Clear filters" means the filters, and losing
    // the search term with them is the most common complaint about this control.
    const q = searchParams.get('q');
    if (q) params.set('q', q);

    startTransition(() => router.replace(`${pathname}?${params.toString()}`, { scroll: false }));
  }, [pathname, router, searchParams]);

  const activeCount = facets.reduce(
    (sum, facet) => sum + facet.values.filter((v) => v.selected).length,
    0,
  );

  const defaultOpen = facets
    .filter((f) => f.values.some((v) => v.selected))
    .map((f) => f.field);

  return (
    <aside className={className} aria-label="Filters">
      <div className="mb-2 flex items-center justify-between">
        <h2 className="flex items-center gap-2 text-sm font-semibold">
          Filters
          {pending && <Loader2 className="h-3.5 w-3.5 animate-spin text-muted-foreground" />}
        </h2>

        {activeCount > 0 && (
          <Button variant="ghost" size="sm" onClick={clearAll}>
            <X className="h-3.5 w-3.5" />
            Clear {activeCount}
          </Button>
        )}
      </div>

      <Accordion
        type="multiple"
        // Facets with a selection start open. A collapsed panel hiding an
        // active filter is why people end up convinced the catalogue is empty.
        defaultValue={defaultOpen.length > 0 ? defaultOpen : facets.slice(0, 3).map((f) => f.field)}
      >
        {facets.map((facet) => (
          <AccordionItem key={facet.field} value={facet.field}>
            <AccordionTrigger>{facet.label}</AccordionTrigger>
            <AccordionContent>
              <ul className="space-y-2.5">
                {facet.values.map((value) => {
                  const id = `${facet.field}:${value.value}`;
                  // A zero-count option that is not selected cannot be reached
                  // and is only noise. A selected one stays, or the user cannot
                  // untick it.
                  if (value.count === 0 && !value.selected) return null;

                  return (
                    <li key={value.value} className="flex items-center gap-2.5">
                      <Checkbox
                        id={id}
                        checked={value.selected}
                        disabled={pending}
                        onCheckedChange={(checked) =>
                          toggle(facet.field, value.value, checked === true)
                        }
                      />
                      <Label htmlFor={id} className="flex-1 cursor-pointer font-normal">
                        {value.value}
                      </Label>
                      <span className="tabular text-xs text-muted-foreground">{value.count}</span>
                    </li>
                  );
                })}
              </ul>
            </AccordionContent>
          </AccordionItem>
        ))}
      </Accordion>
    </aside>
  );
}

/** The active filters, restated above the results so they cannot be missed. */
export function ActiveFilters({ facets }: { facets: SearchFacet[] }) {
  const router = useRouter();
  const pathname = usePathname();
  const searchParams = useSearchParams();

  const active = facets.flatMap((facet) =>
    facet.values.filter((v) => v.selected).map((v) => ({ field: facet.field, label: facet.label, value: v.value })),
  );

  if (active.length === 0) return null;

  const remove = (field: string, value: string) => {
    const params = new URLSearchParams(searchParams.toString());
    const remaining = params.getAll(field).filter((v) => v !== value);
    params.delete(field);
    for (const v of remaining) params.append(field, v);
    params.delete('page');
    router.replace(`${pathname}?${params.toString()}`, { scroll: false });
  };

  return (
    <ul className="mb-4 flex flex-wrap gap-2">
      {active.map(({ field, label, value }) => (
        <li key={`${field}:${value}`}>
          <button
            type="button"
            onClick={() => remove(field, value)}
            className="rounded-full focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          >
            <Badge variant="secondary" className="gap-1 py-1 pr-1.5">
              <span className="text-muted-foreground">{label}:</span> {value}
              <X className="h-3 w-3" />
              <span className="sr-only">Remove filter</span>
            </Badge>
          </button>
        </li>
      ))}
    </ul>
  );
}
