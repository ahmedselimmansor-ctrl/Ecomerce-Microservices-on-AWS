package dev.souq.identity.auth;

import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.util.HexFormat;
import java.util.List;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.security.crypto.argon2.Argon2PasswordEncoder;
import org.springframework.web.client.RestClient;

/**
 * Password hashing and validation.
 *
 * <p><b>Argon2id, not bcrypt.</b> bcrypt silently truncates at 72 bytes — a
 * passphrase manager generating a 100-character password gets the first 72 and
 * nobody is told. It also has no memory cost, so a GPU cracks it orders of
 * magnitude faster. Argon2id is the current OWASP recommendation and the
 * parameters live inside the encoded hash, so raising the cost later re-hashes
 * on next login rather than needing a migration.
 *
 * <p><b>Length over composition rules.</b> NIST SP 800-63B has said since 2017
 * that composition rules make passwords worse, not better: they reliably
 * produce {@code Passw0rd!}. A 12-character minimum with no character-class
 * requirement, plus a breach check, is strictly stronger.
 */
public class PasswordService {

    private static final Logger log = LoggerFactory.getLogger(PasswordService.class);

    /**
     * OWASP's 2024 baseline: 19 MiB, 2 iterations, 1 degree of parallelism.
     *
     * <p>Memory cost is the parameter that matters. Iterations are cheap for an
     * attacker with a GPU; memory is not.
     */
    private static final int SALT_LENGTH = 16;
    private static final int HASH_LENGTH = 32;
    private static final int PARALLELISM = 1;
    private static final int MEMORY_KIB = 19 * 1024;
    private static final int ITERATIONS = 2;

    /** How many previous passwords are refused on a change. */
    private static final int PASSWORD_HISTORY = 5;

    private final Argon2PasswordEncoder encoder =
            new Argon2PasswordEncoder(SALT_LENGTH, HASH_LENGTH, PARALLELISM, MEMORY_KIB, ITERATIONS);

    private final RestClient breachClient = RestClient.builder()
            .baseUrl("https://api.pwnedpasswords.com")
            .build();

    private final boolean breachCheckEnabled;

    public PasswordService(boolean breachCheckEnabled) {
        this.breachCheckEnabled = breachCheckEnabled;
    }

    public String hash(String raw) {
        return encoder.encode(raw);
    }

    /**
     * Verifies a password.
     *
     * <p>Callers MUST call this even when the account does not exist, against a
     * dummy hash. Returning early on an unknown email makes the response
     * measurably faster and turns login into an account-enumeration oracle —
     * see {@link #verifyAgainstDummy()}.
     */
    public boolean verify(String raw, String encoded) {
        return encoder.matches(raw, encoded);
    }

    /**
     * Burns the same CPU as a real verification, for a login attempt against an
     * email that does not exist.
     *
     * <p>Without this, "unknown email" returns in microseconds and "wrong
     * password" takes ~50ms, and anyone can enumerate which addresses have
     * accounts by timing the difference.
     */
    public void verifyAgainstDummy() {
        encoder.matches("dummy-password-for-timing-parity",
                "$argon2id$v=19$m=19456,t=2,p=1$c29tZXNhbHR2YWx1ZQ$"
                        + "aGFzaHZhbHVlaGFzaHZhbHVlaGFzaHZhbHVlaGFzaA");
    }

    /** True when the stored hash used weaker parameters than we now require. */
    public boolean needsRehash(String encoded) {
        return encoder.upgradeEncoding(encoded);
    }

    public record ValidationResult(boolean ok, String reasonCode, String message) {
        public static ValidationResult valid() { return new ValidationResult(true, null, null); }
        public static ValidationResult invalid(String code, String msg) {
            return new ValidationResult(false, code, msg);
        }
    }

    /**
     * Validates a candidate password.
     *
     * @param previousHashes the user's recent hashes; empty on registration
     */
    public ValidationResult validate(String raw, String email, List<String> previousHashes) {
        if (raw == null || raw.length() < 12) {
            return ValidationResult.invalid("PASSWORD_TOO_SHORT",
                    "Use at least 12 characters. A memorable phrase is stronger than a short complex password.");
        }
        if (raw.length() > 256) {
            // Not a security limit — an upper bound so a megabyte of input
            // cannot be used to burn CPU in the KDF.
            return ValidationResult.invalid("PASSWORD_TOO_LONG", "Use at most 256 characters.");
        }
        if (email != null && raw.toLowerCase().contains(email.split("@")[0].toLowerCase())) {
            return ValidationResult.invalid("PASSWORD_CONTAINS_EMAIL",
                    "Your password must not contain your email address.");
        }

        for (String previous : previousHashes.stream().limit(PASSWORD_HISTORY).toList()) {
            if (encoder.matches(raw, previous)) {
                return ValidationResult.invalid("PASSWORD_REUSED",
                        "You have used this password recently. Choose a different one.");
            }
        }

        if (breachCheckEnabled && isBreached(raw)) {
            return ValidationResult.invalid("PASSWORD_BREACHED",
                    "This password has appeared in a known data breach. Choose a different one.");
        }

        return ValidationResult.valid();
    }

    /**
     * Checks a password against Have I Been Pwned using k-anonymity.
     *
     * <p>Only the first five characters of the SHA-1 hash leave this process.
     * The service returns every suffix sharing that prefix — a few hundred —
     * and the comparison happens locally. The full password, and the full
     * hash, are never transmitted.
     *
     * <p>A failure here is not a validation failure. If the service is
     * unreachable we allow the password: blocking every registration because a
     * third-party API is down would be a self-inflicted outage.
     */
    private boolean isBreached(String raw) {
        try {
            MessageDigest sha1 = MessageDigest.getInstance("SHA-1");
            String hash = HexFormat.of().formatHex(
                    sha1.digest(raw.getBytes(StandardCharsets.UTF_8))).toUpperCase();

            String prefix = hash.substring(0, 5);
            String suffix = hash.substring(5);

            String body = breachClient.get()
                    .uri("/range/{prefix}", prefix)
                    .header("Add-Padding", "true")  // hides the response size from an observer
                    .retrieve()
                    .body(String.class);

            if (body == null) return false;

            for (String line : body.split("\n")) {
                int colon = line.indexOf(':');
                if (colon > 0 && line.substring(0, colon).trim().equals(suffix)) {
                    return true;
                }
            }
            return false;
        } catch (Exception e) {
            log.warn("breach check unavailable; allowing the password: {}", e.getMessage());
            return false;
        }
    }
}
