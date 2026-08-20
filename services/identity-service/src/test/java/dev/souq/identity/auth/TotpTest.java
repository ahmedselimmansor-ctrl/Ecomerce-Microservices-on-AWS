package dev.souq.identity.auth;

import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/**
 * RFC 6238 vectors plus the two properties that are easy to get wrong.
 *
 * <p>The published vectors are pinned to fixed timestamps, and {@link Totp}
 * reads the clock internally, so they are checked through the same private
 * generation path by reflection rather than by making the clock injectable —
 * a seam that exists only for a test is a seam production code can be wired
 * through by accident.
 */
class TotpTest {

    /** RFC 6238 Appendix B, SHA-1: secret "12345678901234567890" as base32. */
    private static final String RFC_SECRET = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ";

    private static String generateAt(long unixSeconds) throws Exception {
        var method = Totp.class.getDeclaredMethod("generate", byte[].class, long.class);
        method.setAccessible(true);

        var decode = Totp.class.getDeclaredMethod("decodeBase32", String.class);
        decode.setAccessible(true);
        byte[] key = (byte[]) decode.invoke(null, RFC_SECRET);

        return (String) method.invoke(null, key, unixSeconds / 30);
    }

    @Test
    @DisplayName("matches the RFC 6238 reference vectors")
    void matchesReferenceVectors() throws Exception {
        // Appendix B, truncated to the low 6 digits this implementation emits:
        //   T=59          -> 94287082
        //   T=1111111109  -> 07081804
        //   T=1111111111  -> 14050471
        //   T=1234567890  -> 89005924
        //   T=2000000000  -> 69279037
        org.junit.jupiter.api.Assertions.assertEquals("287082", generateAt(59L));
        org.junit.jupiter.api.Assertions.assertEquals("081804", generateAt(1111111109L));
        org.junit.jupiter.api.Assertions.assertEquals("050471", generateAt(1111111111L));
        org.junit.jupiter.api.Assertions.assertEquals("005924", generateAt(1234567890L));
        org.junit.jupiter.api.Assertions.assertEquals("279037", generateAt(2000000000L));
    }

    @Test
    @DisplayName("accepts the current code")
    void acceptsCurrentCode() throws Exception {
        String now = generateAt(System.currentTimeMillis() / 1000);
        assertTrue(Totp.verify(RFC_SECRET, now));
    }

    /**
     * The tolerance that keeps MFA usable. Phone clocks drift and people type
     * slowly; accepting only the current 30-second step rejects a meaningful
     * share of correct codes, and users respond by disabling MFA.
     */
    @Test
    @DisplayName("accepts a code from the immediately previous step")
    void acceptsPreviousStep() throws Exception {
        String previous = generateAt((System.currentTimeMillis() / 1000) - 30);
        assertTrue(Totp.verify(RFC_SECRET, previous));
    }

    @Test
    @DisplayName("rejects a code two steps old — the window is one step, not two")
    void rejectsTwoStepsOld() throws Exception {
        String stale = generateAt((System.currentTimeMillis() / 1000) - 90);
        assertFalse(Totp.verify(RFC_SECRET, stale));
    }

    @Test
    @DisplayName("rejects malformed input without throwing")
    void rejectsMalformedInput() {
        assertFalse(Totp.verify(RFC_SECRET, null));
        assertFalse(Totp.verify(RFC_SECRET, ""));
        assertFalse(Totp.verify(RFC_SECRET, "12345"));      // too short
        assertFalse(Totp.verify(RFC_SECRET, "1234567"));    // too long
        assertFalse(Totp.verify(RFC_SECRET, "abcdef"));     // not digits
        assertFalse(Totp.verify(null, "123456"));
    }
}
