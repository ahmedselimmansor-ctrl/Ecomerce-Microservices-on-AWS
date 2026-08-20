'use client';

import { useTransition } from 'react';
import { useRouter } from 'next/navigation';

import type { SagaTrace } from '@souq/contracts';

import { SagaInspector } from './saga-inspector';

/**
 * Wires the inspector's actions to the admin API.
 *
 * A thin client boundary so `SagaInspector` itself stays a pure rendering
 * component with no knowledge of transport — which is what makes it testable
 * against a hand-written trace rather than against a running platform.
 */
export function SagaInspectorPanel({ trace, canAct }: { trace: SagaTrace; canAct: boolean }) {
  const router = useRouter();
  const [, startTransition] = useTransition();

  async function post(path: string, body?: unknown) {
    const response = await fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
    });

    if (!response.ok) {
      console.error('[saga] action failed', path, await response.text());
      alert('That action failed. See the console for detail.');
      return;
    }

    startTransition(() => router.refresh());
  }

  return (
    <SagaInspector
      trace={trace}
      canAct={canAct}
      onRetryStep={(step) =>
        void post(`/api/admin/orders/${encodeURIComponent(trace.orderId)}/retry`, { step })
      }
      onForceCancel={() =>
        void post(`/api/admin/orders/${encodeURIComponent(trace.orderId)}/cancel`)
      }
    />
  );
}
