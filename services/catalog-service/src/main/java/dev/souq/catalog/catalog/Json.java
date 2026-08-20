package dev.souq.catalog.catalog;

import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

import com.fasterxml.jackson.core.type.TypeReference;
import com.fasterxml.jackson.databind.ObjectMapper;

import dev.souq.catalog.catalog.Domain.Image;

/**
 * The three JSONB columns, read and written in one place.
 *
 * <p>{@code attributes} and {@code images} are JSONB rather than side tables,
 * which is a deliberate trade. Attributes are per-category and open-ended —
 * a shirt has a collar type, a laptop has a screen size — so a normalised
 * model means either a table per category or an entity-attribute-value schema,
 * and neither is queried the way this data is actually used.
 *
 * <p>The cost is that the database cannot enforce their shape, so this class is
 * the only boundary that can. Every read goes through a converter that produces
 * a sane empty value rather than propagating null, because a product whose
 * images column somehow holds {@code null} should render without images, not
 * throw halfway through a page.
 */
public final class Json {

    private static final ObjectMapper MAPPER = new ObjectMapper();

    private static final TypeReference<Map<String, String>> STRING_MAP = new TypeReference<>() {};
    private static final TypeReference<List<Image>> IMAGE_LIST = new TypeReference<>() {};

    private Json() {}

    public static Map<String, String> readAttributes(String json) {
        if (json == null || json.isBlank()) {
            return Map.of();
        }
        try {
            Map<String, String> parsed = MAPPER.readValue(json, STRING_MAP);
            return parsed == null ? Map.of() : parsed;
        } catch (Exception e) {
            // Logged by the caller's error handler if it matters. Returning an
            // empty map keeps one malformed row from failing a whole page of
            // otherwise-fine products.
            return Map.of();
        }
    }

    public static List<Image> readImages(String json) {
        if (json == null || json.isBlank()) {
            return List.of();
        }
        try {
            List<Image> parsed = MAPPER.readValue(json, IMAGE_LIST);
            return parsed == null ? List.of() : parsed;
        } catch (Exception e) {
            return List.of();
        }
    }

    public static String write(Object value) {
        try {
            return MAPPER.writeValueAsString(value == null ? Map.of() : value);
        } catch (Exception e) {
            throw new IllegalArgumentException("value is not serialisable to JSON", e);
        }
    }

    /**
     * Rejects attribute maps that would break the search index or a URL.
     *
     * <p>Search facets are built from these keys, so a key with a dot or a
     * bracket in it produces an Elasticsearch field name that cannot be
     * addressed in a query — and the failure surfaces in the indexer, hours
     * later, far from the admin who typed it.
     */
    public static List<String> validateAttributes(Map<String, String> attributes) {
        List<String> problems = new ArrayList<>();
        if (attributes == null) {
            return problems;
        }
        if (attributes.size() > 50) {
            problems.add("at most 50 attributes are allowed, got " + attributes.size());
        }

        attributes.forEach((key, value) -> {
            if (key == null || key.isBlank()) {
                problems.add("an attribute key is blank");
            } else if (!key.matches("^[a-z][a-z0-9_]{0,39}$")) {
                problems.add("attribute key '%s' must be lowercase letters, digits and underscores"
                        .formatted(key));
            }
            if (value != null && value.length() > 500) {
                problems.add("attribute '%s' exceeds 500 characters".formatted(key));
            }
        });

        return problems;
    }

    /**
     * Rejects image URLs that are not ours.
     *
     * <p>An admin-supplied URL is rendered in every storefront page for that
     * product. Accepting an arbitrary origin means an admin account — or a
     * compromised one — can point every product image at a third-party server
     * that sees the IP and referrer of every shopper, and can swap the content
     * afterwards.
     */
    public static List<String> validateImages(List<Image> images, List<String> allowedHosts) {
        List<String> problems = new ArrayList<>();
        if (images == null) {
            return problems;
        }
        if (images.size() > 20) {
            problems.add("at most 20 images are allowed, got " + images.size());
        }

        for (Image image : images) {
            if (image.url() == null || image.url().isBlank()) {
                problems.add("an image has no url");
                continue;
            }
            java.net.URI uri;
            try {
                uri = java.net.URI.create(image.url());
            } catch (IllegalArgumentException e) {
                problems.add("image url '%s' is not a valid URI".formatted(image.url()));
                continue;
            }
            if (!"https".equals(uri.getScheme())) {
                problems.add("image url '%s' must be https".formatted(image.url()));
            }
            if (uri.getHost() == null || !allowedHosts.contains(uri.getHost())) {
                problems.add("image host '%s' is not on the allow-list".formatted(uri.getHost()));
            }
        }

        return problems;
    }

    /** Preserves insertion order so an admin's attribute ordering survives a round trip. */
    public static Map<String, String> ordered(Map<String, String> source) {
        return source == null ? new LinkedHashMap<>() : new LinkedHashMap<>(source);
    }
}
