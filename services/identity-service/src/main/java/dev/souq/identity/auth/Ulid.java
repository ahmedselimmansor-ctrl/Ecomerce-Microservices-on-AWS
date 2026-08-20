package dev.souq.identity.auth;

import java.security.SecureRandom;
import java.time.Instant;

/**
 * Prefixed ULIDs, matching every other service in the platform.
 *
 * <p>Sortable by creation time, 26 characters, Crockford base32. The alphabet
 * excludes I, L, O and U precisely so a human reading an id aloud to support
 * cannot introduce an ambiguity.
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
}
