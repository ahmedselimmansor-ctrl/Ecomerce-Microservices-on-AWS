'use client';

import { useMemo, useState } from 'react';

import type { ProductVariant } from '@souq/contracts';

import { cn } from '@/lib/utils';
import { AddToCart } from '@/components/cart/add-to-cart';
import { Price } from './price';
import { StockIndicator } from './stock-indicator';

/**
 * Picks a variant by its attributes.
 *
 * Variants carry a free-form attribute map — `{colour: "black", size: "L"}` —
 * so the picker derives its own axes rather than being told what they are. That
 * keeps it working when a category introduces an attribute nobody anticipated.
 *
 * The subtle part is what happens to an impossible combination. Picking "red"
 * when red only comes in small must not silently keep the previous size and
 * resolve to nothing. Instead the picker keeps the axis the user just touched
 * and repairs the others to the nearest variant that exists — so every click
 * lands on a real SKU and the buy box never shows an empty state.
 */
export function VariantPicker({ variants }: { variants: ProductVariant[] }) {
  const selectable = useMemo(
    () => variants.filter((v) => Object.keys(v.attributes).length > 0),
    [variants],
  );

  // Ordered axes, with their possible values, derived from the variants.
  const axes = useMemo(() => {
    const map = new Map<string, string[]>();
    for (const variant of selectable) {
      for (const [key, value] of Object.entries(variant.attributes)) {
        const values = map.get(key) ?? [];
        if (!values.includes(value)) values.push(value);
        map.set(key, values);
      }
    }
    return [...map.entries()];
  }, [selectable]);

  const [selected, setSelected] = useState<Record<string, string>>(() => {
    // Open on the cheapest variant that is not known to be out of stock. Not
    // simply the first: opening on an unavailable variant makes a purchasable
    // product look sold out.
    const initial =
      selectable.filter((v) => v.available === null || (v.available ?? 0) > 0)
        .sort((a, b) => a.price.amount - b.price.amount)[0]
      ?? selectable[0];

    return initial ? { ...initial.attributes } : {};
  });

  const current = useMemo(
    () =>
      selectable.find((variant) =>
        Object.entries(selected).every(([key, value]) => variant.attributes[key] === value),
      ),
    [selectable, selected],
  );

  function choose(axis: string, value: string) {
    // Every variant that satisfies the axis the user just clicked.
    const candidates = selectable.filter((v) => v.attributes[axis] === value);
    if (candidates.length === 0) return;

    // Among those, the one that agrees with the most of the current selection —
    // so changing colour keeps the size when that size exists in the new
    // colour, and quietly moves to a size that does when it does not.
    const best = candidates.reduce((a, b) => {
      const score = (v: ProductVariant) =>
        Object.entries(selected).filter(([k, val]) => k !== axis && v.attributes[k] === val).length;
      return score(b) > score(a) ? b : a;
    });

    setSelected({ ...best.attributes });
  }

  if (axes.length === 0 || !current) {
    const fallback = variants[0];
    return fallback ? <AddToCart sku={fallback.sku} available={fallback.available} /> : null;
  }

  return (
    <div className="space-y-5">
      {axes.map(([axis, values]) => (
        <fieldset key={axis}>
          <legend className="text-sm font-medium capitalize">
            {axis.replace(/_/g, ' ')}
            <span className="ml-1.5 font-normal text-muted-foreground">{selected[axis]}</span>
          </legend>

          <div className="mt-2 flex flex-wrap gap-2">
            {values.map((value) => {
              const isSelected = selected[axis] === value;

              // Whether this value leads anywhere buyable at all. A value whose
              // every variant is a confirmed zero is marked, not hidden —
              // hiding it makes the option list change shape as you click
              // through it, which is disorienting.
              const anyAvailable = selectable.some(
                (v) => v.attributes[axis] === value && (v.available === null || (v.available ?? 0) > 0),
              );

              return (
                <button
                  key={value}
                  type="button"
                  onClick={() => choose(axis, value)}
                  aria-pressed={isSelected}
                  className={cn(
                    'rounded-md border px-3 py-2 text-sm transition-colors',
                    'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2',
                    isSelected
                      ? 'border-primary bg-primary text-primary-foreground'
                      : 'hover:border-foreground/40',
                    !anyAvailable && !isSelected && 'text-muted-foreground line-through',
                  )}
                >
                  {value}
                  {!anyAvailable && <span className="sr-only"> (out of stock)</span>}
                </button>
              );
            })}
          </div>
        </fieldset>
      ))}

      {/*
        The price and stock re-render with the selection. Showing one variant's
        price above a picker set to another is the bug this component exists to
        avoid, so both live inside it.
      */}
      <div className="space-y-2">
        <Price price={current.price} listPrice={current.listPrice} size="lg" />
        <StockIndicator available={current.available} />
      </div>

      {/*
        Keyed on the SKU so the quantity resets when the variant changes.
        Carrying "12" over from a variant with 400 in stock to one with 2 sets
        the customer up for a rejection at checkout.
      */}
      <AddToCart key={current.sku} sku={current.sku} available={current.available} />
    </div>
  );
}
