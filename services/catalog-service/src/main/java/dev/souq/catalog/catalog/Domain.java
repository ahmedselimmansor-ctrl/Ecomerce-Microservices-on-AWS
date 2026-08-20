package dev.souq.catalog.catalog;

import java.time.Instant;
import java.util.List;
import java.util.Map;

/**
 * The catalogue's domain types.
 *
 * <p>These mirror {@code Product}, {@code ProductVariant} and {@code Category}
 * in libs/ts-contracts/src/api.ts field for field. The storefront parses every
 * response with those Zod schemas, so a field renamed here and not there is a
 * 502 from the BFF rather than a silently missing value — which is the point of
 * validating at the boundary.
 */
public final class Domain {

    private Domain() {}

    public enum Status { DRAFT, ACTIVE, ARCHIVED, DISCONTINUED }

    public record Image(String url, String alt, Integer width, Integer height) {}

    public record Rating(double average, int count) {}

    public record Variant(
            String sku,
            String productId,
            Map<String, String> attributes,
            Money price,
            Money listPrice,
            /**
             * Denormalised from inventory-service, and therefore eventually
             * consistent. docs/CONTRACTS.md is explicit that this is a display
             * hint: the authoritative check happens when the saga reserves.
             * Null means "we have not heard from inventory", which the UI must
             * render differently from zero.
             */
            Integer available,
            List<Image> images,
            String barcode,
            Integer weightGrams,
            int position,
            boolean active) {}

    public record Product(
            String id,
            String slug,
            String title,
            String description,
            String brand,
            String categoryId,
            List<String> categoryPath,
            Map<String, String> attributes,
            List<Image> images,
            Status status,
            Rating rating,
            List<Variant> variants,
            Instant createdAt,
            Instant updatedAt,
            /** Optimistic-lock token. Every update must present the version it read. */
            int version) {

        /**
         * The variant a product page leads with.
         *
         * <p>The cheapest active one, not the first by position. A listing that
         * shows "from EGP 1,200" and then opens on a EGP 3,400 variant is the
         * single most common complaint about faceted catalogues, and it is a
         * sorting decision rather than a data problem.
         */
        public Variant defaultVariant() {
            return variants.stream()
                    .filter(Variant::active)
                    .min(java.util.Comparator.comparingLong(v -> v.price().amount()))
                    .orElse(variants.isEmpty() ? null : variants.get(0));
        }

        public boolean isPubliclyVisible() {
            return status == Status.ACTIVE;
        }
    }

    public record Category(
            String id,
            String slug,
            String name,
            String parentId,
            /** Root-to-leaf, e.g. ["electronics","audio","headphones"]. */
            List<String> path,
            int position,
            long productCount) {}

    /** One page of results, matching the {@code paginated()} helper in the Zod contracts. */
    public record Page<T>(List<T> items, int page, int size, long total) {

        public int totalPages() {
            return size == 0 ? 0 : (int) ((total + size - 1) / size);
        }

        public boolean hasNext() {
            return (long) (page + 1) * size < total;
        }
    }
}
