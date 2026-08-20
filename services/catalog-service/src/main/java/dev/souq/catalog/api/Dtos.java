package dev.souq.catalog.api;

import java.util.List;
import java.util.Map;

import dev.souq.catalog.catalog.Domain;
import dev.souq.catalog.catalog.Domain.Image;
import dev.souq.catalog.catalog.Domain.Product;
import dev.souq.catalog.catalog.Domain.Status;
import dev.souq.catalog.catalog.Domain.Variant;
import jakarta.validation.Valid;
import jakarta.validation.constraints.Max;
import jakarta.validation.constraints.Min;
import jakarta.validation.constraints.NotBlank;
import jakarta.validation.constraints.NotNull;
import jakarta.validation.constraints.Pattern;
import jakarta.validation.constraints.Size;

/**
 * Wire types.
 *
 * <p>The response records mirror {@code Product}, {@code ProductVariant} and
 * {@code Category} in libs/ts-contracts/src/api.ts field for field, because the
 * BFF parses every response with those Zod schemas. A field renamed on one side
 * and not the other produces one clean 502 with a request id, which is the
 * whole reason for validating at that boundary rather than hoping.
 *
 * <p>They are separate from the domain records rather than serialised directly.
 * {@code Product} carries {@code version} and {@code categoryId}, which are an
 * optimistic-lock token and an internal key — neither belongs in a public
 * response, and the way to guarantee that is for the public type not to have
 * the field.
 */
public final class Dtos {

    private Dtos() {}

    // --------------------------------------------------------------- output

    public record MoneyView(long amount, String currency) {
        static MoneyView of(dev.souq.catalog.catalog.Money m) {
            return m == null ? null : new MoneyView(m.amount(), m.currency());
        }
    }

    public record VariantView(
            String sku,
            Map<String, String> attributes,
            MoneyView price,
            MoneyView listPrice,
            /**
             * Denormalised from inventory-service. Null means "unknown", which
             * is not the same as 0 and must not render as "out of stock".
             */
            Integer available,
            List<Image> images) {

        static VariantView of(Variant v) {
            return new VariantView(v.sku(), v.attributes(), MoneyView.of(v.price()),
                    MoneyView.of(v.listPrice()), v.available(), v.images());
        }
    }

    public record RatingView(double average, int count) {}

    public record ProductView(
            String id,
            String sku,
            String title,
            String slug,
            String description,
            String brand,
            List<String> categoryPath,
            Map<String, String> attributes,
            List<Image> images,
            MoneyView price,
            MoneyView listPrice,
            String status,
            List<VariantView> variants,
            RatingView rating,
            String createdAt,
            String updatedAt) {

        public static ProductView of(Product p) {
            Variant primary = p.defaultVariant();

            return new ProductView(
                    p.id(),
                    primary == null ? p.id() : primary.sku(),
                    p.title(), p.slug(), p.description(), p.brand(),
                    p.categoryPath(), p.attributes(), p.images(),
                    primary == null ? null : MoneyView.of(primary.price()),
                    primary == null ? null : MoneyView.of(primary.listPrice()),
                    p.status().name(),
                    p.variants().stream().map(VariantView::of).toList(),
                    p.rating() == null ? null
                            : new RatingView(p.rating().average(), p.rating().count()),
                    p.createdAt().toString(), p.updatedAt().toString());
        }
    }

    /**
     * The admin view.
     *
     * <p>Carries {@code version}, which the public view must not: it is the
     * optimistic-lock token an admin has to send back on update. Exposing it
     * publicly would leak how often a product is edited, and — more to the
     * point — invite a client to send one on a public endpoint that has no
     * business accepting writes.
     */
    public record AdminProductView(ProductView product, int version, String categoryId) {
        public static AdminProductView of(Product p) {
            return new AdminProductView(ProductView.of(p), p.version(), p.categoryId());
        }
    }

    public record PageView<T>(List<T> items, int page, int size, long total,
                              int totalPages, boolean hasNext) {

        public static <D, V> PageView<V> of(Domain.Page<D> page,
                                            java.util.function.Function<D, V> mapper) {
            return new PageView<>(page.items().stream().map(mapper).toList(),
                    page.page(), page.size(), page.total(), page.totalPages(), page.hasNext());
        }
    }

    public record CategoryView(String id, String slug, String name, List<String> path,
                               String parentId, long productCount) {

        public static CategoryView of(Domain.Category c) {
            return new CategoryView(c.id(), c.slug(), c.name(), c.path(),
                    c.parentId(), c.productCount());
        }
    }

    // ---------------------------------------------------------------- input

    public record CreateProductRequest(
            @NotBlank @Size(max = 500) String title,
            @Size(max = 20000) String description,
            @Size(max = 200) String brand,
            @Size(max = 200) String categorySlug,
            Map<String, String> attributes,
            List<Image> images,
            @Pattern(regexp = "DRAFT|ACTIVE|ARCHIVED|DISCONTINUED") String status,
            @Valid @Size(max = 100) List<CreateVariantRequest> variants) {

        public Status statusOrDraft() {
            // Draft by default. Defaulting to ACTIVE means a half-entered
            // product is live on the storefront the moment it is saved.
            return status == null || status.isBlank() ? Status.DRAFT : Status.valueOf(status);
        }

        public List<CreateVariantRequest> variantsOrEmpty() {
            return variants == null ? List.of() : variants;
        }
    }

    public record CreateVariantRequest(
            Map<String, String> attributes,
            @NotNull @Min(0) Long price,
            @Min(0) Long listPrice,
            @NotBlank @Pattern(regexp = "^[A-Z]{3}$", message = "must be an ISO 4217 code")
            String currency,
            @Size(max = 64) String barcode,
            @Min(0) @Max(1_000_000) Integer weightGrams,
            @Min(0) Integer position,
            Boolean active) {

        public int positionOrZero() { return position == null ? 0 : position; }

        public boolean activeOrTrue() { return active == null || active; }
    }

    /**
     * An update.
     *
     * <p>{@code version} is required. Without it the endpoint would be
     * last-writer-wins, and two merchandisers editing one product — the normal
     * state of a merchandising team — would silently lose one of the edits.
     */
    public record UpdateProductRequest(
            @NotNull @Min(0) Integer version,
            @Size(max = 500) String title,
            @Size(max = 20000) String description,
            @Size(max = 200) String brand,
            @Size(max = 200) String categorySlug,
            Map<String, String> attributes,
            List<Image> images,
            @Pattern(regexp = "DRAFT|ACTIVE|ARCHIVED|DISCONTINUED") String status) {

        public Status statusOrNull() {
            return status == null || status.isBlank() ? null : Status.valueOf(status);
        }
    }

    public record ChangePriceRequest(
            @NotNull @Min(0) Long price,
            @Min(0) Long listPrice,
            @NotBlank @Pattern(regexp = "^[A-Z]{3}$") String currency,
            /** Recorded in price_history. Finance asks for it after the fact. */
            @NotBlank @Size(max = 200) String reason) {}

    public record CreateCategoryRequest(
            @NotBlank @Size(max = 200) String name,
            @Pattern(regexp = "^[a-z0-9]+(-[a-z0-9]+)*$",
                     message = "must be lowercase words separated by hyphens")
            @Size(max = 80) String slug,
            @Size(max = 64) String parentId,
            @Min(0) Integer position) {}

    public record MoveCategoryRequest(@Size(max = 64) String newParentId) {}
}
