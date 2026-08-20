package dev.souq.catalog.catalog;

import java.sql.ResultSet;
import java.sql.SQLException;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Optional;

import org.springframework.jdbc.core.namedparam.MapSqlParameterSource;
import org.springframework.jdbc.core.namedparam.NamedParameterJdbcTemplate;
import org.springframework.stereotype.Repository;

import dev.souq.catalog.catalog.Domain.Image;
import dev.souq.catalog.catalog.Domain.Page;
import dev.souq.catalog.catalog.Domain.Product;
import dev.souq.catalog.catalog.Domain.Rating;
import dev.souq.catalog.catalog.Domain.Status;
import dev.souq.catalog.catalog.Domain.Variant;

/**
 * Product reads and writes.
 *
 * <p>Two decisions here are worth the words.
 *
 * <p><b>Products and variants are fetched in one query, not two.</b> A listing
 * page shows twenty products with a handful of variants each. The obvious
 * implementation selects the products and then loops selecting variants, which
 * is twenty-one round trips to Aurora for one page — the N+1 that turns a 15 ms
 * page into a 300 ms one under any real latency. The join below returns one
 * row per variant and the reducer assembles them.
 *
 * <p><b>Updates are conditional on the version.</b> Two admins editing the same
 * product is not rare; it is the normal state of a merchandising team. Without
 * the version check the second write silently discards the first, and nobody
 * finds out until someone notices a price reverted.
 */
@Repository
public class JdbcProductRepository {

    private final NamedParameterJdbcTemplate jdbc;

    public JdbcProductRepository(NamedParameterJdbcTemplate jdbc) {
        this.jdbc = jdbc;
    }

    /** Signals that the row moved under the caller. Rendered as 409. */
    public static class StaleVersion extends RuntimeException {
        public StaleVersion(String productId, int presented) {
            super("product %s has moved on since version %d".formatted(productId, presented));
        }
    }

    // ------------------------------------------------------------------ read

    private static final String SELECT_WITH_VARIANTS = """
            SELECT p.id, p.slug, p.title, p.description, p.brand, p.category_id,
                   p.attributes::text  AS p_attributes,
                   p.images::text      AS p_images,
                   p.status, p.rating_average, p.rating_count,
                   p.created_at, p.updated_at, p.version,
                   coalesce(c.path, '{}') AS category_path,
                   v.sku, v.attributes::text AS v_attributes, v.images::text AS v_images,
                   v.price, v.list_price, v.currency, v.available,
                   v.barcode, v.weight_grams, v.position, v.active
              FROM products p
              LEFT JOIN categories c ON c.id = p.category_id
              LEFT JOIN variants   v ON v.product_id = p.id
            """;

    public Optional<Product> findById(String id, boolean includeUnpublished) {
        String sql = SELECT_WITH_VARIANTS
                + " WHERE p.id = :id " + publishedClause(includeUnpublished)
                + " ORDER BY v.position, v.sku";

        return reduce(jdbc.query(sql, new MapSqlParameterSource("id", id), Rows::new))
                .stream().findFirst();
    }

    public Optional<Product> findBySlug(String slug, boolean includeUnpublished) {
        String sql = SELECT_WITH_VARIANTS
                + " WHERE p.slug = :slug " + publishedClause(includeUnpublished)
                + " ORDER BY v.position, v.sku";

        return reduce(jdbc.query(sql, new MapSqlParameterSource("slug", slug), Rows::new))
                .stream().findFirst();
    }

    public boolean slugExists(String slug) {
        Integer n = jdbc.queryForObject("SELECT count(*) FROM products WHERE slug = :slug",
                new MapSqlParameterSource("slug", slug), Integer.class);
        return n != null && n > 0;
    }

    /**
     * A page of products, optionally filtered.
     *
     * <p>Offset pagination, and that is a real limitation: page 500 makes
     * Postgres walk 12,000 rows to discard them. It is acceptable here because
     * the storefront's deep browsing goes through search-service, which paginates
     * by cursor; this endpoint exists for the admin grid and for small
     * category listings, where the total count is also wanted and a cursor
     * cannot provide one.
     */
    public Page<Product> findPage(String categoryId, String brand, Status status,
                                  int page, int size) {

        var params = new MapSqlParameterSource()
                .addValue("categoryId", categoryId)
                .addValue("brand", brand)
                .addValue("status", status == null ? null : status.name())
                .addValue("limit", size)
                .addValue("offset", (long) page * size);

        String filter = """
                 WHERE (:categoryId::text IS NULL OR p.category_id = :categoryId)
                   AND (:brand::text      IS NULL OR p.brand = :brand)
                   AND (:status::text     IS NULL OR p.status = :status)
                """;

        Long total = jdbc.queryForObject("SELECT count(*) FROM products p " + filter,
                params, Long.class);

        if (total == null || total == 0) {
            return new Page<>(List.of(), page, size, 0);
        }

        // The page of ids is chosen first, then joined to variants. Applying
        // LIMIT to the joined result would count variant rows, so a product
        // with six variants would consume six slots of a 24-item page.
        String sql = SELECT_WITH_VARIANTS + """
                 JOIN (SELECT id FROM products p
                """ + filter + """
                       ORDER BY p.updated_at DESC, p.id
                       LIMIT :limit OFFSET :offset) AS window_ids ON window_ids.id = p.id
                 ORDER BY p.updated_at DESC, p.id, v.position, v.sku
                """;

        return new Page<>(reduce(jdbc.query(sql, params, Rows::new)), page, size, total);
    }

    public List<Product> findByIds(List<String> ids) {
        if (ids.isEmpty()) {
            return List.of();
        }
        String sql = SELECT_WITH_VARIANTS
                + " WHERE p.id IN (:ids) ORDER BY p.id, v.position, v.sku";
        return reduce(jdbc.query(sql, new MapSqlParameterSource("ids", ids), Rows::new));
    }

    private static String publishedClause(boolean includeUnpublished) {
        // Admin callers see drafts and archived products; the storefront must
        // not. Expressed as a clause rather than a parameter so an accidental
        // null cannot widen the visibility.
        return includeUnpublished ? "" : " AND p.status = 'ACTIVE' ";
    }

    // ----------------------------------------------------------------- write

    public void insertProduct(Product product) {
        jdbc.update("""
                INSERT INTO products (id, slug, title, description, brand, category_id,
                                      attributes, images, status)
                VALUES (:id, :slug, :title, :description, :brand, :categoryId,
                        :attributes::jsonb, :images::jsonb, :status)
                """,
                new MapSqlParameterSource()
                        .addValue("id", product.id())
                        .addValue("slug", product.slug())
                        .addValue("title", product.title())
                        .addValue("description", product.description())
                        .addValue("brand", product.brand())
                        .addValue("categoryId", product.categoryId())
                        .addValue("attributes", Json.write(product.attributes()))
                        .addValue("images", Json.write(product.images()))
                        .addValue("status", product.status().name()));
    }

    /**
     * Updates a product if nobody else has.
     *
     * <p>The version check and the write are one statement. Reading the version,
     * comparing it in Java and then writing leaves a window in which the other
     * admin's update lands between the two — which is exactly the lost update
     * the version column exists to prevent, reintroduced by the code meant to
     * prevent it.
     *
     * @throws StaleVersion when the row has moved on
     */
    public int updateProduct(String id, int expectedVersion, String title, String description,
                             String brand, String categoryId, Map<String, String> attributes,
                             List<Image> images, Status status) {

        int updated = jdbc.update("""
                UPDATE products
                   SET title       = coalesce(:title, title),
                       description = coalesce(:description, description),
                       brand       = coalesce(:brand, brand),
                       category_id = coalesce(:categoryId, category_id),
                       attributes  = coalesce(:attributes::jsonb, attributes),
                       images      = coalesce(:images::jsonb, images),
                       status      = coalesce(:status, status),
                       updated_at  = now(),
                       version     = version + 1
                 WHERE id = :id AND version = :expectedVersion
                """,
                new MapSqlParameterSource()
                        .addValue("id", id)
                        .addValue("expectedVersion", expectedVersion)
                        .addValue("title", title)
                        .addValue("description", description)
                        .addValue("brand", brand)
                        .addValue("categoryId", categoryId)
                        .addValue("attributes", attributes == null ? null : Json.write(attributes))
                        .addValue("images", images == null ? null : Json.write(images))
                        .addValue("status", status == null ? null : status.name()));

        if (updated == 0) {
            // Zero rows means either "no such product" or "wrong version". They
            // are distinguished here because the API responses differ — 404 and
            // 409 — and guessing would tell an admin their edit conflicted when
            // in fact they mistyped the id.
            Integer exists = jdbc.queryForObject(
                    "SELECT count(*) FROM products WHERE id = :id",
                    new MapSqlParameterSource("id", id), Integer.class);
            if (exists != null && exists > 0) {
                throw new StaleVersion(id, expectedVersion);
            }
        }
        return updated;
    }

    public void upsertVariant(Variant variant) {
        jdbc.update("""
                INSERT INTO variants (sku, product_id, attributes, images, price, list_price,
                                      currency, barcode, weight_grams, position, active)
                VALUES (:sku, :productId, :attributes::jsonb, :images::jsonb, :price, :listPrice,
                        :currency, :barcode, :weightGrams, :position, :active)
                ON CONFLICT (sku) DO UPDATE SET
                    attributes = EXCLUDED.attributes,
                    images     = EXCLUDED.images,
                    price      = EXCLUDED.price,
                    list_price = EXCLUDED.list_price,
                    currency   = EXCLUDED.currency,
                    barcode    = EXCLUDED.barcode,
                    weight_grams = EXCLUDED.weight_grams,
                    position   = EXCLUDED.position,
                    active     = EXCLUDED.active,
                    updated_at = now()
                """,
                new MapSqlParameterSource()
                        .addValue("sku", variant.sku())
                        .addValue("productId", variant.productId())
                        .addValue("attributes", Json.write(variant.attributes()))
                        .addValue("images", Json.write(variant.images()))
                        .addValue("price", variant.price().amount())
                        .addValue("listPrice", variant.listPrice() == null
                                ? null : variant.listPrice().amount())
                        .addValue("currency", variant.price().currency())
                        .addValue("barcode", variant.barcode())
                        .addValue("weightGrams", variant.weightGrams())
                        .addValue("position", variant.position())
                        .addValue("active", variant.active()));
    }

    /**
     * Changes a price.
     *
     * <p>This method does <b>not</b> write {@code price_history}. A trigger on
     * {@code variants} does, and that is deliberate: three paths update a price
     * (this one, the bulk import, promotion expiry) and a fourth appears
     * whenever someone opens psql during an incident. A history with gaps is
     * worse than no history, and only the database can cover every path.
     *
     * <p>What the trigger cannot see is who and why, so both arrive as
     * transaction-local settings read by {@code current_setting()}. {@code SET
     * LOCAL} rather than {@code SET} is load-bearing: this runs on a pooled
     * connection handed to the next request on commit, and a session-scoped
     * setting would attribute the next admin's change to this one.
     *
     * <p>Adding an explicit insert here as well — the obvious thing to write —
     * produces two history rows per change, which is worse than none: it
     * silently doubles every report finance builds from this table.
     *
     * @return the previous price, or empty if the SKU does not exist
     */
    public Optional<Money> changePrice(String sku, Money newPrice, Money newListPrice,
                                       String changedBy, String reason) {

        // FOR UPDATE: two admins repricing the same SKU concurrently would
        // otherwise both read the same "old" price, and one of the two history
        // rows would record a transition that never happened.
        var previous = jdbc.query(
                "SELECT price, list_price, currency FROM variants WHERE sku = :sku FOR UPDATE",
                new MapSqlParameterSource("sku", sku),
                (rs, i) -> new Money(rs.getLong("price"), rs.getString("currency")));

        if (previous.isEmpty()) {
            return Optional.empty();
        }

        // Consumed by the variants_price_history trigger. Bound as parameters
        // rather than interpolated: set_config takes values, so a reason
        // containing a quote is data and not SQL.
        jdbc.update("SELECT set_config('souq.actor', :actor, true)",
                new MapSqlParameterSource("actor", changedBy == null ? "system" : changedBy));
        jdbc.update("SELECT set_config('souq.reason', :reason, true)",
                new MapSqlParameterSource("reason", reason == null ? "" : reason));

        jdbc.update("""
                UPDATE variants
                   SET price = :price, list_price = :listPrice, currency = :currency,
                       updated_at = now()
                 WHERE sku = :sku
                """,
                new MapSqlParameterSource()
                        .addValue("sku", sku)
                        .addValue("price", newPrice.amount())
                        .addValue("listPrice", newListPrice == null ? null : newListPrice.amount())
                        .addValue("currency", newPrice.currency()));

        return Optional.of(previous.get(0));
    }

    /**
     * Applies an availability figure from inventory-service.
     *
     * <p>Guarded on the SKU existing, and deliberately not on anything else.
     * This is the one column another service's events write, it is documented
     * as a display hint, and a stale value here is a cosmetic problem — whereas
     * failing the consumer because a product was deleted mid-flight would stall
     * the partition behind it.
     */
    public int applyAvailability(String sku, int available) {
        return jdbc.update("""
                UPDATE variants SET available = :available, updated_at = now()
                 WHERE sku = :sku AND available IS DISTINCT FROM :available
                """,
                new MapSqlParameterSource().addValue("sku", sku).addValue("available", available));
    }

    /**
     * Archives rather than deletes.
     *
     * <p>A hard delete cascades to variants, and orders reference SKUs. Losing
     * the row means an order history that cannot render what was bought.
     */
    public int archive(String id) {
        return jdbc.update("""
                UPDATE products SET status = 'ARCHIVED', updated_at = now(), version = version + 1
                 WHERE id = :id AND status <> 'ARCHIVED'
                """,
                new MapSqlParameterSource("id", id));
    }

    // ----------------------------------------------------------- row mapping

    /** One joined row: the product columns repeat per variant. */
    private record Rows(String id, String slug, String title, String description, String brand,
                        String categoryId, String pAttributes, String pImages, String status,
                        java.math.BigDecimal ratingAverage, int ratingCount,
                        java.sql.Timestamp createdAt, java.sql.Timestamp updatedAt, int version,
                        List<String> categoryPath,
                        String sku, String vAttributes, String vImages, Long price, Long listPrice,
                        String currency, Integer available, String barcode, Integer weightGrams,
                        int position, boolean active) {

        Rows(ResultSet rs, int rowNum) throws SQLException {
            this(rs.getString("id"), rs.getString("slug"), rs.getString("title"),
                    rs.getString("description"), rs.getString("brand"), rs.getString("category_id"),
                    rs.getString("p_attributes"), rs.getString("p_images"), rs.getString("status"),
                    rs.getBigDecimal("rating_average"), rs.getInt("rating_count"),
                    rs.getTimestamp("created_at"), rs.getTimestamp("updated_at"),
                    rs.getInt("version"),
                    List.of((String[]) rs.getArray("category_path").getArray()),
                    rs.getString("sku"), rs.getString("v_attributes"), rs.getString("v_images"),
                    (Long) rs.getObject("price"), (Long) rs.getObject("list_price"),
                    rs.getString("currency"), (Integer) rs.getObject("available"),
                    rs.getString("barcode"), (Integer) rs.getObject("weight_grams"),
                    rs.getInt("position"), rs.getBoolean("active"));
        }
    }

    /**
     * Collapses joined rows into products.
     *
     * <p>A {@link LinkedHashMap}, so the SQL {@code ORDER BY} survives. A plain
     * {@code HashMap} would return the page in hash order, which looks like a
     * random shuffle to anyone paging through the admin grid.
     */
    private static List<Product> reduce(List<Rows> rows) {
        Map<String, Product> assembled = new LinkedHashMap<>();
        Map<String, List<Variant>> variants = new LinkedHashMap<>();

        for (Rows row : rows) {
            variants.computeIfAbsent(row.id(), k -> new ArrayList<>());

            // A LEFT JOIN gives a null sku for a product with no variants.
            if (row.sku() != null) {
                variants.get(row.id()).add(new Variant(
                        row.sku(), row.id(), Json.readAttributes(row.vAttributes()),
                        new Money(row.price() == null ? 0 : row.price(),
                                row.currency() == null ? "EGP" : row.currency()),
                        row.listPrice() == null ? null
                                : new Money(row.listPrice(), row.currency()),
                        row.available(), Json.readImages(row.vImages()),
                        row.barcode(), row.weightGrams(), row.position(), row.active()));
            }

            assembled.computeIfAbsent(row.id(), k -> new Product(
                    row.id(), row.slug(), row.title(), row.description(), row.brand(),
                    row.categoryId(), row.categoryPath(),
                    Json.readAttributes(row.pAttributes()), Json.readImages(row.pImages()),
                    Status.valueOf(row.status()),
                    row.ratingAverage() == null ? null
                            : new Rating(row.ratingAverage().doubleValue(), row.ratingCount()),
                    List.of(),
                    row.createdAt().toInstant(), row.updatedAt().toInstant(), row.version()));
        }

        return assembled.values().stream()
                .map(p -> new Product(p.id(), p.slug(), p.title(), p.description(), p.brand(),
                        p.categoryId(), p.categoryPath(), p.attributes(), p.images(), p.status(),
                        p.rating(), List.copyOf(variants.getOrDefault(p.id(), List.of())),
                        p.createdAt(), p.updatedAt(), p.version()))
                .toList();
    }
}
