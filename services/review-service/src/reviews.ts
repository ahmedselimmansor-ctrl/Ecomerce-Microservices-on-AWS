import type { Collection, Db } from 'mongodb';
import { createHash } from 'node:crypto';
import { logger } from './telemetry.js';
import { EMPTY_DISTRIBUTION, bucketFor, summarise } from './rating.js';


/**
 * Reviews.
 *
 * MongoDB rather than Postgres, for once with a real reason rather than
 * habit: a review is a document with a variable shape (images, structured
 * attribute ratings, merchant replies, moderation history), it is always read
 * by product, and the aggregate that the PDP needs — average, count, star
 * histogram — is a `$facet` away rather than four correlated subqueries.
 *
 * Two things here carry most of the value:
 *
 *  1. **Verified purchase is not self-reported.** It is derived from a
 *     projection of confirmed orders, built by consuming
 *     `souq.order.confirmed.v1`. A review service that trusts a
 *     `verifiedPurchase: true` in the request body is a review service that
 *     sells fake reviews to anyone who reads the API docs.
 *
 *  2. **The rating aggregate is precomputed, not aggregated on read.** A PDP
 *     is the most-requested page on the site and it needs the histogram every
 *     time. Recomputing it per request means an aggregation pipeline over
 *     every review of a bestseller on every page view.
 */

export interface Review {
  _id: string;                    // rev_<ULID>
  productId: string;
  userId: string;
  authorName: string;
  rating: 1 | 2 | 3 | 4 | 5;
  title: string;
  body: string;
  images: string[];
  verifiedPurchase: boolean;
  helpfulCount: number;
  status: 'PENDING_MODERATION' | 'PUBLISHED' | 'REJECTED';
  moderation?: {
    reason?: string;
    moderatedBy?: string;
    moderatedAt?: Date;
    autoFlags: string[];
  };
  merchantReply?: { body: string; repliedAt: Date; repliedBy: string };
  locale: string;
  createdAt: Date;
  updatedAt: Date;
}

export interface RatingSummary {
  _id: string;                    // productId
  average: number;
  count: number;
  distribution: [number, number, number, number, number];  // index 0 is 1-star
  verifiedCount: number;
  updatedAt: Date;
}

/** Projection of confirmed orders. Written by the Kafka consumer, read here. */
interface PurchaseRecord {
  _id: string;                    // sha256(userId:productId) — see purchaseKey
  userId: string;
  productId: string;
  orderId: string;
  purchasedAt: Date;
}

export class ReviewsRepository {
  private readonly reviews: Collection<Review>;
  private readonly summaries: Collection<RatingSummary>;
  private readonly purchases: Collection<PurchaseRecord>;

  constructor(db: Db) {
    this.reviews = db.collection<Review>('reviews');
    this.summaries = db.collection<RatingSummary>('rating_summaries');
    this.purchases = db.collection<PurchaseRecord>('purchases');
  }

  /**
   * Indexes.
   *
   * Called at startup. `createIndex` is idempotent, so this is safe to run on
   * every boot — and doing it in code rather than a migration means a new
   * environment cannot come up without them, which is the usual way a
   * MongoDB service ends up doing collection scans in production.
   */
  async ensureIndexes(): Promise<void> {
    await Promise.all([
      // The PDP query: published reviews for a product, newest first.
      // Compound and ordered to match, so it serves both the filter and the
      // sort from the index.
      this.reviews.createIndex(
        { productId: 1, status: 1, createdAt: -1 },
        { name: 'pdp_listing' },
      ),

      // One review per user per product. A unique partial index rather than
      // an application check: two concurrent submissions from a double-clicked
      // button both pass a `findOne` check and both insert.
      this.reviews.createIndex(
        { productId: 1, userId: 1 },
        {
          name: 'one_review_per_user_per_product',
          unique: true,
          // Rejected reviews do not block a corrected resubmission.
          partialFilterExpression: { status: { $in: ['PENDING_MODERATION', 'PUBLISHED'] } },
        },
      ),

      // The moderation queue.
      this.reviews.createIndex(
        { status: 1, createdAt: 1 },
        { name: 'moderation_queue', partialFilterExpression: { status: 'PENDING_MODERATION' } },
      ),

      this.reviews.createIndex({ userId: 1, createdAt: -1 }, { name: 'user_reviews' }),

      // Purchases expire after a year: reviewing something you bought three
      // years ago is not a verified purchase in any useful sense, and the
      // collection would otherwise grow without bound.
      this.purchases.createIndex(
        { purchasedAt: 1 },
        { name: 'purchase_ttl', expireAfterSeconds: 365 * 24 * 3600 },
      ),
      this.purchases.createIndex({ userId: 1, productId: 1 }, { name: 'purchase_lookup' }),
    ]);

    logger.info('review indexes ensured');
  }

  /**
   * Deterministic id for a purchase record, so consuming the same
   * `order.confirmed` event twice is a no-op. The inbox pattern, expressed as
   * a primary key rather than a separate table (docs/CONTRACTS.md §5.2).
   */
  private static purchaseKey(userId: string, productId: string): string {
    return createHash('sha256').update(`${userId}:${productId}`).digest('hex').slice(0, 32);
  }

  /** Called by the `souq.order.confirmed.v1` consumer. */
  async recordPurchase(userId: string, productId: string, orderId: string): Promise<void> {
    await this.purchases.updateOne(
      { _id: ReviewsRepository.purchaseKey(userId, productId) },
      {
        $setOnInsert: {
          userId, productId, orderId, purchasedAt: new Date(),
        },
      },
      { upsert: true },
    );
  }

  async hasPurchased(userId: string, productId: string): Promise<boolean> {
    const found = await this.purchases.findOne(
      { _id: ReviewsRepository.purchaseKey(userId, productId) },
      { projection: { _id: 1 } },
    );
    return found !== null;
  }

  // -------------------------------------------------------------------------

  async create(input: {
    id: string;
    productId: string;
    userId: string;
    authorName: string;
    rating: 1 | 2 | 3 | 4 | 5;
    title: string;
    body: string;
    images: string[];
    locale: string;
  }): Promise<Review> {
    // Derived, never taken from the request. This one line is the difference
    // between a trustworthy review section and a marketplace for fake ones.
    const verifiedPurchase = await this.hasPurchased(input.userId, input.productId);

    const autoFlags = autoModerate(input.title, input.body, input.rating);

    const review: Review = {
      _id: input.id,
      productId: input.productId,
      userId: input.userId,
      authorName: input.authorName,
      rating: input.rating,
      title: input.title,
      body: input.body,
      images: input.images,
      verifiedPurchase,
      helpfulCount: 0,
      // Everything queues for moderation. Auto-publishing verified purchases
      // sounds reasonable until someone buys a £2 item to earn the badge and
      // posts abuse under it.
      status: 'PENDING_MODERATION',
      moderation: { autoFlags },
      locale: input.locale,
      createdAt: new Date(),
      updatedAt: new Date(),
    };

    try {
      await this.reviews.insertOne(review);
    } catch (err) {
      // 11000 is a unique-index violation: this user has already reviewed
      // this product. A domain outcome, not a 500.
      if ((err as { code?: number }).code === 11000) {
        throw new DuplicateReview(input.productId);
      }
      throw err;
    }

    logger.info(
      { reviewId: review._id, productId: review.productId, verifiedPurchase, autoFlags },
      'review submitted',
    );
    return review;
  }

  /**
   * Publish or reject, and update the aggregate in the same logical step.
   *
   * The summary is deliberately NOT recomputed from scratch: on a bestseller
   * with 40,000 reviews that is a full collection scan per moderation action.
   * An incremental delta is exact for integers and O(1).
   */
  async moderate(
    reviewId: string,
    decision: 'PUBLISHED' | 'REJECTED',
    moderatedBy: string,
    reason?: string,
  ): Promise<void> {
    const before = await this.reviews.findOne({ _id: reviewId });
    if (!before) throw new ReviewNotFound(reviewId);
    if (before.status === decision) return;   // idempotent

    await this.reviews.updateOne(
      { _id: reviewId },
      {
        $set: {
          status: decision,
          updatedAt: new Date(),
          'moderation.moderatedBy': moderatedBy,
          'moderation.moderatedAt': new Date(),
          ...(reason ? { 'moderation.reason': reason } : {}),
        },
      },
    );

    // Publishing adds to the aggregate; un-publishing something previously
    // published removes it. Both directions matter — a review removed for
    // abuse must stop counting towards the product's rating.
    const wasCounted = before.status === 'PUBLISHED';
    const nowCounted = decision === 'PUBLISHED';
    if (wasCounted === nowCounted) return;

    const delta = nowCounted ? 1 : -1;
    await this.applyRatingDelta(before.productId, before.rating, delta, before.verifiedPurchase);
  }

  /**
   * Incremental aggregate update.
   *
   * `average` is stored but derived from a running sum held in
   * `distribution`, so it can never drift from the histogram the way two
   * independently-maintained counters would.
   */
  private async applyRatingDelta(
    productId: string,
    rating: number,
    delta: number,
    verified: boolean,
  ): Promise<void> {
    // bucketFor throws on a rating outside 1-5 rather than silently filing
    // it into bucket 0 and corrupting every future average for this product.
    const bucket = `distribution.${bucketFor(rating)}`;

    await this.summaries.updateOne(
      { _id: productId },
      {
        $inc: {
          [bucket]: delta,
          count: delta,
          ...(verified ? { verifiedCount: delta } : {}),
        },
        $set: { updatedAt: new Date() },
        $setOnInsert: { _id: productId },
      },
      { upsert: true },
    );

    // Recompute `average` from the histogram rather than maintaining a
    // separate sum. One source of truth, and the arithmetic is exact.
    const summary = await this.summaries.findOne({ _id: productId });
    if (!summary) return;

    // The arithmetic lives in ./rating.ts so it can be tested without Mongo.
    // It is the code that decides the number on every product page, and an
    // off-by-one here shifts every rating on the site by a whole star.
    const { average, count } = summarise(summary.distribution ?? EMPTY_DISTRIBUTION);

    await this.summaries.updateOne(
      { _id: productId },
      { $set: { average, count } },
    );
  }

  /** The PDP query. */
  async listForProduct(
    productId: string,
    opts: { limit: number; cursor?: string; verifiedOnly?: boolean; rating?: number },
  ): Promise<{ items: Review[]; nextCursor: string | null; hasMore: boolean }> {
    const filter: Record<string, unknown> = { productId, status: 'PUBLISHED' };
    if (opts.verifiedOnly) filter.verifiedPurchase = true;
    if (opts.rating) filter.rating = opts.rating;

    // Cursor on createdAt, not skip/limit. On a collection being written to
    // while a user pages, skip silently drops and duplicates rows.
    if (opts.cursor) {
      filter.createdAt = { $lt: new Date(Buffer.from(opts.cursor, 'base64url').toString()) };
    }

    const items = await this.reviews
      .find(filter)
      .sort({ createdAt: -1 })
      .limit(opts.limit + 1)   // one extra, to know whether another page exists
      .toArray();

    const hasMore = items.length > opts.limit;
    if (hasMore) items.pop();

    const last = items[items.length - 1];
    return {
      items,
      nextCursor: hasMore && last
        ? Buffer.from(last.createdAt.toISOString()).toString('base64url')
        : null,
      hasMore,
    };
  }

  async summary(productId: string): Promise<RatingSummary | null> {
    return this.summaries.findOne({ _id: productId });
  }

  /**
   * Rebuilds a product's aggregate from scratch.
   *
   * The incremental path above is exact, so this exists only for recovery —
   * after a restore, or if a bug is found in the delta logic. It is
   * deliberately not called on the read path.
   */
  async rebuildSummary(productId: string): Promise<RatingSummary> {
    const [result] = await this.reviews.aggregate<{
      count: number; verifiedCount: number; buckets: { _id: number; n: number }[];
    }>([
      { $match: { productId, status: 'PUBLISHED' } },
      {
        $facet: {
          totals: [{
            $group: {
              _id: null,
              count: { $sum: 1 },
              verifiedCount: { $sum: { $cond: ['$verifiedPurchase', 1, 0] } },
            },
          }],
          buckets: [{ $group: { _id: '$rating', n: { $sum: 1 } } }],
        },
      },
      {
        $project: {
          count: { $ifNull: [{ $arrayElemAt: ['$totals.count', 0] }, 0] },
          verifiedCount: { $ifNull: [{ $arrayElemAt: ['$totals.verifiedCount', 0] }, 0] },
          buckets: 1,
        },
      },
    ]).toArray();

    const distribution: [number, number, number, number, number] = [0, 0, 0, 0, 0];
    for (const b of result?.buckets ?? []) {
      if (b._id >= 1 && b._id <= 5) distribution[b._id - 1] = b.n;
    }

    const total = distribution.reduce((a, b) => a + b, 0);
    const weighted = distribution.reduce((acc, n, i) => acc + n * (i + 1), 0);

    const summary: RatingSummary = {
      _id: productId,
      average: total > 0 ? Math.round((weighted / total) * 10) / 10 : 0,
      count: total,
      distribution,
      verifiedCount: result?.verifiedCount ?? 0,
      updatedAt: new Date(),
    };

    await this.summaries.replaceOne({ _id: productId }, summary, { upsert: true });
    return summary;
  }

  /** Idempotent on (reviewId, userId): one vote per person, not per click. */
  async markHelpful(reviewId: string, userId: string, db: Db): Promise<number> {
    const votes = db.collection<{ _id: string }>('helpful_votes');
    const voteId = createHash('sha256').update(`${reviewId}:${userId}`).digest('hex').slice(0, 32);

    const result = await votes.updateOne(
      { _id: voteId },
      { $setOnInsert: { _id: voteId } },
      { upsert: true },
    );

    // upsertedCount is 0 when the vote already existed.
    if (result.upsertedCount === 0) {
      const current = await this.reviews.findOne({ _id: reviewId }, { projection: { helpfulCount: 1 } });
      return current?.helpfulCount ?? 0;
    }

    const updated = await this.reviews.findOneAndUpdate(
      { _id: reviewId },
      { $inc: { helpfulCount: 1 } },
      { returnDocument: 'after', projection: { helpfulCount: 1 } },
    );
    return updated?.helpfulCount ?? 0;
  }
}

// ---------------------------------------------------------------------------

/**
 * Cheap heuristics that flag a review for a human, never auto-reject.
 *
 * Auto-rejecting on keywords catches legitimate complaints — "this product is
 * garbage" is a real review — and silently suppressing negative feedback is
 * both dishonest and, in several jurisdictions, unlawful. These only route.
 */
function autoModerate(title: string, body: string, rating: number): string[] {
  const flags: string[] = [];
  const text = `${title} ${body}`.toLowerCase();

  if (/https?:\/\/|www\.|\.com\b/.test(text)) flags.push('CONTAINS_URL');
  if (/\b\d{10,}\b|\b[\w.]+@[\w.]+\b/.test(text)) flags.push('CONTAINS_CONTACT_DETAILS');

  // A 5-star review with almost no content is the shape of a paid review.
  if (rating === 5 && body.trim().length < 25) flags.push('SHORT_FIVE_STAR');

  // Shouting.
  const letters = body.replace(/[^a-zA-Z]/g, '');
  if (letters.length > 20 && letters === letters.toUpperCase()) flags.push('ALL_CAPS');

  // "aaaaaaaa" and similar.
  if (/(.)\1{6,}/.test(body)) flags.push('REPEATED_CHARACTERS');

  return flags;
}

export class DuplicateReview extends Error {
  constructor(productId: string) {
    super(`you have already reviewed ${productId}`);
    this.name = 'DuplicateReview';
  }
}

export class ReviewNotFound extends Error {
  constructor(id: string) {
    super(`review ${id} not found`);
    this.name = 'ReviewNotFound';
  }
}
