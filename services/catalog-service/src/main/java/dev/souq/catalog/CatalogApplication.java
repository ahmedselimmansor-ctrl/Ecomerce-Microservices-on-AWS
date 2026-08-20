package dev.souq.catalog;

import org.springframework.boot.SpringApplication;
import org.springframework.boot.autoconfigure.SpringBootApplication;
import org.springframework.scheduling.annotation.EnableScheduling;

/**
 * catalog-service.
 *
 * <p>Owns products, variants, categories and price history, and is the only
 * writer to the {@code catalog} database (docs/CONTRACTS.md §6). Everything
 * else — search, recommendations, the storefront's read models — is built from
 * the {@code souq.catalog.events.v1} topic this service emits.
 *
 * <p>That topic is <b>compacted</b>, which is why every product event carries
 * the full current state rather than a delta. Compaction keeps only the newest
 * message per key, so a consumer rebuilding from the topic must be able to
 * reconstruct the entity from one message. A delta-based event would leave a
 * rebuilt index holding whatever the last change happened to touch.
 */
@SpringBootApplication
@EnableScheduling
public class CatalogApplication {

    public static void main(String[] args) {
        SpringApplication.run(CatalogApplication.class, args);
    }
}
