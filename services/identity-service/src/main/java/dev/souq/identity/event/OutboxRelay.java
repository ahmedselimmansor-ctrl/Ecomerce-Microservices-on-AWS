package dev.souq.identity.event;

import java.time.Duration;
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
 * Publishes rows from the transactional outbox to Kafka.
 *
 * <p>The pattern is the same in all five languages, and each of the three parts
 * is individually load-bearing — {@code outbox_model_test.go} in order-service
 * shows the failure that appears when any one is removed:
 *
 * <ol>
 *   <li>The business write and the event row commit in <b>one</b> transaction.
 *       Publishing to Kafka inside that transaction instead means a rollback
 *       cannot unpublish, and a crash after commit loses the event.</li>
 *   <li>This relay publishes, then marks. It can die in between, so a duplicate
 *       is a <b>guarantee</b> of the design rather than an edge case.</li>
 *   <li>Consumers dedupe on {@code event_id}, which makes (2) harmless.</li>
 * </ol>
 *
 * <p>Two details below are the ones that break under load rather than in a test.
 *
 * <p><b>{@code FOR UPDATE SKIP LOCKED}.</b> Every replica runs this loop. Plain
 * {@code FOR UPDATE} makes them queue behind each other and the throughput of
 * three pods equals one. {@code SKIP LOCKED} hands each pod a disjoint batch.
 *
 * <p><b>Ordering is per key, not global.</b> Rows are claimed in id order and
 * sent with {@code partition_key} as the Kafka key, so everything about one
 * user lands on one partition in order. Two pods can interleave across
 * different keys, which is fine — no consumer in this platform depends on a
 * global order, and providing one would mean a single relay and no headroom.
 */
@Component
public class OutboxRelay {

    private static final Logger log = LoggerFactory.getLogger(OutboxRelay.class);

    /** Bounded so one poll cannot hold a claim long enough for the next to overlap. */
    private static final int BATCH_SIZE = 100;

    /**
     * Past this, the row is parked rather than retried forever.
     *
     * <p>A payload Kafka rejects — malformed, or over {@code max.message.bytes} —
     * fails identically on every attempt. Retrying it at the head of the batch
     * blocks every well-formed event behind it, which turns one bad row into a
     * total stall of the notification pipeline.
     */
    private static final int MAX_ATTEMPTS = 10;

    private final NamedParameterJdbcTemplate jdbc;
    private final KafkaTemplate<String, String> kafka;

    public OutboxRelay(NamedParameterJdbcTemplate jdbc,
                       KafkaTemplate<String, String> kafka,
                       MeterRegistry meters) {
        this.jdbc = jdbc;
        this.kafka = kafka;

        // The single most useful signal this service emits. Backlog depth going
        // up means events are being written faster than they leave — which the
        // service's own latency and error rate will not show, because from the
        // API's point of view every request succeeded.
        Gauge.builder("souq_outbox_backlog", this, OutboxRelay::backlogDepth)
                .description("Unpublished rows in the identity-service outbox")
                .register(meters);

        Gauge.builder("souq_outbox_parked", this, OutboxRelay::parkedCount)
                .description("Outbox rows that exhausted their retries and need a human")
                .register(meters);
    }

    @Scheduled(fixedDelayString = "${souq.outbox.poll-interval-ms:500}")
    public void poll() {
        try {
            int published = publishBatch();
            // Keep draining while batches come back full: a burst should clear
            // in one poll cycle, not at 100 events every 500 ms.
            int guard = 0;
            while (published == BATCH_SIZE && guard++ < 20) {
                published = publishBatch();
            }
        } catch (RuntimeException e) {
            // Never propagate: an exception out of a @Scheduled method with a
            // fixed delay is logged and the schedule continues, but a loop that
            // throws every tick fills the log and hides everything else.
            log.error("outbox poll failed; will retry on the next tick", e);
        }
    }

    private record Row(long id, String eventId, String topic, String key, String payload, int attempts) {}

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
                // Synchronous. Firing all hundred asynchronously and marking
                // them at the end would mark rows whose send later failed —
                // the one thing the outbox exists to prevent. The 10-second
                // bound is long enough for a leader election and short enough
                // that a wedged broker does not hold the transaction open.
                kafka.send(row.topic(), row.key(), row.payload()).get(10, TimeUnit.SECONDS);

                jdbc.update("UPDATE outbox SET published_at = now() WHERE id = :id",
                        new MapSqlParameterSource("id", row.id()));
                published++;

            } catch (InterruptedException e) {
                // Restore the flag and stop; the pod is shutting down and the
                // rest of the batch stays unpublished for the next owner.
                Thread.currentThread().interrupt();
                log.info("outbox relay interrupted after publishing {} of {}", published, batch.size());
                break;

            } catch (Exception e) {
                recordFailure(row, e);
                // Keep going. One poisoned row must not stop the batch behind it.
            }
        }

        return published;
    }

    private void recordFailure(Row row, Exception cause) {
        int nextAttempt = row.attempts() + 1;

        jdbc.update("""
                UPDATE outbox
                   SET attempts = attempts + 1,
                       last_error = left(:error, 500)
                 WHERE id = :id
                """,
                new MapSqlParameterSource()
                        .addValue("id", row.id())
                        .addValue("error", cause.getMessage() == null
                                ? cause.getClass().getName() : cause.getMessage()));

        if (nextAttempt >= MAX_ATTEMPTS) {
            // WARN, and there is an alert on souq_outbox_parked. A parked row is
            // an event that will never be delivered unless someone looks.
            log.warn("outbox row {} ({}) parked after {} attempts: {}",
                    row.id(), row.eventId(), nextAttempt, cause.toString());
        } else {
            log.info("outbox row {} failed attempt {}: {}", row.id(), nextAttempt, cause.toString());
        }
    }

    /**
     * Deletes rows published long enough ago to be uninteresting.
     *
     * <p>Seven days, matching the topic retention in docs/CONTRACTS.md §4 —
     * once Kafka has dropped the event, keeping the row proves nothing. Without
     * this the table grows forever and the partial index the relay depends on
     * eventually stops fitting in cache.
     */
    @Scheduled(cron = "${souq.outbox.purge-cron:0 17 3 * * *}")
    @Transactional
    public void purgePublished() {
        int deleted = jdbc.update("""
                DELETE FROM outbox
                 WHERE published_at IS NOT NULL AND published_at < now() - INTERVAL '7 days'
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

    /** Exposed for the readiness probe: a relay that has stalled is not ready to take traffic. */
    public boolean isDraining(Duration threshold) {
        Integer oldest = jdbc.queryForObject("""
                SELECT coalesce(extract(epoch FROM now() - min(created_at))::int, 0)
                  FROM outbox WHERE published_at IS NULL AND attempts < :max
                """,
                new MapSqlParameterSource("max", MAX_ATTEMPTS), Integer.class);

        return oldest != null && oldest <= threshold.toSeconds();
    }
}
