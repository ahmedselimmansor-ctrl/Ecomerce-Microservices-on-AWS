import { describe, expect, it } from 'vitest';

import { applyDelta, bucketFor, EMPTY_DISTRIBUTION, summarise } from '../rating.js';

describe('bucketFor', () => {
  it('maps 1-5 stars onto 0-4 indices', () => {
    expect([1, 2, 3, 4, 5].map(bucketFor)).toEqual([0, 1, 2, 3, 4]);
  });

  /**
   * Throwing rather than clamping. A rating outside 1-5 means the caller has a
   * bug, and silently filing it into bucket 0 corrupts every future average
   * for that product with no error anywhere.
   */
  it('rejects anything outside the range', () => {
    for (const bad of [0, 6, -1, 1.5, NaN, Infinity]) {
      expect(() => bucketFor(bad), String(bad)).toThrow(RangeError);
    }
  });
});

describe('summarise', () => {
  it('returns zero for a product with no ratings', () => {
    expect(summarise(EMPTY_DISTRIBUTION)).toEqual({ average: 0, count: 0 });
  });

  it('averages a single rating exactly', () => {
    expect(summarise([0, 0, 0, 0, 1])).toEqual({ average: 5, count: 1 });
    expect(summarise([1, 0, 0, 0, 0])).toEqual({ average: 1, count: 1 });
  });

  /**
   * The off-by-one guard. If the weighting used the index rather than
   * index + 1, this would come out as 1.5 instead of 2.5 — every rating on
   * the site shifted down by a whole star.
   */
  it('weights each bucket by its star value, not its index', () => {
    // One 1-star and one 4-star: (1 + 4) / 2 = 2.5
    expect(summarise([1, 0, 0, 1, 0])).toEqual({ average: 2.5, count: 2 });
  });

  it('rounds to one decimal place', () => {
    // Two 4s and one 5: 13/3 = 4.333...
    expect(summarise([0, 0, 0, 2, 1]).average).toBe(4.3);
    // One 4 and two 5s: 14/3 = 4.666...
    expect(summarise([0, 0, 0, 1, 2]).average).toBe(4.7);
  });

  it('counts every rating', () => {
    expect(summarise([3, 5, 7, 11, 13]).count).toBe(39);
  });

  it('handles a large histogram without drifting', () => {
    const summary = summarise([1000, 1000, 1000, 1000, 1000]);
    expect(summary.average).toBe(3);
    expect(summary.count).toBe(5000);
  });

  /** A short histogram must not be read as five buckets of undefined. */
  it('tolerates a distribution shorter than five buckets', () => {
    expect(summarise([2, 2])).toEqual({ average: 1.5, count: 4 });
  });
});

describe('applyDelta', () => {
  it('increments the right bucket on publish', () => {
    expect(applyDelta(EMPTY_DISTRIBUTION, 5, 1)).toEqual([0, 0, 0, 0, 1]);
    expect(applyDelta(EMPTY_DISTRIBUTION, 1, 1)).toEqual([1, 0, 0, 0, 0]);
  });

  it('decrements on withdrawal', () => {
    expect(applyDelta([0, 0, 0, 0, 3], 5, -1)).toEqual([0, 0, 0, 0, 2]);
  });

  /**
   * A negative bucket makes every subsequent average nonsense, and the
   * symptom — a product rated 6.2 stars — is far from the double un-publish
   * that caused it.
   */
  it('floors at zero rather than going negative', () => {
    expect(applyDelta(EMPTY_DISTRIBUTION, 3, -1)).toEqual([0, 0, 0, 0, 0]);
  });

  it('does not mutate its input', () => {
    const original = [0, 0, 0, 0, 1];
    applyDelta(original, 1, 1);
    expect(original).toEqual([0, 0, 0, 0, 1]);
  });

  it('round-trips: publish then withdraw leaves the histogram unchanged', () => {
    const start = [1, 2, 3, 4, 5];
    const after = applyDelta(applyDelta(start, 3, 1), 3, -1);
    expect(after).toEqual(start);
    expect(summarise(after)).toEqual(summarise(start));
  });
});
