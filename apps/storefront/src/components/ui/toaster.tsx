'use client';

import { useToast } from '@/hooks/use-toast';
import {
  Toast, ToastClose, ToastDescription, ToastProvider, ToastTitle, ToastViewport,
} from '@/components/ui/toast';

export function Toaster() {
  const { toasts } = useToast();

  return (
    // 6 seconds, not the Radix default of 5 minutes. A toast that outlives the
    // action it describes is clutter; one that vanishes in two seconds is
    // missed. Errors override this and stay until dismissed.
    <ToastProvider duration={6000} swipeDirection="right">
      {toasts.map(({ id, title, description, action, ...props }) => (
        <Toast key={id} {...props}>
          <div className="grid gap-1">
            {title && <ToastTitle>{title}</ToastTitle>}
            {description && <ToastDescription>{description}</ToastDescription>}
          </div>
          {action}
          <ToastClose />
        </Toast>
      ))}
      <ToastViewport />
    </ToastProvider>
  );
}
