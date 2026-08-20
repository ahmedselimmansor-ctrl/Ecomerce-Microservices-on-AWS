package dev.souq.catalog.event;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.jdbc.core.namedparam.MapSqlParameterSource;
import org.springframework.jdbc.core.namedparam.NamedParameterJdbcTemplate;
import org.springframework.kafka.annotation.KafkaListener;
import org.springframework.kafka.support.Acknowledgment;
import org.springframework.kafka.support.KafkaHeaders;
import org.springframework.messaging.handler.annotation.Header;
import org.springframework.messaging.handler.annotation.Payload;
import org.springframework.stereotype.Component;
import org.springframework.transaction.annotation.Transactional;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;

import dev.souq.catalog.catalog.ProductService;

/**
 * Keeps {@code variants.available} roughly in step with inventory-service.
 *
 * <p>Roughly is the correct standard here, and the contract says so:
 * {@code available} is a display hint, the authoritative check happens when the
 * order saga reserves. This consumer exists so a product page is one query
 * instead of a fan-out to inventory for every SKU on it.
 *
 * <p>The parts that are not obvious:
 *
 * <p><b>The inbox is checked and written in the same transaction as the
 * update.</b> The relay that produced these events is at-least-once by design,
 * so duplicates are a guarantee rather than an edge case. Marking the event
 * processed in a separate transaction would let a crash between the two replay
 * it — harmless for this idempotent update, but the pattern has to be right
 * because it is copied.
 *
 * <p><b>A malformed message is acknowledged, not retried.</b> It will be
 * malformed on every attempt, and a poisoned message at the head of a partition
 * blocks every well-formed one behind it. Availability for one SKU going stale
 * is a far smaller problem than availability for the whole catalogue freezing.
 *
 * <p><b>Failing to find the SKU is not an error.</b> Inventory can hold stock
 * for a SKU this service has archived, and the orders are not synchronised.
 */
@Component
public class InventoryConsumer {

    private static final Logger log = LoggerFactory.getLogger(InventoryConsumer.class);
    private static final ObjectMapper MAPPER = new ObjectMapper();

    /** docs/CONTRACTS.md §9: consumer groups are {@code <service>.<purpose>}. */
    private static final String CONSUMER = "catalog-service.availability";

    private final NamedParameterJdbcTemplate jdbc;
    private final ProductService catalogue;

    public InventoryConsumer(NamedParameterJdbcTemplate jdbc, ProductService catalogue) {
        this.jdbc = jdbc;
        this.catalogue = catalogue;
    }

    @KafkaListener(
            topics = "souq.inventory.events.v1",
            groupId = CONSUMER,
            containerFactory = "kafkaListenerContainerFactory")
    @Transactional
    public void onInventoryEvent(@Payload String message,
                                 @Header(name = KafkaHeaders.RECEIVED_KEY, required = false) String key,
                                 Acknowledgment ack) {
        JsonNode envelope;
        try {
            envelope = MAPPER.readTree(message);
        } catch (Exception e) {
            log.error("dropping an unparseable inventory event (key={}): {}", key, e.toString());
            ack.acknowledge();
            return;
        }

        String eventId = envelope.path("id").asText(null);
        String type = envelope.path("type").asText("");

        if (eventId == null || eventId.isBlank()) {
            // A CloudEvent with no id cannot be deduped, so accepting it would
            // mean accepting unbounded reprocessing on every rebalance.
            log.error("dropping an inventory event with no CloudEvents id (type={})", type);
            ack.acknowledge();
            return;
        }

        // Only the events that carry an availability figure.
        if (!type.endsWith("stock_level_changed.v1") && !type.endsWith("stock_adjusted.v1")) {
            ack.acknowledge();
            return;
        }

        // The inbox. ON CONFLICT DO NOTHING makes the insert both the check and
        // the claim, so two consumers racing the same event have exactly one
        // winner without a SELECT that the other could pass first.
        int claimed = jdbc.update("""
                INSERT INTO processed_events (event_id, consumer)
                VALUES (:eventId, :consumer)
                ON CONFLICT (event_id, consumer) DO NOTHING
                """,
                new MapSqlParameterSource()
                        .addValue("eventId", eventId)
                        .addValue("consumer", CONSUMER));

        if (claimed == 0) {
            log.debug("inventory event {} already processed", eventId);
            ack.acknowledge();
            return;
        }

        JsonNode data = envelope.path("data");
        String sku = data.path("sku").asText(null);

        if (sku == null || !data.hasNonNull("available")) {
            log.warn("inventory event {} carries no sku/available pair", eventId);
            ack.acknowledge();
            return;
        }

        int available = Math.max(0, data.path("available").asInt());
        catalogue.applyAvailability(sku, available);

        // Acknowledged after the transaction's work, but the commit happens
        // when this method returns. A crash in between replays the event, which
        // the inbox makes harmless — that is the whole point of it.
        ack.acknowledge();
    }

    /**
     * Trims the inbox.
     *
     * <p>Thirty days is far longer than any plausible redelivery window and far
     * shorter than forever. Without this the table grows without bound and the
     * unique index behind every message's dedup check stops fitting in cache,
     * which turns the cheapest operation in the consumer into a disk read.
     */
    @org.springframework.scheduling.annotation.Scheduled(
            cron = "${souq.inbox.purge-cron:0 41 3 * * *}")
    @Transactional
    public void purgeProcessed() {
        int deleted = jdbc.update("""
                DELETE FROM processed_events
                 WHERE consumer = :consumer AND processed_at < now() - INTERVAL '30 days'
                """,
                new MapSqlParameterSource("consumer", CONSUMER));

        if (deleted > 0) {
            log.info("purged {} processed-event rows", deleted);
        }
    }
}
