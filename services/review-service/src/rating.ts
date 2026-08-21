/**
 * Rating arithmetic, derived from the star histogram.
 *
 * Extracted from ReviewsRepository so it can be exercised without MongoDB.
 * These few lines decide the number on every product page, and an off-by-one
 * in the bucket index shifts every rating by a whole star — a bug that looks
 * like a data problem and is arithmetic.
 *
 * The histogram is the single source of truth. `average` and `count` are both
 * derived from it rather than maintained as separate counters, because two
 * counters updated independently drift the moment one write fails between them.
 */

/** `distribution[0]` is the number of 1-star ratings, `[4]` the number of 5-star. */
export type Distribution = readonly number[];

export interface RatingSummary {
  average: number;
  count: number;
}

export const EMPTY_DISTRIBUTION: Distribution = [0, 0, 0, 0, 0];

/**
 * The bucket index for a star rating.
 *
 * Ratings are 1–5 and array indices are 0–4, so this is where an off-by-one
 * lives if it lives anywhere.
 */
export function bucketFor(rating: number): number {
  if (!Number.isInteger(rating) || rating < 1 || rating > 5) {
    throw new RangeError(`rating must be an integer 1-5, got ${rating}`);
  }
  return rating - 1;
}

/**
 * Average and count from a histogram.
 *
 * Rounded to one decimal place — the precision the UI renders. Storing full
 * precision invites two clients rounding it differently and disagreeing about
 * whether a product is 4.2 or 4.3.
 *
 * A product with no ratings returns `{average: 0, count: 0}`, and the caller
 * is expected to render that as "no reviews yet" rather than as zero stars.
 * Zero stars reads as "everyone hated it"; no reviews reads as "nobody has
 * said yet", and they are very different claims to make about a new product.
 */
export function summarise(distribution: Distribution): RatingSummary {
  const count = distribution.reduce((total, n) => total + n, 0);

  if (count === 0) {
    return { average: 0, count: 0 };
  }

  const weighted = distribution.reduce((acc, n, index) => acc + n * (index + 1), 0);

  return {
    average: Math.round((weighted / count) * 10) / 10,
    count,
  };
}

/**
 * Applies a delta to a histogram.
 *
 * `delta` is +1 when a review is published and −1 when it is withdrawn or
 * rejected. Clamped at zero: a bucket cannot go negative, and if a double
 * un-publish ever gets past the caller, a floor is a far better outcome than
 * a negative count that makes the average nonsense.
 */
export function applyDelta(
  distribution: Distribution,
  rating: number,
  delta: number,
): number[] {
  const next = [...distribution];
  const index = bucketFor(rating);
  next[index] = Math.max(0, (next[index] ?? 0) + delta);
  return next;
}
