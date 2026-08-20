package dev.souq.catalog.api;

import java.time.Duration;
import java.util.List;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.http.CacheControl;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.PathVariable;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RequestParam;
import org.springframework.web.bind.annotation.RestController;

import dev.souq.catalog.api.Dtos.PageView;
import dev.souq.catalog.api.Dtos.ProductView;
import dev.souq.catalog.catalog.Domain.Status;
import dev.souq.catalog.catalog.ProductService;
import jakarta.servlet.http.HttpServletRequest;

/**
 * The public read API.
 *
 * <p>Everything here is cacheable at CloudFront, and the cache headers are the
 * most consequential lines in the file. A product page served from the edge
 * costs nothing and never touches Aurora; the same page with {@code no-store}
 * puts every shopper's every click through the connection pool.
 *
 * <p>The TTL is a minute rather than an hour because a price change has to
 * reach shoppers quickly — showing a stale price is a legal problem in most
 * markets, not just a bad experience. {@code stale-while-revalidate} then does
 * the real work: past the minute the edge serves the slightly-old copy
 * immediately and refreshes behind it, so a TTL expiring across a popular
 * product does not become a thundering herd of identical origin requests.
 */
@RestController
@RequestMapping("/v1/products")
public class ProductController {

    private final ProductService catalogue;
    private final int defaultPageSize;
    private final int maxPageSize;
    private final Duration productTtl;

    public ProductController(ProductService catalogue,
                             @Value("${souq.catalog.default-page-size}") int defaultPageSize,
                             @Value("${souq.catalog.max-page-size}") int maxPageSize,
                             @Value("${souq.catalog.product-cache-seconds}") int cacheSeconds) {
        this.catalogue = catalogue;
        this.defaultPageSize = defaultPageSize;
        this.maxPageSize = maxPageSize;
        this.productTtl = Duration.ofSeconds(cacheSeconds);
    }

    /**
     * A page of products.
     *
     * <p>This is not the search endpoint. Deep browsing, text queries and
     * facets all go to search-service, which paginates by cursor. This exists
     * for small category listings and for the admin grid, where a total count
     * is wanted and a cursor cannot give one.
     */
    @GetMapping
    public ResponseEntity<PageView<ProductView>> list(
            @RequestParam(required = false) String category,
            @RequestParam(required = false) String brand,
            @RequestParam(required = false) String status,
            @RequestParam(defaultValue = "0") int page,
            @RequestParam(required = false) Integer size,
            HttpServletRequest http) {

        boolean privileged = Caller.canSeeUnpublished(http);

        // Clamped rather than rejected. A client asking for 5,000 items gets
        // the maximum and a working page; a 400 just moves the failure into
        // their retry loop.
        int effectiveSize = Math.clamp(size == null ? defaultPageSize : size, 1, maxPageSize);
        int effectivePage = Math.max(0, page);

        var result = catalogue.page(category, brand,
                privileged && status != null ? Status.valueOf(status) : null,
                effectivePage, effectiveSize, privileged);

        return ResponseEntity.ok()
                .cacheControl(cacheFor(privileged))
                .body(PageView.of(result, ProductView::of));
    }

    /**
     * One product, by id or slug.
     *
     * <p>Both in one route because the storefront links by slug and every
     * service-to-service caller holds an id. Ids are prefixed {@code prd_}, so
     * there is no ambiguity and no need to try one and fall back.
     */
    @GetMapping("/{idOrSlug}")
    public ResponseEntity<ProductView> byIdOrSlug(@PathVariable String idOrSlug,
                                                  HttpServletRequest http) {
        boolean privileged = Caller.canSeeUnpublished(http);
        var product = catalogue.require(idOrSlug, privileged);

        return ResponseEntity.ok()
                .cacheControl(cacheFor(privileged))
                // ETag lets a returning browser revalidate with a 304 instead of
                // re-downloading a product page that has not changed. The
                // version column makes it exact rather than a content hash.
                .eTag("\"%s-%d\"".formatted(product.id(), product.version()))
                .body(ProductView.of(product));
    }

    /**
     * Several products at once.
     *
     * <p>The cart and order-history pages each need a handful of specific
     * products. Without this they fetch them one at a time, which is a dozen
     * round trips to render one page — the classic N+1, moved from SQL to HTTP.
     */
    @GetMapping("/batch")
    public ResponseEntity<List<ProductView>> batch(@RequestParam List<String> ids,
                                                   HttpServletRequest http) {
        if (ids.size() > 100) {
            throw new ProductService.Invalid(
                    List.of("at most 100 ids per batch, got " + ids.size()));
        }

        boolean privileged = Caller.canSeeUnpublished(http);

        var found = catalogue.batch(ids, privileged).stream()
                .map(ProductView::of)
                .toList();

        // Missing ids are omitted rather than returned as nulls. A product can
        // be archived between the cart being built and being rendered, and the
        // caller has to handle a short result anyway.
        return ResponseEntity.ok().cacheControl(cacheFor(privileged)).body(found);
    }

    private CacheControl cacheFor(boolean privileged) {
        if (privileged) {
            // An admin response can contain DRAFT products. Caching it anywhere
            // shared risks the edge serving it to a shopper.
            return CacheControl.noStore().cachePrivate();
        }
        return CacheControl.maxAge(productTtl)
                .cachePublic()
                .staleWhileRevalidate(productTtl.multipliedBy(5));
    }
}
