package dev.souq.identity.auth;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.util.HashSet;
import java.util.Set;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

class UlidTest {

    @Test
    @DisplayName("is 26 Crockford base32 characters")
    void shapeIsCorrect() {
        String id = Ulid.next();
        assertEquals(26, id.length());
        assertTrue(id.matches("[0-9A-HJKMNP-TV-Z]{26}"), id);
    }

    /**
     * The alphabet excludes I, L, O and U so an id read aloud to support cannot
     * be confused with 1, 1, 0 or a word nobody wants in an identifier.
     */
    @Test
    @DisplayName("never emits an ambiguous character")
    void excludesAmbiguousCharacters() {
        for (int i = 0; i < 2_000; i++) {
            String id = Ulid.next();
            for (char c : new char[]{'I', 'L', 'O', 'U'}) {
                assertTrue(id.indexOf(c) < 0, "'" + c + "' appeared in " + id);
            }
        }
    }

    @Test
    @DisplayName("is unique across a tight loop")
    void isUnique() {
        Set<String> seen = new HashSet<>();
        for (int i = 0; i < 50_000; i++) {
            assertTrue(seen.add(Ulid.next()), "duplicate id at iteration " + i);
        }
    }

    /**
     * Lexicographic order tracks creation order. This is why ids are ULIDs and
     * not UUIDv4: a primary-key index on random values scatters inserts across
     * the whole B-tree, and on a hot table that is the difference between
     * appending to one page and dirtying a page per insert.
     */
    @Test
    @DisplayName("sorts by creation time")
    void sortsByTime() throws InterruptedException {
        String earlier = Ulid.next();
        Thread.sleep(3);
        String later = Ulid.next();
        assertTrue(earlier.compareTo(later) < 0, earlier + " should sort before " + later);
    }
}
