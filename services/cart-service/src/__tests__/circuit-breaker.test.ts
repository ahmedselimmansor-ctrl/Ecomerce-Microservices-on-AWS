import { beforeEach, describe, expect, it } from 'vitest';

import { CircuitBreaker } from '../circuit-breaker.js';

/**
 * The breaker that decides whether a cart is priced or falls back to list
 * price. Every assertion here is a behaviour that costs money or availability
 * when it is wrong.
 */
describe('CircuitBreaker', () => {
  let clock: number;
  const now = () => clock;

  beforeEach(() => {
    clock = 1_000_000;
  });

  const make = (overrides = {}) =>
    new CircuitBreaker({ windowSize: 20, failureThreshold: 0.5, probeAfterMs: 10_000, now, ...overrides });

  it('starts closed and allows traffic', () => {
    const breaker = make();
    expect(breaker.currentState).toBe('closed');
    expect(breaker.allow()).toBe(true);
  });

  /**
   * The single most important property. A cold start's first few calls race
   * the pod's own warm-up; opening on them would take pricing out on every
   * deploy, when nothing is actually wrong.
   */
  it('does not open before the window is full, even on 100% failures', () => {
    const breaker = make();

    for (let i = 0; i < 19; i++) breaker.record(false);

    expect(breaker.currentState).toBe('closed');
    expect(breaker.allow()).toBe(true);
  });

  it('opens on the 20th call once the failure rate reaches the threshold', () => {
    const breaker = make();

    for (let i = 0; i < 20; i++) breaker.record(false);

    expect(breaker.currentState).toBe('open');
    expect(breaker.allow()).toBe(false);
  });

  it('stays closed at exactly under the threshold', () => {
    const breaker = make();

    // 9 failures in 20 is 45%, below 50%.
    for (let i = 0; i < 9; i++) breaker.record(false);
    for (let i = 0; i < 11; i++) breaker.record(true);

    expect(breaker.currentState).toBe('closed');
  });

  it('opens at exactly the threshold', () => {
    const breaker = make();

    // 10 in 20 is 50%, and the comparison is >=.
    for (let i = 0; i < 10; i++) breaker.record(false);
    for (let i = 0; i < 10; i++) breaker.record(true);

    expect(breaker.currentState).toBe('open');
  });

  /** Rolling, not cumulative: a service that recovers must be forgiven. */
  it('forgets old failures as the window rolls', () => {
    const breaker = make();

    for (let i = 0; i < 20; i++) breaker.record(true);   // fill it clean
    for (let i = 0; i < 9; i++) breaker.record(false);   // 9 of the last 20

    expect(breaker.currentState).toBe('closed');
  });

  describe('recovery', () => {
    const openIt = (breaker: CircuitBreaker) => {
      for (let i = 0; i < 20; i++) breaker.record(false);
    };

    it('refuses traffic until the probe delay has elapsed', () => {
      const breaker = make();
      openIt(breaker);

      clock += 9_999;
      expect(breaker.allow()).toBe(false);
      expect(breaker.currentState).toBe('open');
    });

    it('admits exactly one probe once the delay has elapsed', () => {
      const breaker = make();
      openIt(breaker);

      clock += 10_000;

      expect(breaker.allow()).toBe(true);          // the probe
      expect(breaker.currentState).toBe('half-open');

      // Everything else waits. Letting the backlog through the moment the
      // timer expires is how a recovering service is knocked back over.
      expect(breaker.allow()).toBe(false);
      expect(breaker.allow()).toBe(false);
    });

    it('closes and clears the window when the probe succeeds', () => {
      const breaker = make();
      openIt(breaker);
      clock += 10_000;
      breaker.allow();

      breaker.record(true);

      expect(breaker.currentState).toBe('closed');
      // The window was cleared, so it takes a fresh full window to reopen.
      for (let i = 0; i < 19; i++) breaker.record(false);
      expect(breaker.currentState).toBe('closed');
    });

    it('re-opens immediately when the probe fails, and restarts the delay', () => {
      const breaker = make();
      openIt(breaker);
      clock += 10_000;
      breaker.allow();

      breaker.record(false);

      expect(breaker.currentState).toBe('open');
      // The delay restarts from the failed probe, not from the original open.
      expect(breaker.allow()).toBe(false);
      clock += 10_000;
      expect(breaker.allow()).toBe(true);
    });
  });

  it('reports the state it is in', () => {
    const observed: string[] = [];
    const breaker = make({
      onOpen: () => observed.push('open'),
      onHalfOpen: () => observed.push('half-open'),
      onClose: () => observed.push('close'),
    });

    for (let i = 0; i < 20; i++) breaker.record(false);
    clock += 10_000;
    breaker.allow();
    breaker.record(true);

    expect(observed).toEqual(['open', 'half-open', 'close']);
  });
});
