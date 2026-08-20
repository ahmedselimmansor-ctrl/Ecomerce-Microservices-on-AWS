'use client';

import { useState, useTransition } from 'react';
import { useRouter } from 'next/navigation';
import { Loader2 } from 'lucide-react';

import { Badge } from '@/components/ui/badge';
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu';

const STATUSES = ['DRAFT', 'ACTIVE', 'ARCHIVED', 'DISCONTINUED'] as const;
type Status = (typeof STATUSES)[number];

const TONE: Record<Status, 'default' | 'secondary' | 'success' | 'destructive' | 'warning'> = {
  DRAFT: 'secondary',
  ACTIVE: 'success',
  ARCHIVED: 'destructive',
  DISCONTINUED: 'warning',
};

/**
 * Publish, unpublish or retire a product.
 *
 * The `version` read with the row is sent back with the change. If someone else
 * saved in the meantime, catalog-service returns 409 and this says so rather
 * than retrying with a fresh version — retrying would reapply the change
 * against a product the operator never looked at.
 */
export function ProductStatusControl({
  productId,
  status,
  version,
}: {
  productId: string;
  status: string;
  version: number;
}) {
  const router = useRouter();
  const [, startTransition] = useTransition();
  const [busy, setBusy] = useState(false);

  async function change(next: Status) {
    if (next === status) return;
    setBusy(true);

    try {
      const response = await fetch(`/api/admin/catalog/${encodeURIComponent(productId)}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ status: next, version }),
      });

      if (response.status === 409) {
        alert(
          'Someone else saved this product while you were looking at it. '
          + 'Refreshing so you can see their change first.',
        );
        startTransition(() => router.refresh());
        return;
      }

      if (!response.ok) throw new Error(await response.text());

      startTransition(() => router.refresh());
    } catch (error) {
      console.error('[catalog] status change failed', error);
      alert('Could not change the status. See the console for detail.');
    } finally {
      setBusy(false);
    }
  }

  return (
    <DropdownMenu>
      <DropdownMenuTrigger disabled={busy} className="disabled:opacity-50">
        <Badge variant={TONE[status as Status] ?? 'secondary'} className="cursor-pointer gap-1">
          {busy && <Loader2 className="h-3 w-3 animate-spin" />}
          {status}
        </Badge>
        <span className="sr-only">Change status</span>
      </DropdownMenuTrigger>

      <DropdownMenuContent align="start">
        {STATUSES.map((option) => (
          <DropdownMenuItem
            key={option}
            disabled={option === status}
            onSelect={() => void change(option)}
          >
            {option}
            {option === 'ACTIVE' && (
              <span className="ml-auto text-[10px] text-muted-foreground">visible</span>
            )}
          </DropdownMenuItem>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
