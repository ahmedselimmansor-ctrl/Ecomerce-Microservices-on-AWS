package dev.souq.catalog.catalog;

import java.util.List;
import java.util.Map;
import java.util.Optional;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.stereotype.Service;
import org.springframework.transaction.annotation.Transactional;

import dev.souq.catalog.catalog.Domain.Image;
import dev.souq.catalog.catalog.Domain.Product;
import dev.souq.catalog.catalog.Domain.Status;
import dev.souq.catalog.catalog.Domain.Variant;
import dev.souq.catalog.event.CatalogEvents;

/**
 * The write side of the catalogue.
 *
 * <p>Every method is one transaction containing both the database write and the
 * outbox row. That is the whole design: there is no path in this class that
 * changes a product without emitting the event describing the change, because
 * the two are the same commit and neither can happen alone.
 */
@Service
public class ProductService {

    private static final Logger log = LoggerFactory.getLogger(ProductService.class);

    private final JdbcProductRepository products;
    private final JdbcCategoryRepository categories;
    private final CatalogEvents events;
    private final List<String> allowedImageHosts;

    public ProductService(JdbcProductRepository products,
                          JdbcCategoryRepository categories,
                          CatalogEvents events,
                          List<String> allowedImageHosts) {
        this.products = products;
        this.categories = categories;
        this.events = events;
        this.allowedImageHosts = allowedImageHosts;
    }

    public static class NotFound extends RuntimeException {
        public NotFound(String what) { super(what + " does not exist"); }
    }

    public static class Invalid extends RuntimeException {
        private final List<String> problems;
        public Invalid(List<String> problems) {
            super(String.join("; ", problems));
            this.problems = problems;
        }
        public List<String> problems() { return problems; }
    }

    // ------------------------------------------------------------------ read

    public Product require(String idOrSlug, boolean includeUnpublished) {
        // Ids are prefixed, so which lookup to use is unambiguous and there is
        // no need to try one and fall back to the other.
        Optional<Product> found = idOrSlug.startsWith("prd_")
                ? products.findById(idOrSlug, includeUnpublished)
                : products.findBySlug(idOrSlug, includeUnpublished);

        return found.orElseThrow(() -> new NotFound("product " + idOrSlug));
    }

    public Domain.Page<Product> page(String categorySlug, String brand, Status status,
                                     int page, int size, boolean includeUnpublished) {
        String categoryId = null;
        if (categorySlug != null && !categorySlug.isBlank()) {
            categoryId = categories.findBySlug(categorySlug)
                    .orElseThrow(() -> new NotFound("category " + categorySlug))
                    .id();
        }

        // A public caller may only ever see ACTIVE, whatever they asked for.
        Status effective = includeUnpublished ? status : Status.ACTIVE;
        return products.findPage(categoryId, brand, effective, page, size);
    }

    /**
     * Several products by id, in one query.
     *
     * <p>The cart and the order-history page each need a specific handful.
     * Without this they fetch one at a time — the classic N+1, relocated from
     * SQL to HTTP, where each hop also costs a TLS handshake and a hop through
     * the mesh.
     *
     * <p>Ids that do not resolve are omitted rather than returned as nulls. A
     * product can be archived between a cart being built and being rendered,
     * so the caller has to cope with a short result in any case, and a null in
     * a typed array is the more surprising of the two.
     */
    public List<Product> batch(List<String> ids, boolean includeUnpublished) {
        if (ids == null || ids.isEmpty()) {
            return List.of();
        }

        var found = products.findByIds(ids.stream().distinct().toList());

        return includeUnpublished
                ? found
                : found.stream().filter(Product::isPubliclyVisible).toList();
    }

    // ----------------------------------------------------------------- write

    public record NewProduct(String title, String description, String brand, String categorySlug,
                             Map<String, String> attributes, List<Image> images,
                             Status status, List<NewVariant> variants) {}

    public record NewVariant(Map<String, String> attributes, long price, Long listPrice,
                             String currency, String barcode, Integer weightGrams,
                             int position, boolean active) {}

    /**
     * Creates a product and its variants.
     *
     * <p>Validation runs before anything is written, so a partially valid
     * request leaves nothing behind. Doing it per-variant as they are inserted
     * would create the product and the first two variants and then fail on the
     * third, which is worse than either outcome.
     */
    @Transactional
    public Product create(NewProduct request) {
        validate(request.attributes(), request.images(), request.variants());

        String categoryId = null;
        List<String> categoryPath = List.of();
        if (request.categorySlug() != null && !request.categorySlug().isBlank()) {
            var category = categories.findBySlug(request.categorySlug())
                    .orElseThrow(() -> new NotFound("category " + request.categorySlug()));
            categoryId = category.id();
            categoryPath = category.path();
        }

        String id = Ulid.product();
        String slug = Slugs.uniqueFrom(request.title(), id, products::slugExists);

        var now = java.time.Instant.now();
        var product = new Product(id, slug, request.title(),
                request.description() == null ? "" : request.description(),
                request.brand(), categoryId, categoryPath,
                Json.ordered(request.attributes()),
                request.images() == null ? List.of() : request.images(),
                request.status() == null ? Status.DRAFT : request.status(),
                null, List.of(), now, now, 0);

        products.insertProduct(product);

        var created = request.variants().stream()
                .map(v -> new Variant(Ulid.sku(), id, Json.ordered(v.attributes()),
                        Money.of(v.price(), v.currency()),
                        v.listPrice() == null ? null : Money.of(v.listPrice(), v.currency()),
                        null, List.of(), v.barcode(), v.weightGrams(), v.position(), v.active()))
                .toList();

        created.forEach(products::upsertVariant);

        var complete = withVariants(product, created);
        events.productUpserted(complete);

        log.info("created product {} ({}) with {} variant(s)", id, slug, created.size());
        return complete;
    }

    /**
     * Updates a product.
     *
     * <p>The caller must present the version it read. Two merchandisers editing
     * the same product at once is the normal state of the world, and without
     * this the second save silently discards the first — a class of bug nobody
     * reports because it looks like someone changing their mind.
     *
     * @throws JdbcProductRepository.StaleVersion when the row has moved on
     */
    @Transactional
    public Product update(String id, int expectedVersion, String title, String description,
                          String brand, String categorySlug, Map<String, String> attributes,
                          List<Image> images, Status status) {

        validate(attributes, images, List.of());

        String categoryId = null;
        if (categorySlug != null && !categorySlug.isBlank()) {
            categoryId = categories.findBySlug(categorySlug)
                    .orElseThrow(() -> new NotFound("category " + categorySlug)).id();
        }

        int updated = products.updateProduct(id, expectedVersion, title, description, brand,
                categoryId, attributes, images, status);

        if (updated == 0) {
            throw new NotFound("product " + id);
        }

        // Re-read rather than reconstruct. The row now holds the merged result
        // of the coalesce()s, and the event must carry what is actually stored
        // — a hand-built payload would publish the request, not the truth.
        var product = products.findById(id, true).orElseThrow(() -> new NotFound("product " + id));
        events.productUpserted(product);
        return product;
    }

    /**
     * Changes a price.
     *
     * <p>Emits two events. {@code price_changed} is the narrow one the
     * repricing and price-alert consumers subscribe to; {@code product_upserted}
     * refreshes the compacted snapshot so a search index rebuilt tomorrow shows
     * the new price. Emitting only the first leaves the snapshot stale forever,
     * because compaction keeps the last message per key and that message would
     * still hold the old price.
     */
    @Transactional
    public Product changePrice(String productId, String sku, long price, Long listPrice,
                               String currency, String changedBy, String reason) {

        if (price < 0) {
            throw new Invalid(List.of("price must not be negative"));
        }
        if (listPrice != null && listPrice < price) {
            // Also a CHECK constraint. Caught here so the admin gets a field
            // error rather than a 500 from a constraint violation.
            throw new Invalid(List.of(
                    "the reference price must not be below the selling price"));
        }

        var newPrice = Money.of(price, currency);
        var oldPrice = products.changePrice(sku,
                        newPrice,
                        listPrice == null ? null : Money.of(listPrice, currency),
                        changedBy, reason)
                .orElseThrow(() -> new NotFound("variant " + sku));

        var product = products.findById(productId, true)
                .orElseThrow(() -> new NotFound("product " + productId));

        events.priceChanged(productId, sku, oldPrice, newPrice);
        events.productUpserted(product);

        log.info("price for {} changed from {} to {} by {} ({})",
                sku, oldPrice.amount(), newPrice.amount(), changedBy, reason);
        return product;
    }

    @Transactional
    public Product upsertVariant(String productId, String sku, NewVariant request) {
        var product = products.findById(productId, true)
                .orElseThrow(() -> new NotFound("product " + productId));

        var variant = new Variant(sku == null ? Ulid.sku() : sku, productId,
                Json.ordered(request.attributes()),
                Money.of(request.price(), request.currency()),
                request.listPrice() == null ? null
                        : Money.of(request.listPrice(), request.currency()),
                null, List.of(), request.barcode(), request.weightGrams(),
                request.position(), request.active());

        products.upsertVariant(variant);

        var refreshed = products.findById(productId, true).orElse(product);
        events.productUpserted(refreshed);
        return refreshed;
    }

    /**
     * Archives a product.
     *
     * <p>Not a delete. Orders reference SKUs, and an order history that cannot
     * render what was bought is worse than a row nobody browses. The
     * {@code product_deleted} event still goes out, with its compaction
     * tombstone, so downstream read models drop it.
     */
    @Transactional
    public void archive(String id) {
        if (products.archive(id) == 0) {
            throw new NotFound("product " + id);
        }
        events.productDeleted(id);
        log.info("archived product {}", id);
    }

    /**
     * Applies an availability figure from inventory-service.
     *
     * <p>No event is emitted. Availability is denormalised <em>from</em> another
     * service, so republishing it would put catalog-service's copy of
     * inventory's number back on the bus as if it were catalogue state — and
     * any consumer reading both would see two sources for one fact, arriving in
     * an order nobody controls.
     */
    @Transactional
    public void applyAvailability(String sku, int available) {
        products.applyAvailability(sku, available);
    }

    // -----------------------------------------------------------------------

    private void validate(Map<String, String> attributes, List<Image> images,
                          List<NewVariant> variants) {
        var problems = new java.util.ArrayList<String>();
        problems.addAll(Json.validateAttributes(attributes));
        problems.addAll(Json.validateImages(images, allowedImageHosts));

        for (NewVariant variant : variants) {
            problems.addAll(Json.validateAttributes(variant.attributes()));
            if (variant.price() < 0) {
                problems.add("variant price must not be negative");
            }
            if (variant.listPrice() != null && variant.listPrice() < variant.price()) {
                problems.add("a variant's reference price must not be below its selling price");
            }
            if (variant.currency() == null || variant.currency().length() != 3) {
                problems.add("variant currency must be an ISO 4217 alphabetic code");
            }
        }

        if (!problems.isEmpty()) {
            throw new Invalid(problems);
        }
    }

    private static Product withVariants(Product product, List<Variant> variants) {
        return new Product(product.id(), product.slug(), product.title(), product.description(),
                product.brand(), product.categoryId(), product.categoryPath(),
                product.attributes(), product.images(), product.status(), product.rating(),
                variants, product.createdAt(), product.updatedAt(), product.version());
    }
}
