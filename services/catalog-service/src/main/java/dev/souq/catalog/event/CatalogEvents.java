package dev.souq.catalog.event;

import java.time.Instant;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

import org.springframework.jdbc.core.namedparam.MapSqlParameterSource;
import org.springframework.jdbc.core.namedparam.NamedParameterJdbcTemplate;
import org.springframework.stereotype.Component;

import com.fasterxml.jackson.databind.ObjectMapper;

import dev.souq.catalog.catalog.Domain.Product;
import dev.souq.catalog.catalog.Domain.Variant;
import dev.souq.catalog.catalog.Ulid;

/**
 * Writes catalogue events to the outbox.
 *
 * <p>Every method here takes no connection and no transaction handle: they run
 * inside the caller's {@code @Transactional} boundary through the shared
 * {@code NamedParameterJdbcTemplate}, so the event row and the product row
 * commit together. Publishing straight to Kafka instead would mean a rolled-back
 * write whose event was already read by search-service.
 *
 * <p><b>{@code souq.catalog.events.v1} is compacted</b>, which shapes the
 * payloads more than anything else. Compaction keeps only the newest message
 * per key, so a consumer rebuilding its index from the topic sees exactly one
 * message per product. That message therefore has to be the <em>full current
 * state</em>. A delta — "title changed to X" — would rebuild an index holding
 * nothing but whichever field was edited last.
 *
 * <p>It also means the partition key must be the product id and nothing else.
 * Keying by category, or by a random id, would leave several messages per
 * product surviving compaction with no defined order between them.
 */
@Component
public class CatalogEvents {

    private static final ObjectMapper MAPPER = new ObjectMapper();

    private static final String TOPIC = "souq.catalog.events.v1";
    private static final String SOURCE = "souq/catalog-service";

    private final NamedParameterJdbcTemplate jdbc;

    public CatalogEvents(NamedParameterJdbcTemplate jdbc) {
        this.jdbc = jdbc;
    }

    /**
     * Emits the full current state of a product.
     *
     * <p>Called after every write that changes anything a consumer can see —
     * including a price change, which also emits its own narrower event. The
     * duplication is intentional: {@code price_changed} is what the repricing
     * and alerting consumers subscribe to, while {@code product_upserted} is
     * what makes the compacted topic a complete snapshot.
     */
    public void productUpserted(Product product) {
        Variant primary = product.defaultVariant();

        Map<String, Object> data = new LinkedHashMap<>();
        data.put("productId", product.id());
        // The contract requires a sku. A product with no variants cannot be
        // bought, so it is published under its own id rather than omitted —
        // the search index still needs to drop it when it is archived.
        data.put("sku", primary == null ? product.id() : primary.sku());
        data.put("title", product.title());
        data.put("description", product.description());
        data.put("brand", product.brand());
        data.put("categoryPath", product.categoryPath());
        data.put("attributes", product.attributes());
        data.put("images", product.images());
        data.put("price", primary == null ? null : money(primary.price()));
        data.put("listPrice", primary == null || primary.listPrice() == null
                ? null : money(primary.listPrice()));
        data.put("status", product.status().name());
        data.put("locale", "en-GB");
        data.put("updatedAt", product.updatedAt().toString());

        enqueue("souq.catalog.product_upserted.v1", product.id(), data);
    }

    public void priceChanged(String productId, String sku,
                             dev.souq.catalog.catalog.Money oldPrice,
                             dev.souq.catalog.catalog.Money newPrice) {
        enqueue("souq.catalog.price_changed.v1", productId, Map.of(
                "productId", productId,
                "sku", sku,
                "oldPrice", money(oldPrice),
                "newPrice", money(newPrice)));
    }

    /**
     * Emits a deletion.
     *
     * <p>Two messages, and the second one matters. The first carries
     * {@code {productId}} so a consumer that is following along knows to remove
     * the entity. The second has a <b>null payload</b>, which is what Kafka
     * compaction treats as a tombstone: it is the only thing that makes the key
     * itself eventually disappear from the topic. Without it, a rebuild months
     * later still replays every product ever deleted.
     */
    public void productDeleted(String productId) {
        enqueue("souq.catalog.product_deleted.v1", productId, Map.of("productId", productId));
        enqueueTombstone(productId);
    }

    // -----------------------------------------------------------------------

    private static Map<String, Object> money(dev.souq.catalog.catalog.Money m) {
        // {amount, currency} with amount in minor units, matching the Money
        // schema in the AsyncAPI document and the Zod contracts.
        return Map.of("amount", m.amount(), "currency", m.currency());
    }

    private void enqueue(String type, String productId, Map<String, Object> data) {
        String eventId = Ulid.next();

        Map<String, Object> envelope = new LinkedHashMap<>();
        envelope.put("specversion", "1.0");
        envelope.put("id", eventId);
        envelope.put("source", SOURCE);
        envelope.put("type", type);
        envelope.put("time", Instant.now().toString());
        envelope.put("datacontenttype", "application/json");
        envelope.put("subject", productId);
        envelope.put("data", data);

        insert(eventId, type, productId, serialise(envelope));
    }

    /** A literal SQL NULL payload — the compaction tombstone. */
    private void enqueueTombstone(String productId) {
        jdbc.update("""
                INSERT INTO outbox (aggregate_type, aggregate_id, event_id, event_type,
                                    topic, partition_key, payload, headers)
                VALUES ('product', :productId, :eventId, 'souq.catalog.tombstone.v1',
                        :topic, :productId, 'null'::jsonb,
                        '{"souq-tombstone":"true"}'::jsonb)
                ON CONFLICT (event_id) DO NOTHING
                """,
                new MapSqlParameterSource()
                        .addValue("productId", productId)
                        .addValue("eventId", Ulid.next())
                        .addValue("topic", TOPIC));
    }

    private void insert(String eventId, String type, String productId, String payload) {
        jdbc.update("""
                INSERT INTO outbox (aggregate_type, aggregate_id, event_id, event_type,
                                    topic, partition_key, payload)
                VALUES ('product', :productId, :eventId, :type, :topic, :productId, :payload::jsonb)
                ON CONFLICT (event_id) DO NOTHING
                """,
                new MapSqlParameterSource()
                        .addValue("productId", productId)
                        .addValue("eventId", eventId)
                        .addValue("type", type)
                        .addValue("topic", TOPIC)
                        .addValue("payload", payload));
    }

    private static String serialise(Object value) {
        try {
            return MAPPER.writeValueAsString(value);
        } catch (Exception e) {
            // Failing the transaction is correct. Writing the product without
            // its event leaves the search index permanently behind, and the
            // divergence is silent.
            throw new IllegalStateException("catalogue event is not serialisable", e);
        }
    }

    /** Exposed so a backfill can republish a known set without re-reading them one by one. */
    public void republish(List<Product> products) {
        products.forEach(this::productUpserted);
    }
}
