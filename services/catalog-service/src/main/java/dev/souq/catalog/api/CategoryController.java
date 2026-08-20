package dev.souq.catalog.api;

import java.time.Duration;
import java.util.List;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.http.CacheControl;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.PathVariable;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RestController;

import dev.souq.catalog.api.Dtos.CategoryView;
import dev.souq.catalog.catalog.JdbcCategoryRepository;
import dev.souq.catalog.catalog.ProductService;

/**
 * The category tree.
 *
 * <p>Cached five times longer than products: the tree changes when someone
 * restructures merchandising, which is a handful of times a year, whereas a
 * price changes daily. The whole tree comes back in one response because the
 * storefront's navigation needs all of it and it is a few hundred rows.
 */
@RestController
@RequestMapping("/v1/categories")
public class CategoryController {

    private final JdbcCategoryRepository categories;
    private final Duration ttl;

    public CategoryController(JdbcCategoryRepository categories,
                              @Value("${souq.catalog.category-cache-seconds}") int cacheSeconds) {
        this.categories = categories;
        this.ttl = Duration.ofSeconds(cacheSeconds);
    }

    @GetMapping
    public ResponseEntity<List<CategoryView>> all() {
        // Already ordered parents-before-children by cardinality(path), so the
        // client builds the tree in one pass with no sorting.
        return ResponseEntity.ok()
                .cacheControl(cached())
                .body(categories.findAll().stream().map(CategoryView::of).toList());
    }

    @GetMapping("/{slug}")
    public ResponseEntity<CategoryView> bySlug(@PathVariable String slug) {
        var category = categories.findBySlug(slug)
                .orElseThrow(() -> new ProductService.NotFound("category " + slug));

        return ResponseEntity.ok().cacheControl(cached()).body(CategoryView.of(category));
    }

    /** A category and everything beneath it — one indexed query on the path array. */
    @GetMapping("/{slug}/subtree")
    public ResponseEntity<List<CategoryView>> subtree(@PathVariable String slug) {
        var found = categories.findSubtree(slug);

        if (found.isEmpty()) {
            throw new ProductService.NotFound("category " + slug);
        }

        return ResponseEntity.ok()
                .cacheControl(cached())
                .body(found.stream().map(CategoryView::of).toList());
    }

    private CacheControl cached() {
        return CacheControl.maxAge(ttl).cachePublic().staleWhileRevalidate(ttl.multipliedBy(4));
    }
}
