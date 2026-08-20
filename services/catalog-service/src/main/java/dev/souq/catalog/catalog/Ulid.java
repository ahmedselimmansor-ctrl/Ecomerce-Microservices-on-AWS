package dev.souq.catalog.catalog;

import java.security.SecureRandom;
import java.time.Instant;

/**
 * Prefixed ULIDs, matching every other service.
 *
 * <p>Time-sortable, so a primary-key index appends rather than scattering
 * inserts across the whole B-tree the way a UUIDv4 does. Crockford base32 with
 * I, L, O and U excluded, so an id read aloud to support is unambiguous.
 */
public final class Ulid {

    private static final char[] CROCKFORD = "0123456789ABCDEFGHJKMNPQRSTVWXYZ".toCharArray();
    private static final SecureRandom RANDOM = new SecureRandom();

    private Ulid() {}

    public static String next() {
        char[] out = new char[26];

        long timestamp = Instant.now().toEpochMilli();
        for (int i = 9; i >= 0; i--) {
            out[i] = CROCKFORD[(int) (timestamp & 0x1F)];
            timestamp >>>= 5;
        }

        byte[] entropy = new byte[16];
        RANDOM.nextBytes(entropy);
        for (int i = 0; i < 16; i++) {
            out[10 + i] = CROCKFORD[entropy[i] & 0x1F];
        }

        return new String(out);
    }

    public static String product() { return "prd_" + next(); }

    public static String sku()     { return "sku_" + next(); }

    public static String category() { return "cat_" + next(); }
}
