package dev.souq.catalog.catalog;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.util.List;
import java.util.Map;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import dev.souq.catalog.catalog.Domain.Image;

class JsonTest {

    private static final List<String> HOSTS = List.of("cdn.souq.dev", "souq-media.s3.amazonaws.com");

    // ------------------------------------------------------------ reading

    /**
     * JSONB has no schema, so a malformed value is possible however careful the
     * writers are. One bad row must not fail a whole page of good products.
     */
    @Test
    @DisplayName("degrades to empty rather than throwing on malformed JSON")
    void malformedJsonDegrades() {
        assertEquals(Map.of(), Json.readAttributes("{not json"));
        assertEquals(Map.of(), Json.readAttributes(null));
        assertEquals(Map.of(), Json.readAttributes(""));
        assertEquals(List.of(), Json.readImages("[[["));
        assertEquals(List.of(), Json.readImages(null));
    }

    @Test
    @DisplayName("round-trips attributes and images")
    void roundTrips() {
        var attributes = Map.of("colour", "black", "size", "large");
        assertEquals(attributes, Json.readAttributes(Json.write(attributes)));

        var images = List.of(new Image("https://cdn.souq.dev/a.jpg", "A", 800, 600));
        var read = Json.readImages(Json.write(images));
        assertEquals(1, read.size());
        assertEquals("https://cdn.souq.dev/a.jpg", read.get(0).url());
        assertEquals(800, read.get(0).width());
    }

    // --------------------------------------------------------- attributes

    /**
     * Search facets are built from these keys. A key with a dot or a bracket
     * produces an Elasticsearch field name that cannot be addressed in a query,
     * and the failure surfaces in the indexer hours later.
     */
    @Test
    @DisplayName("rejects attribute keys that would break a search facet")
    void rejectsUnsafeAttributeKeys() {
        assertFalse(Json.validateAttributes(Map.of("colour.primary", "black")).isEmpty());
        assertFalse(Json.validateAttributes(Map.of("colour[0]", "black")).isEmpty());
        assertFalse(Json.validateAttributes(Map.of("Colour", "black")).isEmpty());
        assertFalse(Json.validateAttributes(Map.of("1colour", "black")).isEmpty());
        assertFalse(Json.validateAttributes(Map.of("", "black")).isEmpty());

        assertTrue(Json.validateAttributes(Map.of("colour", "black")).isEmpty());
        assertTrue(Json.validateAttributes(Map.of("screen_size_in", "13")).isEmpty());
    }

    @Test
    @DisplayName("bounds the number and size of attributes")
    void boundsAttributes() {
        var many = new java.util.HashMap<String, String>();
        for (int i = 0; i < 51; i++) {
            many.put("attr_" + i, "v");
        }
        assertFalse(Json.validateAttributes(many).isEmpty());

        assertFalse(Json.validateAttributes(Map.of("colour", "x".repeat(501))).isEmpty());
        assertTrue(Json.validateAttributes(Map.of("colour", "x".repeat(500))).isEmpty());
    }

    // ------------------------------------------------------------- images

    /**
     * The host allow-list is the point of this method.
     *
     * <p>A product image is rendered on every storefront page for that product.
     * An arbitrary origin means a compromised admin account can see the IP and
     * referrer of every shopper — and swap the content afterwards.
     */
    @Test
    @DisplayName("rejects image hosts that are not ours")
    void rejectsForeignImageHosts() {
        var foreign = List.of(new Image("https://evil.example/track.gif", "", null, null));
        assertFalse(Json.validateImages(foreign, HOSTS).isEmpty());

        var ours = List.of(new Image("https://cdn.souq.dev/a.jpg", "", null, null));
        assertTrue(Json.validateImages(ours, HOSTS).isEmpty());
    }

    @Test
    @DisplayName("rejects plain http and unparseable urls")
    void rejectsInsecureAndMalformedUrls() {
        assertFalse(Json.validateImages(
                List.of(new Image("http://cdn.souq.dev/a.jpg", "", null, null)), HOSTS).isEmpty());
        assertFalse(Json.validateImages(
                List.of(new Image("not a url at all", "", null, null)), HOSTS).isEmpty());
        assertFalse(Json.validateImages(
                List.of(new Image("", "", null, null)), HOSTS).isEmpty());
        assertFalse(Json.validateImages(
                List.of(new Image("javascript:alert(1)", "", null, null)), HOSTS).isEmpty());
    }

    @Test
    @DisplayName("bounds the number of images")
    void boundsImages() {
        var many = new java.util.ArrayList<Image>();
        for (int i = 0; i < 21; i++) {
            many.add(new Image("https://cdn.souq.dev/" + i + ".jpg", "", null, null));
        }
        assertFalse(Json.validateImages(many, HOSTS).isEmpty());
    }

    @Test
    @DisplayName("treats null collections as empty rather than throwing")
    void nullsAreEmpty() {
        assertTrue(Json.validateAttributes(null).isEmpty());
        assertTrue(Json.validateImages(null, HOSTS).isEmpty());
        assertEquals(Map.of(), Json.ordered(null));
    }
}
