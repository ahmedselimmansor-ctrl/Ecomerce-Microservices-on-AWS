package dev.souq.identity.mfa;

import java.security.MessageDigest;
import java.security.SecureRandom;
import java.time.Duration;
import java.time.Instant;
import java.util.ArrayList;
import java.util.List;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.jdbc.core.namedparam.MapSqlParameterSource;
import org.springframework.jdbc.core.namedparam.NamedParameterJdbcTemplate;
import org.springframework.transaction.annotation.Transactional;

import dev.souq.identity.auth.Totp;
import dev.souq.identity.token.TokenService;

/**
 * TOTP enrolment and recovery codes.
 *
 * <p>The shape of this is dictated by one failure mode: a user who enables MFA
 * and then cannot produce a code is locked out of their own account, and the
 * only way back is a human identity check — which is expensive and is the
 * weakest link in the entire scheme. Everything below is arranged to make that
 * less likely.
 *
 * <p><b>The secret is pending until a code proves it works.</b> It is written to
 * {@code mfa_pending_secret}, not to {@code mfa_secret}, and only moves across
 * on a successful verification. Enabling at issue time locks out everyone whose
 * authenticator app failed to save it.
 *
 * <p><b>Recovery codes are issued at the same moment, and only then.</b> They
 * are returned once and stored hashed, so there is no path — for the user or
 * for support — that retrieves them later.
 *
 * <p><b>Disabling requires a current code.</b> Otherwise a stolen access token
 * turns off the protection that exists to make a stolen access token survivable.
 */
public class MfaService {

    private static final Logger log = LoggerFactory.getLogger(MfaService.class);
    private static final SecureRandom RANDOM = new SecureRandom();

    /** RFC 4648 base32, which is what every authenticator app expects. */
    private static final String BASE32 = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";

    /** 160 bits, the size RFC 6238 specifies for HMAC-SHA1. */
    private static final int SECRET_BYTES = 20;

    private static final int RECOVERY_CODE_COUNT = 10;

    /**
     * How long an unconfirmed enrolment survives.
     *
     * <p>Long enough to install an app and fetch a phone from another room;
     * short enough that an abandoned secret is not sitting in the database
     * indefinitely.
     */
    private static final Duration PENDING_TTL = Duration.ofMinutes(30);

    private final NamedParameterJdbcTemplate jdbc;
    private final TokenService tokens;
    private final String issuerLabel;

    public MfaService(NamedParameterJdbcTemplate jdbc, TokenService tokens, String issuerLabel) {
        this.jdbc = jdbc;
        this.tokens = tokens;
        this.issuerLabel = issuerLabel;
    }

    public record Enrolment(String secret, String uri) {}

    public static class AlreadyEnrolled extends RuntimeException {
        public AlreadyEnrolled() { super("two-factor authentication is already on"); }
    }

    public static class NoPendingEnrolment extends RuntimeException {
        public NoPendingEnrolment() {
            super("start enrolment before confirming it, or start again — it may have expired");
        }
    }

    public static class BadCode extends RuntimeException {
        public BadCode() { super("that code was not accepted"); }
    }

    // -----------------------------------------------------------------------

    /**
     * Issues a secret and stores it as pending.
     *
     * <p>Restarting an enrolment overwrites the previous pending secret rather
     * than adding a second. Two live pending secrets would mean a code from
     * either enables MFA, and the user has no way to tell which one their app
     * actually holds.
     */
    @Transactional
    public Enrolment begin(String userId, String email) {
        Boolean enabled = jdbc.queryForObject(
                "SELECT mfa_enabled FROM users WHERE id = :id",
                new MapSqlParameterSource("id", userId), Boolean.class);

        if (Boolean.TRUE.equals(enabled)) {
            // Re-enrolling would silently invalidate the app the user is
            // currently relying on. Turning it off first is a deliberate step.
            throw new AlreadyEnrolled();
        }

        String secret = randomBase32Secret();

        jdbc.update("""
                UPDATE users
                   SET mfa_pending_secret = :secret,
                       mfa_pending_since  = now(),
                       updated_at = now()
                 WHERE id = :id
                """,
                new MapSqlParameterSource().addValue("id", userId).addValue("secret", secret));

        log.info("started MFA enrolment for {}", userId);
        return new Enrolment(secret, otpauthUri(secret, email));
    }

    /**
     * Confirms an enrolment and returns the recovery codes.
     *
     * <p>The recovery codes are generated here and returned exactly once. Only
     * their hashes are stored, so this response is the only readable copy that
     * will ever exist.
     */
    @Transactional
    public List<String> confirm(String userId, String code) {
        var pending = jdbc.query("""
                SELECT mfa_pending_secret, mfa_pending_since
                  FROM users
                 WHERE id = :id AND mfa_pending_secret IS NOT NULL
                """,
                new MapSqlParameterSource("id", userId),
                (rs, i) -> new Object[]{rs.getString(1), rs.getTimestamp(2).toInstant()});

        if (pending.isEmpty()) {
            throw new NoPendingEnrolment();
        }

        String secret = (String) pending.get(0)[0];
        Instant since = (Instant) pending.get(0)[1];

        if (since.plus(PENDING_TTL).isBefore(Instant.now())) {
            clearPending(userId);
            throw new NoPendingEnrolment();
        }

        if (!Totp.verify(secret, code)) {
            // The pending secret is left in place. A mistyped digit should not
            // mean re-scanning the QR code.
            throw new BadCode();
        }

        jdbc.update("""
                UPDATE users
                   SET mfa_secret = mfa_pending_secret,
                       mfa_enabled = TRUE,
                       mfa_pending_secret = NULL,
                       mfa_pending_since = NULL,
                       updated_at = now()
                 WHERE id = :id
                """,
                new MapSqlParameterSource("id", userId));

        List<String> codes = issueRecoveryCodes(userId);

        log.info("MFA enabled for {} with {} recovery codes", userId, codes.size());
        return codes;
    }

    /**
     * Turns MFA off.
     *
     * <p>Requires a currently valid code or an unused recovery code. Without
     * that requirement, a stolen access token disables the very thing that makes
     * a stolen access token survivable — and the 15-minute TTL means an attacker
     * only needs one window.
     */
    @Transactional
    public void disable(String userId, String code) {
        var row = jdbc.query("SELECT mfa_secret FROM users WHERE id = :id AND mfa_enabled",
                new MapSqlParameterSource("id", userId), (rs, i) -> rs.getString(1));

        if (row.isEmpty()) {
            // Already off. Idempotent rather than an error — the end state is
            // what the caller asked for.
            return;
        }

        if (!Totp.verify(row.get(0), code) && !consumeRecoveryCode(userId, code)) {
            throw new BadCode();
        }

        jdbc.update("""
                UPDATE users
                   SET mfa_enabled = FALSE, mfa_secret = NULL,
                       mfa_pending_secret = NULL, mfa_pending_since = NULL,
                       updated_at = now()
                 WHERE id = :id
                """,
                new MapSqlParameterSource("id", userId));

        jdbc.update("DELETE FROM mfa_recovery_codes WHERE user_id = :id",
                new MapSqlParameterSource("id", userId));

        // Every session, gone. Turning off a second factor is exactly the action
        // an attacker takes first, so the legitimate user re-authenticating is a
        // small price.
        tokens.revokeAllForUser(userId, "MFA_DISABLED");

        log.warn("MFA disabled for {}; all sessions revoked", userId);
    }

    /**
     * Spends a recovery code.
     *
     * <p>The {@code UPDATE ... WHERE used_at IS NULL} is what makes it single
     * use: two requests racing the same code produce exactly one winner, where
     * a select-then-update would let both through.
     */
    @Transactional
    public boolean consumeRecoveryCode(String userId, String code) {
        int spent = jdbc.update("""
                UPDATE mfa_recovery_codes
                   SET used_at = now()
                 WHERE user_id = :id AND code_hash = :hash AND used_at IS NULL
                """,
                new MapSqlParameterSource()
                        .addValue("id", userId)
                        .addValue("hash", sha256(normalise(code))));

        if (spent == 1) {
            Integer remaining = jdbc.queryForObject("""
                    SELECT count(*) FROM mfa_recovery_codes
                     WHERE user_id = :id AND used_at IS NULL
                    """,
                    new MapSqlParameterSource("id", userId), Integer.class);

            log.warn("recovery code used for {}; {} remaining", userId, remaining);
        }
        return spent == 1;
    }

    /** Clears an abandoned enrolment. Called by the sweeper and on expiry. */
    @Transactional
    public int clearExpiredPending() {
        return jdbc.update("""
                UPDATE users
                   SET mfa_pending_secret = NULL, mfa_pending_since = NULL
                 WHERE mfa_pending_secret IS NOT NULL
                   AND mfa_pending_since < now() - INTERVAL '30 minutes'
                """,
                new MapSqlParameterSource());
    }

    // -----------------------------------------------------------------------

    private void clearPending(String userId) {
        jdbc.update("""
                UPDATE users SET mfa_pending_secret = NULL, mfa_pending_since = NULL
                 WHERE id = :id
                """,
                new MapSqlParameterSource("id", userId));
    }

    private List<String> issueRecoveryCodes(String userId) {
        // Any codes from a previous enrolment are void. Leaving them would mean
        // a code printed months ago still bypasses the factor just installed.
        jdbc.update("DELETE FROM mfa_recovery_codes WHERE user_id = :id",
                new MapSqlParameterSource("id", userId));

        var codes = new ArrayList<String>(RECOVERY_CODE_COUNT);

        for (int i = 0; i < RECOVERY_CODE_COUNT; i++) {
            String code = randomRecoveryCode();
            codes.add(code);

            jdbc.update("""
                    INSERT INTO mfa_recovery_codes (user_id, code_hash) VALUES (:id, :hash)
                    """,
                    new MapSqlParameterSource()
                            .addValue("id", userId)
                            .addValue("hash", sha256(normalise(code))));
        }
        return codes;
    }

    private static String randomBase32Secret() {
        byte[] bytes = new byte[SECRET_BYTES];
        RANDOM.nextBytes(bytes);

        var out = new StringBuilder();
        int buffer = 0;
        int bits = 0;

        for (byte b : bytes) {
            buffer = (buffer << 8) | (b & 0xFF);
            bits += 8;
            while (bits >= 5) {
                out.append(BASE32.charAt((buffer >> (bits - 5)) & 0x1F));
                bits -= 5;
            }
        }
        if (bits > 0) {
            out.append(BASE32.charAt((buffer << (5 - bits)) & 0x1F));
        }
        return out.toString();
    }

    /**
     * A recovery code.
     *
     * <p>Grouped into blocks and drawn from an alphabet with no {@code 0/O} or
     * {@code 1/I}. These get written on paper and typed back months later, under
     * stress, by someone who has just lost their phone.
     */
    private static String randomRecoveryCode() {
        final String alphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ";
        var out = new StringBuilder(11);

        for (int i = 0; i < 10; i++) {
            if (i == 5) out.append('-');
            out.append(alphabet.charAt(RANDOM.nextInt(alphabet.length())));
        }
        return out.toString();
    }

    /** Case and separators are cosmetic; the stored hash is of the bare characters. */
    private static String normalise(String code) {
        return code == null ? "" : code.replace("-", "").replace(" ", "").toUpperCase();
    }

    private String otpauthUri(String secret, String email) {
        // The label carries the issuer twice — in the path and as a parameter —
        // because different authenticator apps read different ones, and an
        // entry that just says the email address is unidentifiable once someone
        // has a few.
        String label = java.net.URLEncoder.encode(issuerLabel + ":" + email,
                java.nio.charset.StandardCharsets.UTF_8);
        String issuer = java.net.URLEncoder.encode(issuerLabel,
                java.nio.charset.StandardCharsets.UTF_8);

        return "otpauth://totp/%s?secret=%s&issuer=%s&algorithm=SHA1&digits=6&period=30"
                .formatted(label, secret, issuer);
    }

    private static String sha256(String value) {
        try {
            byte[] digest = MessageDigest.getInstance("SHA-256")
                    .digest(value.getBytes(java.nio.charset.StandardCharsets.UTF_8));
            return java.util.HexFormat.of().formatHex(digest);
        } catch (Exception e) {
            throw new IllegalStateException("SHA-256 unavailable", e);
        }
    }
}
