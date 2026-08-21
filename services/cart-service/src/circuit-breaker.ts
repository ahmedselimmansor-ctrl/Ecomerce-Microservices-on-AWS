/**
 * A rolling-window circuit breaker.
 *
 * Extracted from PricingClient so the policy can be exercised without a gRPC
 * channel and a proto file. It was previously inlined there, which meant the
 * only way to test "does the breaker open at the right point" was to stand up
 * a failing server.
 *
 * The tuning is docs/CONTRACTS.md §5.4: open at 50% failures over a 20-request
 * window, probe after 10 seconds.
 *
 * Three decisions in here are the ones that matter under load.
 *
 * **It only judges on a FULL window.** Opening after the first two failures of
 * a cold start would take pricing out on every deploy, when the first few
 * calls race the pod's own warm-up.
 *
 * **Half-open admits exactly one probe.** Letting the whole backlog through
 * the moment the timer expires is how a service that is recovering gets
 * knocked straight back over.
 *
 * **The clock is injected.** Not for purity — because a test that has to sleep
 * ten seconds to check the probe delay is a test nobody runs.
 */

export type BreakerState = 'closed' | 'open' | 'half-open';

export interface CircuitBreakerOptions {
  windowSize?: number;
  failureThreshold?: number;
  probeAfterMs?: number;
  /** Injected so the probe delay can be tested without waiting for it. */
  now?: () => number;
  onOpen?: (failureRate: number) => void;
  onClose?: () => void;
  onHalfOpen?: () => void;
}

export class CircuitBreaker {
  private state: BreakerState = 'closed';
  private failures: boolean[] = [];
  private openedAt = 0;

  private readonly windowSize: number;
  private readonly failureThreshold: number;
  private readonly probeAfterMs: number;
  private readonly now: () => number;
  private readonly onOpen?: (failureRate: number) => void;
  private readonly onClose?: () => void;
  private readonly onHalfOpen?: () => void;

  constructor(options: CircuitBreakerOptions = {}) {
    this.windowSize = options.windowSize ?? 20;
    this.failureThreshold = options.failureThreshold ?? 0.5;
    this.probeAfterMs = options.probeAfterMs ?? 10_000;
    this.now = options.now ?? Date.now;
    this.onOpen = options.onOpen;
    this.onClose = options.onClose;
    this.onHalfOpen = options.onHalfOpen;
  }

  /** Whether a call may proceed under the current state. */
  allow(): boolean {
    if (this.state === 'closed') return true;

    if (this.state === 'open') {
      if (this.now() - this.openedAt < this.probeAfterMs) return false;

      // One probe. If it succeeds the breaker closes; if it fails we go
      // straight back to open without a thundering herd of retries.
      this.state = 'half-open';
      this.onHalfOpen?.();
      return true;
    }

    // half-open: only the single probe is in flight.
    return false;
  }

  record(success: boolean): void {
    if (this.state === 'half-open') {
      this.state = success ? 'closed' : 'open';
      if (success) {
        this.failures = [];
        this.onClose?.();
      } else {
        this.openedAt = this.now();
        this.onOpen?.(1);
      }
      return;
    }

    this.failures.push(!success);
    if (this.failures.length > this.windowSize) this.failures.shift();

    // Only judge on a full window — see the class comment.
    if (this.failures.length < this.windowSize) return;

    const rate = this.failures.filter(Boolean).length / this.failures.length;
    if (rate >= this.failureThreshold && this.state === 'closed') {
      this.state = 'open';
      this.openedAt = this.now();
      this.onOpen?.(rate);
    }
  }

  get currentState(): BreakerState {
    return this.state;
  }
}
