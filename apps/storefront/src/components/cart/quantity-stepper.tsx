'use client';

import { useEffect, useState } from 'react';
import { Minus, Plus } from 'lucide-react';

import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { cn } from '@/lib/utils';

/**
 * A quantity control.
 *
 * The typed input is committed on blur or Enter, not on every keystroke. Firing
 * a mutation per character means typing "12" sends a request for 1 and then for
 * 12, and on a slow connection those can land in the wrong order — leaving a
 * basket line at 1 when the user asked for 12.
 *
 * Empty is a legal intermediate state while typing. Coercing it to 0 on the
 * first backspace removes the line out from under someone who was about to
 * type a different number.
 */
export function QuantityStepper({
  value,
  onCommit,
  min = 1,
  max = 99,
  disabled = false,
  label = 'Quantity',
  className,
}: {
  value: number;
  onCommit: (quantity: number) => void;
  min?: number;
  max?: number;
  disabled?: boolean;
  label?: string;
  className?: string;
}) {
  const [draft, setDraft] = useState(String(value));

  // Resync when the server's value differs from what we optimistically showed.
  useEffect(() => {
    setDraft(String(value));
  }, [value]);

  function commit() {
    const parsed = Number.parseInt(draft, 10);

    if (Number.isNaN(parsed)) {
      setDraft(String(value));
      return;
    }

    const clamped = Math.max(min, Math.min(max, parsed));
    setDraft(String(clamped));
    if (clamped !== value) onCommit(clamped);
  }

  return (
    <div className={cn('inline-flex items-center rounded-md border', className)}>
      <Button
        variant="ghost"
        size="icon"
        className="h-10 w-10 rounded-r-none"
        disabled={disabled || value <= min}
        onClick={() => onCommit(value - 1)}
      >
        <Minus className="h-4 w-4" />
        <span className="sr-only">Decrease {label.toLowerCase()}</span>
      </Button>

      <Input
        type="text"
        // inputMode over type="number": a number input on mobile shows the
        // right keypad but also brings spinner arrows, accepts "1e5", and
        // silently reports an empty string for any invalid value — so you
        // cannot tell "cleared" from "typed nonsense".
        inputMode="numeric"
        pattern="[0-9]*"
        value={draft}
        disabled={disabled}
        aria-label={label}
        onChange={(e) => setDraft(e.target.value.replace(/[^0-9]/g, ''))}
        onBlur={commit}
        onKeyDown={(e) => {
          if (e.key === 'Enter') {
            e.preventDefault();
            commit();
          }
        }}
        className="tabular h-10 w-14 rounded-none border-y-0 border-x text-center text-sm"
      />

      <Button
        variant="ghost"
        size="icon"
        className="h-10 w-10 rounded-l-none"
        disabled={disabled || value >= max}
        onClick={() => onCommit(value + 1)}
      >
        <Plus className="h-4 w-4" />
        <span className="sr-only">Increase {label.toLowerCase()}</span>
      </Button>
    </div>
  );
}
