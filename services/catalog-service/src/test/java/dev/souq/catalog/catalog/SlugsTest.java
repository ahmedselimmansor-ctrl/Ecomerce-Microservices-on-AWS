package dev.souq.catalog.catalog;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.util.Set;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

class SlugsTest {

    @Test
    @DisplayName("lowercases, and collapses runs of punctuation into one hyphen")
    void basicSlugging() {
        assertEquals("wireless-headphones", Slugs.from("Wireless Headphones").orElseThrow());
        assertEquals("usb-c-cable-2m", Slugs.from("USB-C Cable  (2m)").orElseThrow());
        assertEquals("sony-wh-1000xm5", Slugs.from("Sony WH-1000XM5").orElseThrow());
    }

    /**
     * The reason NFD decomposition happens before the ASCII filter.
     *
     * <p>Stripping non-ASCII from the composed form deletes "é" entirely and
     * produces "caf". Decomposing first splits it into "e" plus a combining
     * accent, and only the accent is dropped.
     */
    @Test
    @DisplayName("keeps the base letter when an accent is stripped")
    void accentsDecomposeRatherThanDisappear() {
        assertEquals("cafe-noir", Slugs.from("Café Noir").orElseThrow());
        assertEquals("uber-widget", Slugs.from("Über Widget").orElseThrow());
        assertEquals("nino-toys", Slugs.from("Niño Toys").orElseThrow());
        // Not "caf-noir" — a hyphen where the accent was would be the bug.
        assertFalse(Slugs.from("Café Noir").orElseThrow().contains("caf-"));
    }

    /**
     * Arabic yields no ASCII at all. Returning an empty slug would make every
     * Arabic-titled product collide on the same URL, so the caller is told and
     * falls back to the id.
     */
    @Test
    @DisplayName("reports failure rather than returning an empty slug")
    void nonLatinScriptsProduceNothing() {
        assertTrue(Slugs.from("سماعات لاسلكية").isEmpty());
        assertTrue(Slugs.from("!!!").isEmpty());
        assertTrue(Slugs.from("   ").isEmpty());
        assertTrue(Slugs.from("").isEmpty());
        assertTrue(Slugs.from(null).isEmpty());
    }

    @Test
    @DisplayName("never starts or ends with a hyphen")
    void noLeadingOrTrailingHyphen() {
        for (String input : new String[]{"  Leading", "Trailing  ", "--Both--", "(Parens)"}) {
            String slug = Slugs.from(input).orElseThrow();
            assertFalse(slug.startsWith("-"), input + " -> " + slug);
            assertFalse(slug.endsWith("-"), input + " -> " + slug);
        }
    }

    /** A product legitimately called "New" would otherwise take the admin create route. */
    @Test
    @DisplayName("refuses words that would collide with a route")
    void reservedWordsAreRejected() {
        assertTrue(Slugs.from("New").isEmpty());
        assertTrue(Slugs.from("admin").isEmpty());
        assertTrue(Slugs.from("Checkout").isEmpty());
        // Only the whole slug is reserved; a word inside one is fine.
        assertEquals("new-arrivals", Slugs.from("New Arrivals").orElseThrow());
    }

    @Test
    @DisplayName("is bounded, and still does not end with a hyphen when truncated")
    void isBounded() {
        String slug = Slugs.from("a ".repeat(200)).orElseThrow();
        assertTrue(slug.length() <= 80, "length was " + slug.length());
        assertFalse(slug.endsWith("-"));
    }

    @Test
    @DisplayName("appends a readable counter rather than a random suffix")
    void uniquenessUsesACounter() {
        Set<String> taken = Set.of("wireless-headphones", "wireless-headphones-2");

        assertEquals("wireless-headphones-3",
                Slugs.uniqueFrom("Wireless Headphones", "prd_01H", taken::contains));

        assertEquals("wireless-headphones",
                Slugs.uniqueFrom("Wireless Headphones", "prd_01H", s -> false));
    }

    @Test
    @DisplayName("falls back to the id when the title yields no slug")
    void fallsBackToTheId() {
        assertEquals("prd_01habc",
                Slugs.uniqueFrom("سماعات", "PRD_01HABC", s -> false));
    }
}
