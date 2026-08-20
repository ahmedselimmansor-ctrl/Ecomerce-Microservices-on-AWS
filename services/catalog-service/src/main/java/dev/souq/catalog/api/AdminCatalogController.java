package dev.souq.catalog.api;

import java.util.List;

import org.springframework.http.HttpHeaders;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.DeleteMapping;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.PatchMapping;
import org.springframework.web.bind.annotation.PathVariable;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.PutMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RestController;

import dev.souq.catalog.api.Dtos.AdminProductView;
import dev.souq.catalog.api.Dtos.ChangePriceRequest;
import dev.souq.catalog.api.Dtos.CreateCategoryRequest;
import dev.souq.catalog.api.Dtos.CreateProductRequest;
import dev.souq.catalog.api.Dtos.CreateVariantRequest;
import dev.souq.catalog.api.Dtos.MoveCategoryRequest;
import dev.souq.catalog.api.Dtos.UpdateProductRequest;
import dev.souq.catalog.catalog.JdbcCategoryRepository;
import dev.souq.catalog.catalog.ProductService;
import dev.souq.catalog.catalog.ProductService.NewProduct;
import dev.souq.catalog.catalog.ProductService.NewVariant;
import dev.souq.catalog.catalog.Slugs;
import dev.souq.catalog.catalog.Ulid;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.validation.Valid;

/**
 * Catalogue writes.
 *
 * <p>Separated from {@link ProductController} rather than mixed in behind role
 * checks on individual methods. The split is the safety property: everything
 * under {@code /v1/admin} takes a write role on its first line, and a new
 * endpoint added to the wrong class is visible in review as a route in the
 * wrong file — whereas a missing annotation on one method among twenty is not.
 *
 * <p>Every response is {@code no-store}. These bodies carry DRAFT products and
 * the optimistic-lock version, and neither belongs in a shared cache.
 */
@RestController
@RequestMapping("/v1/admin")
public class AdminCatalogController {

    private final ProductService catalogue;
    private final JdbcCategoryRepository categories;

    public AdminCatalogController(ProductService catalogue, JdbcCategoryRepository categories) {
        this.catalogue = catalogue;
        this.categories = categories;
    }

    // ------------------------------------------------------------- products

    @PostMapping("/products")
    public ResponseEntity<AdminProductView> create(@Valid @RequestBody CreateProductRequest body,
                                                   HttpServletRequest http) {
        Caller.writer(http);

        var product = catalogue.create(new NewProduct(
                body.title(), body.description(), body.brand(), body.categorySlug(),
                body.attributes(), body.images(), body.statusOrDraft(),
                body.variantsOrEmpty().stream().map(AdminCatalogController::toNewVariant).toList()));

        return ResponseEntity
                .created(java.net.URI.create("/v1/products/" + product.slug()))
                .header(HttpHeaders.CACHE_CONTROL, "no-store")
                .body(AdminProductView.of(product));
    }

    /**
     * Updates a product.
     *
     * <p>{@code version} is required in the body and the update fails with 409
     * if it has moved on. That is the difference between two merchandisers
     * editing one product safely and one of them silently losing their work.
     */
    @PatchMapping("/products/{id}")
    public ResponseEntity<AdminProductView> update(@PathVariable String id,
                                                   @Valid @RequestBody UpdateProductRequest body,
                                                   HttpServletRequest http) {
        Caller.writer(http);

        var product = catalogue.update(id, body.version(), body.title(), body.description(),
                body.brand(), body.categorySlug(), body.attributes(), body.images(),
                body.statusOrNull());

        return noStore(AdminProductView.of(product));
    }

    @GetMapping("/products/{id}")
    public ResponseEntity<AdminProductView> get(@PathVariable String id, HttpServletRequest http) {
        Caller.writer(http);
        return noStore(AdminProductView.of(catalogue.require(id, true)));
    }

    /**
     * Archives a product.
     *
     * <p>{@code DELETE} in HTTP terms, an {@code UPDATE} in the database.
     * Orders reference SKUs, so a hard delete would leave an order history that
     * cannot render what was bought. The {@code product_deleted} event and its
     * compaction tombstone still go out, so read models drop it.
     */
    @DeleteMapping("/products/{id}")
    public ResponseEntity<Void> archive(@PathVariable String id, HttpServletRequest http) {
        Caller.writer(http);
        catalogue.archive(id);
        return ResponseEntity.noContent().build();
    }

    // ------------------------------------------------------------- variants

    @PutMapping("/products/{productId}/variants/{sku}")
    public ResponseEntity<AdminProductView> upsertVariant(
            @PathVariable String productId, @PathVariable String sku,
            @Valid @RequestBody CreateVariantRequest body, HttpServletRequest http) {

        Caller.writer(http);
        return noStore(AdminProductView.of(
                catalogue.upsertVariant(productId, sku, toNewVariant(body))));
    }

    @PostMapping("/products/{productId}/variants")
    public ResponseEntity<AdminProductView> addVariant(
            @PathVariable String productId,
            @Valid @RequestBody CreateVariantRequest body, HttpServletRequest http) {

        Caller.writer(http);
        return noStore(AdminProductView.of(
                catalogue.upsertVariant(productId, null, toNewVariant(body))));
    }

    /**
     * Changes a price.
     *
     * <p>A separate endpoint rather than a field on the update, because a price
     * change writes {@code price_history} and emits its own event. Folding it
     * into a general PATCH means every no-op save writes an audit row, and the
     * history stops being usable evidence of what a price actually was.
     */
    @PostMapping("/products/{productId}/variants/{sku}/price")
    public ResponseEntity<AdminProductView> changePrice(
            @PathVariable String productId, @PathVariable String sku,
            @Valid @RequestBody ChangePriceRequest body, HttpServletRequest http) {

        var caller = Caller.writer(http);

        var product = catalogue.changePrice(productId, sku, body.price(), body.listPrice(),
                body.currency(), caller.userId(), body.reason());

        return noStore(AdminProductView.of(product));
    }

    // ----------------------------------------------------------- categories

    /**
     * Creates a category.
     *
     * <p>Restructuring the tree moves every product beneath it, so this needs
     * more than a merchandiser's role — see docs/CONTRACTS.md §7.
     */
    @PostMapping("/categories")
    public ResponseEntity<Dtos.CategoryView> createCategory(
            @Valid @RequestBody CreateCategoryRequest body, HttpServletRequest http) {

        Caller.admin(http);

        String id = Ulid.category();
        String slug = body.slug() != null && !body.slug().isBlank()
                ? body.slug()
                : Slugs.uniqueFrom(body.name(), id, categories::slugExists);

        if (categories.slugExists(slug)) {
            throw new ProductService.Invalid(List.of("a category with slug '%s' already exists"
                    .formatted(slug)));
        }

        categories.insert(id, slug, body.name(), body.parentId(),
                body.position() == null ? 0 : body.position());

        var created = categories.findById(id)
                .orElseThrow(() -> new ProductService.NotFound("category " + id));

        return ResponseEntity
                .created(java.net.URI.create("/v1/categories/" + slug))
                .header(HttpHeaders.CACHE_CONTROL, "no-store")
                .body(Dtos.CategoryView.of(created));
    }

    /**
     * Re-parents a category.
     *
     * <p>One statement rewrites the whole subtree's paths. Doing it row by row
     * in application code leaves the tree inconsistent if the process dies
     * partway, and a broken path array silently drops products out of listings
     * with no error anywhere.
     */
    @PostMapping("/categories/{id}/move")
    public ResponseEntity<Dtos.CategoryView> moveCategory(
            @PathVariable String id, @Valid @RequestBody MoveCategoryRequest body,
            HttpServletRequest http) {

        Caller.admin(http);
        categories.move(id, body.newParentId());

        var moved = categories.findById(id)
                .orElseThrow(() -> new ProductService.NotFound("category " + id));

        return noStore(Dtos.CategoryView.of(moved));
    }

    /**
     * Deletes an empty leaf category.
     *
     * <p>Both emptiness guards are inside the {@code DELETE}'s {@code WHERE}
     * clause, so a product filed into it concurrently cannot slip through the
     * window between a check and the delete.
     */
    @DeleteMapping("/categories/{id}")
    public ResponseEntity<Void> deleteCategory(@PathVariable String id, HttpServletRequest http) {
        Caller.admin(http);

        if (!categories.deleteIfEmpty(id)) {
            throw new ProductService.Invalid(List.of(
                    "a category can only be deleted once it has no children and no products"));
        }
        return ResponseEntity.noContent().build();
    }

    // -----------------------------------------------------------------------

    private static NewVariant toNewVariant(CreateVariantRequest v) {
        return new NewVariant(v.attributes(), v.price(), v.listPrice(), v.currency(),
                v.barcode(), v.weightGrams(), v.positionOrZero(), v.activeOrTrue());
    }

    private static <T> ResponseEntity<T> noStore(T body) {
        return ResponseEntity.ok().header(HttpHeaders.CACHE_CONTROL, "no-store").body(body);
    }
}
