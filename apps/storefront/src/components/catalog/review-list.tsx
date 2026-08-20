import { BadgeCheck } from 'lucide-react';

import type { Review } from '@souq/contracts';

import { Badge } from '@/components/ui/badge';
import { Rating } from './rating';

/**
 * Customer reviews.
 *
 * `verifiedPurchase` is derived by review-service from the order stream, not
 * self-declared, which is what makes the badge worth anything. Rendering it for
 * a review that merely claims a purchase would be the sort of thing a consumer
 * authority treats as a misleading commercial practice.
 *
 * Body text is rendered as text, never as HTML. Reviews are attacker-controlled
 * by definition — `dangerouslySetInnerHTML` here is stored XSS with extra steps.
 */
export function ReviewList({ reviews }: { reviews: Review[] }) {
  return (
    <ul className="divide-y">
      {reviews.map((review) => (
        <li key={review.id} className="py-5">
          <div className="flex flex-wrap items-center gap-3">
            <Rating value={review.rating} showCount={false} size="sm" />

            {review.verifiedPurchase && (
              <Badge variant="success" className="gap-1">
                <BadgeCheck className="h-3 w-3" />
                Verified purchase
              </Badge>
            )}

            <span className="text-xs text-muted-foreground">
              {review.authorName || 'Anonymous'}
            </span>

            {/*
              A machine-readable date with a fixed, locale-independent display.
              A relative string ("2 days ago") rendered on the server bakes the
              server's clock into a cached page and is stale within the hour.
            */}
            <time dateTime={review.createdAt} className="text-xs text-muted-foreground">
              {new Date(review.createdAt).toLocaleDateString('en-GB', {
                year: 'numeric',
                month: 'short',
                day: 'numeric',
              })}
            </time>
          </div>

          {review.title && <h3 className="mt-2 text-sm font-medium">{review.title}</h3>}

          <p className="mt-1 whitespace-pre-line text-sm text-muted-foreground">{review.body}</p>

          {review.helpfulCount > 0 && (
            <p className="tabular mt-2 text-xs text-muted-foreground">
              {review.helpfulCount} {review.helpfulCount === 1 ? 'person' : 'people'} found this
              helpful
            </p>
          )}
        </li>
      ))}
    </ul>
  );
}
