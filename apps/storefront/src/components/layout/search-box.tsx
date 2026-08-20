'use client';

import { useEffect, useRef, useState } from 'react';
import { useRouter, useSearchParams } from 'next/navigation';
import Link from 'next/link';
import { Loader2, Search, X } from 'lucide-react';

import { SuggestResponse } from '@souq/contracts';

import { apiFetch } from '@/lib/api-client';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { cn } from '@/lib/utils';

type Suggestion = { text: string; type: string; productId?: string };

/**
 * Search with type-ahead.
 *
 * Three things here exist because the naive version misbehaves under real use.
 *
 * **Debounced, and the in-flight request is aborted.** Without the abort, a
 * fast typist has six requests in flight and they resolve out of order — so the
 * suggestions for "so" can land after the suggestions for "sony" and overwrite
 * them. Debouncing alone does not fix that; cancelling does.
 *
 * **A failed suggest call is swallowed.** Suggestions are a convenience. An
 * error toast because a background type-ahead request timed out is noise about
 * something the user never asked for, and the form still submits fine.
 *
 * **The form submits on Enter regardless of suggestion state.** A dropdown that
 * swallows Enter to select a highlighted item, when nothing is highlighted, is
 * the most reliable way to make a search box feel broken.
 */
export function SearchBox({ className }: { className?: string }) {
  const router = useRouter();
  const searchParams = useSearchParams();

  const [value, setValue] = useState(searchParams.get('q') ?? '');
  const [suggestions, setSuggestions] = useState<Suggestion[]>([]);
  const [open, setOpen] = useState(false);
  const [loading, setLoading] = useState(false);

  const containerRef = useRef<HTMLDivElement>(null);

  // Keep the box in step with the URL when the user navigates back.
  useEffect(() => {
    setValue(searchParams.get('q') ?? '');
  }, [searchParams]);

  useEffect(() => {
    const query = value.trim();

    // Two characters is the point below which suggestions are noise — one
    // letter matches most of the catalogue.
    if (query.length < 2) {
      setSuggestions([]);
      setLoading(false);
      return;
    }

    const controller = new AbortController();
    const timer = setTimeout(() => {
      setLoading(true);
      apiFetch(`/api/bff/suggest?q=${encodeURIComponent(query)}`, {
        schema: SuggestResponse,
        signal: controller.signal,
      })
        .then((data) => {
          setSuggestions(data.suggestions);
          setOpen(data.suggestions.length > 0);
        })
        .catch(() => {
          // Deliberately silent. See above.
          setSuggestions([]);
        })
        .finally(() => setLoading(false));
    }, 180);

    return () => {
      clearTimeout(timer);
      controller.abort();
    };
  }, [value]);

  // Close on an outside click. Radix would give this for free, but a popover
  // here would steal focus from the input on every keystroke.
  useEffect(() => {
    function onPointerDown(event: PointerEvent) {
      if (!containerRef.current?.contains(event.target as Node)) setOpen(false);
    }
    document.addEventListener('pointerdown', onPointerDown);
    return () => document.removeEventListener('pointerdown', onPointerDown);
  }, []);

  function submit(event: React.FormEvent) {
    event.preventDefault();
    setOpen(false);

    const query = value.trim();
    router.push(query ? `/search?q=${encodeURIComponent(query)}` : '/search');
  }

  return (
    <div ref={containerRef} className={cn('relative w-full', className)}>
      <form onSubmit={submit} role="search">
        <label htmlFor="site-search" className="sr-only">
          Search products
        </label>

        <div className="relative">
          <Search
            className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground"
            aria-hidden="true"
          />
          <Input
            id="site-search"
            type="search"
            value={value}
            onChange={(e) => setValue(e.target.value)}
            onFocus={() => suggestions.length > 0 && setOpen(true)}
            onKeyDown={(e) => e.key === 'Escape' && setOpen(false)}
            placeholder="Search products"
            className="pl-9 pr-9"
            autoComplete="off"
            // The listbox is not a combobox implementation — it is a set of
            // links. Announcing it as a combobox without arrow-key selection
            // would promise an interaction that is not there.
            aria-autocomplete="list"
          />

          {loading && (
            <Loader2 className="absolute right-3 top-1/2 h-4 w-4 -translate-y-1/2 animate-spin text-muted-foreground" />
          )}
          {!loading && value && (
            <Button
              variant="ghost"
              size="icon"
              className="absolute right-0 top-1/2 h-9 w-9 -translate-y-1/2"
              onClick={() => {
                setValue('');
                setSuggestions([]);
              }}
            >
              <X className="h-4 w-4" />
              <span className="sr-only">Clear search</span>
            </Button>
          )}
        </div>
      </form>

      {open && suggestions.length > 0 && (
        <ul className="absolute inset-x-0 top-full z-50 mt-1 overflow-hidden rounded-md border bg-popover py-1 shadow-md">
          {suggestions.slice(0, 8).map((suggestion) => (
            <li key={`${suggestion.type}:${suggestion.text}`}>
              <Link
                href={
                  suggestion.type === 'product' && suggestion.productId
                    ? `/products/${suggestion.productId}`
                    : `/search?q=${encodeURIComponent(suggestion.text)}`
                }
                onClick={() => setOpen(false)}
                className="flex items-center gap-2 px-3 py-2 text-sm hover:bg-accent"
              >
                <Search className="h-3.5 w-3.5 shrink-0 text-muted-foreground" aria-hidden="true" />
                <span className="truncate">{suggestion.text}</span>
                {suggestion.type !== 'query' && (
                  <span className="ml-auto shrink-0 text-xs text-muted-foreground">
                    {suggestion.type}
                  </span>
                )}
              </Link>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
