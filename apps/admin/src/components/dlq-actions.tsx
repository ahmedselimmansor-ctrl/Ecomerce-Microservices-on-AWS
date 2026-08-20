'use client';

import { useState, useTransition } from 'react';
import { useRouter } from 'next/navigation';
import { Loader2, RotateCw, Trash2 } from 'lucide-react';

import { Button } from '@/components/ui/button';
import {
  Tooltip, TooltipContent, TooltipTrigger,
} from '@/components/ui/tooltip';

/**
 * Replay or discard one dead-lettered message.
 *
 * **Replay needs no confirmation.** Every consumer dedupes on `event_id` through
 * its inbox table, so replaying a message that did partially apply is a no-op.
 * Making a safe action feel dangerous is how people learn to click through
 * every dialog, including the one that mattered.
 *
 * **Discard does.** It is the only irreversible control in this app: the event
 * is gone, and whatever it represented — a stock adjustment, a notification —
 * never happens. The confirmation asks for the event id to be typed, not for a
 * yes/no, because a yes/no on a row of identical-looking buttons is a mis-click
 * waiting to happen.
 */
export function DlqActions({ messageId, eventId }: { messageId: string; eventId: string }) {
  const router = useRouter();
  const [pending, startTransition] = useTransition();
  const [confirming, setConfirming] = useState(false);
  const [typed, setTyped] = useState('');
  const [busy, setBusy] = useState<'replay' | 'discard' | null>(null);

  async function act(action: 'replay' | 'discard') {
    setBusy(action);
    try {
      const response = await fetch(`/api/admin/dlq/${encodeURIComponent(messageId)}/${action}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
      });
      if (!response.ok) throw new Error(await response.text());

      setConfirming(false);
      setTyped('');
      startTransition(() => router.refresh());
    } catch (error) {
      console.error(`[dlq] ${action} failed`, error);
      // Surfaced through the row itself rather than a toast: an operator acting
      // on twenty rows needs to see which one failed, not a stack of banners.
      alert(`Could not ${action} this message. See the console for detail.`);
    } finally {
      setBusy(null);
    }
  }

  if (confirming) {
    return (
      <div className="flex items-center justify-end gap-2">
        <input
          value={typed}
          onChange={(e) => setTyped(e.target.value)}
          placeholder={eventId.slice(0, 12)}
          aria-label={`Type the event id ${eventId} to confirm discarding it`}
          className="h-8 w-36 rounded-md border px-2 font-mono text-xs"
          autoFocus
        />
        <Button
          size="sm"
          variant="destructive"
          // A prefix match on the visible placeholder, so the operator does not
          // have to copy a 26-character ULID by hand — but does have to look at
          // the specific row they are about to destroy.
          disabled={!eventId.startsWith(typed) || typed.length < 8 || busy !== null}
          onClick={() => void act('discard')}
        >
          {busy === 'discard' ? <Loader2 className="animate-spin" /> : 'Discard'}
        </Button>
        <Button size="sm" variant="ghost" onClick={() => { setConfirming(false); setTyped(''); }}>
          Cancel
        </Button>
      </div>
    );
  }

  return (
    <div className="flex items-center justify-end gap-1">
      <Tooltip>
        <TooltipTrigger asChild>
          <Button
            size="sm"
            variant="outline"
            disabled={busy !== null || pending}
            onClick={() => void act('replay')}
          >
            {busy === 'replay' ? <Loader2 className="animate-spin" /> : <RotateCw />}
            Replay
          </Button>
        </TooltipTrigger>
        <TooltipContent>
          Safe to repeat — consumers dedupe on event_id.
        </TooltipContent>
      </Tooltip>

      <Tooltip>
        <TooltipTrigger asChild>
          <Button
            size="sm"
            variant="ghost"
            disabled={busy !== null || pending}
            onClick={() => setConfirming(true)}
            className="text-muted-foreground hover:text-destructive"
          >
            <Trash2 />
            <span className="sr-only">Discard message {eventId}</span>
          </Button>
        </TooltipTrigger>
        <TooltipContent>Permanent. The event will never be applied.</TooltipContent>
      </Tooltip>
    </div>
  );
}
