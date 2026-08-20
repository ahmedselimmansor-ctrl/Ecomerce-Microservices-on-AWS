package dev.souq.catalog.event;

import java.util.List;
import java.util.concurrent.TimeUnit;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.jdbc.core.namedparam.MapSqlParameterSource;
import org.springframework.jdbc.core.namedparam.NamedParameterJdbcTemplate;
import org.springframework.kafka.core.KafkaTemplate;
import org.springframework.scheduling.annotation.Scheduled;
import org.springframework.stereotype.Component;
import org.springframework.transaction.annotation.Transactional;

import io.micrometer.core.instrument.Gauge;
import io.micrometer.core.instrument.MeterRegistry;

/**
 * Publishes outbox rows to Kafka.
 *
 * <p>The same three-part pattern as every other service — write the event in
 * the business transaction, publish at-least-once, dedupe on the consumer —
 * with one addition this service needs and the others do not.
 *
 * <p><b>A null payload is published as a real Kafka tombstone.</b> The
 * {@code souq.catalog.events.v1} topic is compacted, and a tombstone is the
 * only thing that makes a key eventually disappear from it. The row stores the
 * JSON literal {@code null}; publishing that as the four-character string
 * {@code "null"} would be an ordinary message, the key would survive
 * compaction forever, and every rebuilt search index would replay every product
 * ever deleted.
 *
 * <p>{@code FOR UPDATE SKIP LOCKED} is what lets several replicas run this loop
 * at once. Plain {@code FOR UPDATE} makes them queue behind one another, so
 * three pods have the throughput of one.
 */
@Component
public class OutboxRelay {

    private static final Logger log = LoggerFactory.getLogger(OutboxRelay.class);

    private static final int BATCH_SIZE = 100;

    /**
     * Past this the row is parked instead of retried forever.
     *
     * <p>A payload Kafka rejects — malformed, or over {@code max.message.bytes},
     * which a product with a long description and twenty images can approach —
     * fails identically every time. Retrying it at the head of the batch blocks
     * every well-formed event behind it and stalls the search index.
     */
    private static final int MAX_ATTEMPTS = 10;

    private final NamedParameterJdbcTemplate jdbc;
    private final KafkaTemplate<String, String> kafka;

    public OutboxRelay(NamedParameterJdbcTemplate jdbc,
                       KafkaTemplate<String, String> kafka,
                       MeterRegistry meters) {
        this.jdbc = jdbc;
        this.kafka = kafka;

        // Backlog depth is the signal that matters. Events piling up is
        // invisible in latency and error rate — every API request succeeded —
        // and shows up as "search results are a day old" hours later.
        Gauge.builder("souq_outbox_backlog", this, OutboxRelay::backlogDepth)
                .description("Unpublished rows in the catalog-service outbox")
                .register(meters);

        Gauge.builder("souq_outbox_parked", this, OutboxRelay::parkedCount)
                .description("Outbox rows that exhausted their retries and need a human")
                .register(meters);
    }

    @Scheduled(fixedDelayString = "${souq.outbox.poll-interval-ms:500}")
    public void poll() {
        try {
            int published = publishBatch();
            // Keep draining while batches come back full. A bulk import writes
            // thousands of rows at once and should clear in seconds, not at
            // 100 events every 500 ms.
            int guard = 0;
            while (published == BATCH_SIZE && guard++ < 50) {
                published = publishBatch();
            }
        } catch (RuntimeException e) {
            log.error("outbox poll failed; will retry on the next tick", e);
        }
    }

    private record Row(long id, String eventId, String topic, String key,
                       String payload, int attempts) {}

    @Transactional
    protected int publishBatch() {
        List<Row> batch = jdbc.query("""
                SELECT id, event_id, topic, partition_key, payload::text AS payload, attempts
                  FROM outbox
                 WHERE published_at IS NULL AND attempts < :maxAttempts
                 ORDER BY id
                 LIMIT :limit
                   FOR UPDATE SKIP LOCKED
                """,
                new MapSqlParameterSource()
                        .addValue("maxAttempts", MAX_ATTEMPTS)
                        .addValue("limit", BATCH_SIZE),
                (rs, i) -> new Row(rs.getLong("id"), rs.getString("event_id"),
                        rs.getString("topic"), rs.getString("partition_key"),
                        rs.getString("payload"), rs.getInt("attempts")));

        if (batch.isEmpty()) {
            return 0;
        }

        int published = 0;

        for (Row row : batch) {
            try {
                // A JSONB `null` reads back as the text "null". That is the
                // compaction tombstone, and it has to reach Kafka as an actual
                // null value — not as a four-character message.
                String value = "null".equals(row.payload()) ? null : row.payload();

                // Synchronous. Firing the batch asynchronously and marking at
                // the end would mark rows whose send later failed, which is the
                // one thing the outbox exists to prevent.
                kafka.send(row.topic(), row.key(), value).get(10, TimeUnit.SECONDS);

                jdbc.update("UPDATE outbox SET published_at = now() WHERE id = :id",
                        new MapSqlParameterSource("id", row.id()));
                published++;

            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
                log.info("outbox relay interrupted after publishing {} of {}",
                        published, batch.size());
                break;

            } catch (Exception e) {
                recordFailure(row, e);
                // Keep going: one poisoned row must not stop the batch behind it.
            }
        }

        return published;
    }

    private void recordFailure(Row row, Exception cause) {
        int nextAttempt = row.attempts() + 1;

        jdbc.update("""
                UPDATE outbox SET attempts = attempts + 1, last_error = left(:error, 500)
                 WHERE id = :id
                """,
                new MapSqlParameterSource()
                        .addValue("id", row.id())
                        .addValue("error", cause.getMessage() == null
                                ? cause.getClass().getName() : cause.getMessage()));

        if (nextAttempt >= MAX_ATTEMPTS) {
            log.warn("outbox row {} ({}) parked after {} attempts: {}",
                    row.id(), row.eventId(), nextAttempt, cause.toString());
        } else {
            log.info("outbox row {} failed attempt {}: {}", row.id(), nextAttempt, cause.toString());
        }
    }

    /**
     * Deletes rows long since published.
     *
     * <p>Thirty days rather than identity-service's seven. The catalogue topic
     * is compacted rather than time-retained, so these rows are the only record
     * of when a specific change was published — useful for a fortnight when
     * someone asks why the search index disagreed with the admin screen.
     */
    @Scheduled(cron = "${souq.outbox.purge-cron:0 23 3 * * *}")
    @Transactional
    public void purgePublished() {
        int deleted = jdbc.update("""
                DELETE FROM outbox
                 WHERE published_at IS NOT NULL AND published_at < now() - INTERVAL '30 days'
                """,
                new MapSqlParameterSource());

        if (deleted > 0) {
            log.info("purged {} published outbox rows", deleted);
        }
    }

    private double backlogDepth() {
        Integer n = jdbc.queryForObject(
                "SELECT count(*) FROM outbox WHERE published_at IS NULL AND attempts < :max",
                new MapSqlParameterSource("max", MAX_ATTEMPTS), Integer.class);
        return n == null ? 0 : n;
    }

    private double parkedCount() {
        Integer n = jdbc.queryForObject(
                "SELECT count(*) FROM outbox WHERE published_at IS NULL AND attempts >= :max",
                new MapSqlParameterSource("max", MAX_ATTEMPTS), Integer.class);
        return n == null ? 0 : n;
    }
}
