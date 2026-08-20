package dev.souq.identity.auth;

import java.nio.ByteBuffer;
import java.security.MessageDigest;
import java.time.Instant;

import javax.crypto.Mac;
import javax.crypto.spec.SecretKeySpec;

/**
 * RFC 6238 time-based one-time passwords.
 *
 * <p>Two details here are the difference between working MFA and MFA that
 * frustrates every user:
 *
 * <ul>
 *   <li><b>A one-step window either side.</b> Phone clocks drift and people
 *       type slowly. Accepting only the current 30-second step rejects a
 *       meaningful fraction of correct codes. Accepting ±1 step covers a
 *       90-second span, which is the standard compromise.</li>
 *   <li><b>Constant-time comparison.</b> A byte-by-byte compare on a 6-digit
 *       code leaks how many leading digits were right, which reduces the
 *       search from a million to sixty guesses.</li>
 * </ul>
 */
public final class Totp {

    private static final int DIGITS = 6;
    private static final int STEP_SECONDS = 30;
    private static final int WINDOW_STEPS = 1;

    private Totp() {}

    public static boolean verify(String base32Secret, String code) {
        if (base32Secret == null || code == null || code.length() != DIGITS) {
            return false;
        }

        byte[] key = decodeBase32(base32Secret);
        long step = Instant.now().getEpochSecond() / STEP_SECONDS;

        for (int offset = -WINDOW_STEPS; offset <= WINDOW_STEPS; offset++) {
            String expected = generate(key, step + offset);
            // Constant time. MessageDigest.isEqual does not short-circuit.
            if (MessageDigest.isEqual(expected.getBytes(), code.getBytes())) {
                return true;
            }
        }
        return false;
    }

    private static String generate(byte[] key, long step) {
        try {
            Mac mac = Mac.getInstance("HmacSHA1");
            mac.init(new SecretKeySpec(key, "HmacSHA1"));
            byte[] hash = mac.doFinal(ByteBuffer.allocate(8).putLong(step).array());

            int offset = hash[hash.length - 1] & 0x0F;
            int binary = ((hash[offset] & 0x7F) << 24)
                    | ((hash[offset + 1] & 0xFF) << 16)
                    | ((hash[offset + 2] & 0xFF) << 8)
                    | (hash[offset + 3] & 0xFF);

            return String.format("%0" + DIGITS + "d", binary % 1_000_000);
        } catch (Exception e) {
            throw new IllegalStateException("HmacSHA1 unavailable", e);
        }
    }

    private static byte[] decodeBase32(String s) {
        String clean = s.replace("=", "").replace(" ", "").toUpperCase();
        final String alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";

        int buffer = 0;
        int bitsLeft = 0;
        byte[] out = new byte[clean.length() * 5 / 8];
        int index = 0;

        for (char c : clean.toCharArray()) {
            int value = alphabet.indexOf(c);
            if (value < 0) continue;
            buffer = (buffer << 5) | value;
            bitsLeft += 5;
            if (bitsLeft >= 8) {
                out[index++] = (byte) (buffer >> (bitsLeft - 8));
                bitsLeft -= 8;
            }
        }
        return out;
    }
}
