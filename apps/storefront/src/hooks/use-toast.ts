'use client';

import * as React from 'react';

import type { ToastActionElement, ToastProps } from '@/components/ui/toast';

/**
 * Toast state, held outside React.
 *
 * A module-level store with subscribers rather than a Context provider. The
 * reason is that `toast()` is called from event handlers, from `catch` blocks
 * and from non-component modules — places where a hook cannot be called. A
 * Context-based API forces every one of those call sites to thread a callback
 * down to it.
 */

const TOAST_LIMIT = 3;
/** Time between a toast closing and its state being dropped — long enough for the exit animation. */
const TOAST_REMOVE_DELAY = 400;

type ToasterToast = ToastProps & {
  id: string;
  title?: React.ReactNode;
  description?: React.ReactNode;
  action?: ToastActionElement;
};

let count = 0;
function nextId(): string {
  count = (count + 1) % Number.MAX_SAFE_INTEGER;
  return count.toString();
}

interface State {
  toasts: ToasterToast[];
}

const listeners: Array<(state: State) => void> = [];
let memoryState: State = { toasts: [] };

const removalTimers = new Map<string, ReturnType<typeof setTimeout>>();

function dispatch(next: State) {
  memoryState = next;
  listeners.forEach((listener) => listener(memoryState));
}

function scheduleRemoval(toastId: string) {
  // Guarded: without it, a double dismiss queues two timers and the second
  // fires against state the first already cleaned up.
  if (removalTimers.has(toastId)) return;

  removalTimers.set(
    toastId,
    setTimeout(() => {
      removalTimers.delete(toastId);
      dispatch({ toasts: memoryState.toasts.filter((t) => t.id !== toastId) });
    }, TOAST_REMOVE_DELAY),
  );
}

export function toast(props: Omit<ToasterToast, 'id'>) {
  const id = nextId();

  const dismiss = () => {
    dispatch({
      toasts: memoryState.toasts.map((t) => (t.id === id ? { ...t, open: false } : t)),
    });
    scheduleRemoval(id);
  };

  dispatch({
    toasts: [
      {
        ...props,
        id,
        open: true,
        onOpenChange: (open: boolean) => {
          if (!open) dismiss();
        },
      },
      // Oldest first out. Three is the point past which a stack of toasts
      // covers the content the user is trying to act on.
      ...memoryState.toasts,
    ].slice(0, TOAST_LIMIT),
  });

  return { id, dismiss };
}

export function useToast() {
  const [state, setState] = React.useState<State>(memoryState);

  React.useEffect(() => {
    listeners.push(setState);
    return () => {
      const index = listeners.indexOf(setState);
      if (index > -1) listeners.splice(index, 1);
    };
  }, []);

  return {
    ...state,
    toast,
    dismiss: (toastId: string) => {
      dispatch({
        toasts: memoryState.toasts.map((t) => (t.id === toastId ? { ...t, open: false } : t)),
      });
      scheduleRemoval(toastId);
    },
  };
}
