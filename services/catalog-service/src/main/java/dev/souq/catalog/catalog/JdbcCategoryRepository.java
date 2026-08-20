package dev.souq.catalog.catalog;

import java.util.List;
import java.util.Optional;

import org.springframework.jdbc.core.namedparam.MapSqlParameterSource;
import org.springframework.jdbc.core.namedparam.NamedParameterJdbcTemplate;
import org.springframework.stereotype.Repository;

import dev.souq.catalog.catalog.Domain.Category;

/**
 * The category tree.
 *
 * <p>Stored as an adjacency list ({@code parent_id}) <em>plus</em> a
 * materialised {@code path} array. That is deliberate redundancy, and it is
 * worth it: the adjacency list is what makes a move cheap, while the path array
 * is what makes "everything under electronics" one indexed query instead of a
 * recursive CTE on the hot browsing path.
 *
 * <p>The cost of the redundancy is that a move must rewrite the paths of every
 * descendant, which {@link #move} does in a single statement. Doing it in
 * application code — read the subtree, rewrite each, write them back — leaves
 * the tree inconsistent if the process dies halfway, and a category tree with a
 * broken path array silently drops products out of listings.
 */
@Repository
public class JdbcCategoryRepository {

    private final NamedParameterJdbcTemplate jdbc;

    public JdbcCategoryRepository(NamedParameterJdbcTemplate jdbc) {
        this.jdbc = jdbc;
    }

    public static class CycleWouldBeCreated extends RuntimeException {
        public CycleWouldBeCreated(String id, String newParent) {
            super("moving %s under %s would create a cycle".formatted(id, newParent));
        }
    }

    private static final String SELECT = """
            SELECT c.id, c.slug, c.name, c.parent_id, c.path, c.position,
                   (SELECT count(*) FROM products p
                     WHERE p.category_id = c.id AND p.status = 'ACTIVE') AS product_count
              FROM categories c
            """;

    private static Category map(java.sql.ResultSet rs, int rowNum) throws java.sql.SQLException {
        return new Category(
                rs.getString("id"), rs.getString("slug"), rs.getString("name"),
                rs.getString("parent_id"),
                List.of((String[]) rs.getArray("path").getArray()),
                rs.getInt("position"), rs.getLong("product_count"));
    }

    /**
     * The whole tree, in render order.
     *
     * <p>All of it in one query. The tree is small — hundreds of rows, not
     * millions — and the storefront needs the entire thing for its navigation,
     * so paginating it or fetching it level by level would be more round trips
     * for no benefit. {@code cardinality(path)} orders parents before children,
     * so a caller can build the tree in one pass without sorting.
     */
    public List<Category> findAll() {
        return jdbc.query(SELECT + " ORDER BY cardinality(c.path), c.position, c.name",
                new MapSqlParameterSource(), JdbcCategoryRepository::map);
    }

    public Optional<Category> findBySlug(String slug) {
        return jdbc.query(SELECT + " WHERE c.slug = :slug",
                        new MapSqlParameterSource("slug", slug), JdbcCategoryRepository::map)
                .stream().findFirst();
    }

    public Optional<Category> findById(String id) {
        return jdbc.query(SELECT + " WHERE c.id = :id",
                        new MapSqlParameterSource("id", id), JdbcCategoryRepository::map)
                .stream().findFirst();
    }

    /**
     * A category and everything beneath it.
     *
     * <p>{@code path @> ARRAY[slug]} uses the GIN index, so this stays fast
     * however deep the tree gets. The recursive-CTE alternative walks the
     * adjacency list level by level and cannot use an index at all.
     */
    public List<Category> findSubtree(String rootSlug) {
        return jdbc.query(SELECT + " WHERE c.path @> ARRAY[:slug] ORDER BY cardinality(c.path), c.position",
                new MapSqlParameterSource("slug", rootSlug), JdbcCategoryRepository::map);
    }

    /**
     * Inserts a category, deriving its path from its parent.
     *
     * <p>The path is computed in SQL from the parent's row rather than passed
     * in. Letting a caller supply the path means a caller can supply a wrong
     * one, and a wrong path is invisible until a listing quietly returns the
     * wrong products.
     */
    public void insert(String id, String slug, String name, String parentId, int position) {
        jdbc.update("""
                INSERT INTO categories (id, slug, name, parent_id, path, position)
                SELECT :id, :slug, :name, :parentId,
                       CASE WHEN :parentId::text IS NULL THEN ARRAY[:slug]::text[]
                            ELSE (SELECT path FROM categories WHERE id = :parentId)
                                 || ARRAY[:slug]::text[]
                       END,
                       :position
                """,
                new MapSqlParameterSource()
                        .addValue("id", id)
                        .addValue("slug", slug)
                        .addValue("name", name)
                        .addValue("parentId", parentId)
                        .addValue("position", position));
    }

    public int rename(String id, String name, Integer position) {
        return jdbc.update("""
                UPDATE categories
                   SET name = coalesce(:name, name),
                       position = coalesce(:position, position)
                 WHERE id = :id
                """,
                new MapSqlParameterSource()
                        .addValue("id", id).addValue("name", name).addValue("position", position));
    }

    /**
     * Re-parents a category and repairs every descendant's path.
     *
     * <p>The cycle check comes first and is the reason this is not just an
     * {@code UPDATE}. Moving a node under its own descendant produces a tree
     * with no root, and every subsequent path query either returns nothing or
     * recurses until the connection dies.
     */
    public void move(String id, String newParentId) {
        var current = findById(id).orElseThrow(
                () -> new IllegalArgumentException("no category " + id));

        if (newParentId != null) {
            var target = findById(newParentId).orElseThrow(
                    () -> new IllegalArgumentException("no category " + newParentId));

            // The target's path contains our slug exactly when the target is
            // below us.
            if (target.path().contains(current.slug())) {
                throw new CycleWouldBeCreated(id, newParentId);
            }
        }

        int oldDepth = current.path().size();

        // One statement: replace the prefix of every path in the subtree. The
        // subtree is identified by the old path, which is why the new path is
        // computed from a subquery evaluated before any row is written.
        jdbc.update("""
                WITH new_prefix AS (
                    SELECT CASE WHEN :parentId::text IS NULL THEN ARRAY[:slug]::text[]
                                ELSE (SELECT path FROM categories WHERE id = :parentId)
                                     || ARRAY[:slug]::text[]
                           END AS prefix
                )
                UPDATE categories c
                   SET path = (SELECT prefix FROM new_prefix) || c.path[:oldDepth + 1 : ],
                       parent_id = CASE WHEN c.id = :id THEN :parentId ELSE c.parent_id END
                 WHERE c.path @> ARRAY[:slug]
                """,
                new MapSqlParameterSource()
                        .addValue("id", id)
                        .addValue("slug", current.slug())
                        .addValue("parentId", newParentId)
                        .addValue("oldDepth", oldDepth));
    }

    /**
     * Deletes a leaf category with no products.
     *
     * <p>Both guards are in the {@code WHERE} clause rather than checked first,
     * so a product or child added concurrently cannot slip through the window
     * between the check and the delete.
     *
     * @return true if it was deleted
     */
    public boolean deleteIfEmpty(String id) {
        return jdbc.update("""
                DELETE FROM categories c
                 WHERE c.id = :id
                   AND NOT EXISTS (SELECT 1 FROM categories x WHERE x.parent_id = c.id)
                   AND NOT EXISTS (SELECT 1 FROM products p WHERE p.category_id = c.id)
                """,
                new MapSqlParameterSource("id", id)) == 1;
    }

    public boolean slugExists(String slug) {
        Integer n = jdbc.queryForObject("SELECT count(*) FROM categories WHERE slug = :slug",
                new MapSqlParameterSource("slug", slug), Integer.class);
        return n != null && n > 0;
    }
}
