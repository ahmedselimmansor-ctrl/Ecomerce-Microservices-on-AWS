package dev.souq.identity.auth;

import java.security.MessageDigest;
import java.security.SecureRandom;
import java.time.Instant;
import java.util.Base64;
import java.util.List;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.jdbc.core.namedparam.MapSqlParameterSource;
import org.springframework.jdbc.core.namedparam.NamedParameterJdbcTemplate;
import org.springframework.transaction.annotation.Transactional;

import dev.souq.identity.token.TokenService;
import dev.souq.identity.user.JdbcUserRepository;

/**
 * Password change, password reset, and email verification.
 *
 * <p>The reset flow is where most implementations go wrong, so the reasoning is
 * spelled out at each step:
 *
 * <ol>
 *   <li><b>Only the hash of a reset token is stored.</b> A database dump then
 *       contains no usable token. Storing the token itself makes any read-only
 *       leak — a backup, a replica, a log line — an account-takeover kit for
 *       every pending reset.</li>
 *   <li><b>"Forgot password" always returns the same thing.</b> Same as
 *       registration and login: it is an unauthenticated endpoint, so any
 *       difference in response is a free account-enumeration oracle.</li>
 *   <li><b>Consuming the token and revoking sessions happen in one
 *       transaction.</b> If the reset commits and the revocation does not, an
 *       attacker whose session prompted the reset keeps it.</li>
 * </ol>
 */
public class ProfileService {

    private static final Logger log = LoggerFactory.getLogger(ProfileService.class);
    private static final SecureRandom RANDOM = new SecureRandom();

    private final NamedParameterJdbcTemplate jdbc;
    private final JdbcUserRepository users;
    private final PasswordService passwords;
    private final TokenService tokens;

    public ProfileService(NamedParameterJdbcTemplate jdbc, JdbcUserRepository users,
                          PasswordService passwords, TokenService tokens) {
        this.jdbc = jdbc;
        this.users = users;
        this.passwords = passwords;
        this.tokens = tokens;
    }

    public static class UserNotFound extends RuntimeException {
        public UserNotFound(String id) { super("no user " + id); }
    }

    public static class WrongCurrentPassword extends RuntimeException {
        public WrongCurrentPassword() { super("the current password is incorrect"); }
    }

    public static class PasswordReused extends RuntimeException {
        public PasswordReused() { super("that password was used recently"); }
    }

    // -----------------------------------------------------------------------

    /**
     * Changes a password for a signed-in user.
     *
     * <p>Every other session is revoked, and the current one is kept. That
     * asymmetry is the point of changing a password after a suspected
     * compromise: the attacker is logged out, the user is not. Revoking
     * everything including the caller's own session is the more common
     * implementation and it trains users to avoid the feature.
     */
    @Transactional
    public void changePassword(String userId, String currentSessionId,
                               String currentPassword, String newPassword) {

        String existingHash = users.currentPasswordHash(userId)
                .orElseThrow(() -> new UserNotFound(userId));

        if (!passwords.verify(currentPassword, existingHash)) {
            throw new WrongCurrentPassword();
        }

        applyNewPassword(userId, newPassword, existingHash);

        int revoked = jdbc.update("""
                UPDATE refresh_tokens
                   SET state = 'REVOKED', revoked_reason = 'PASSWORD_CHANGED'
                 WHERE user_id = :userId AND session_id <> :keep AND state = 'ACTIVE'
                """,
                new MapSqlParameterSource().addValue("userId", userId).addValue("keep", currentSessionId));

        log.info("password changed for {}; revoked {} other sessions", userId, revoked);
    }

    /**
     * Starts a reset.
     *
     * <p>Returns nothing and never throws for an unknown address. A token is
     * only created when the account exists, but the caller cannot tell.
     */
    @Transactional
    public void requestPasswordReset(String email) {
        var userId = users.findIdByEmail(email);

        if (userId.isEmpty()) {
            log.info("password reset requested for an address with no account");
            return;
        }

        // Any outstanding token is invalidated. Otherwise requesting a second
        // reset leaves the first email's link live, and "I clicked the old one
        // by mistake" becomes a support ticket rather than an error.
        jdbc.update("""
                UPDATE password_reset_tokens SET used = TRUE
                 WHERE user_id = :userId AND NOT used
                """,
                new MapSqlParameterSource("userId", userId.get()));

        byte[] raw = new byte[32];
        RANDOM.nextBytes(raw);
        String token = Base64.getUrlEncoder().withoutPadding().encodeToString(raw);

        jdbc.update("""
                INSERT INTO password_reset_tokens (token_hash, user_id)
                VALUES (:hash, :userId)
                """,
                new MapSqlParameterSource()
                        .addValue("hash", sha256(token))
                        .addValue("userId", userId.get()));

        // Same transaction as the token row: a crash between them would send a
        // link that resolves to nothing, or store a token nobody was told about.
        enqueueResetEmail(email, token);
    }

    /**
     * Completes a reset.
     *
     * <p>The token is consumed with a conditional {@code UPDATE} that also
     * checks expiry, so two requests racing with the same token produce exactly
     * one winner. Reading the row and then updating it lets both through.
     */
    @Transactional
    public void completePasswordReset(String token, String newPassword) {
        var claimed = jdbc.query("""
                UPDATE password_reset_tokens
                   SET used = TRUE
                 WHERE token_hash = :hash AND NOT used AND expires_at > now()
                RETURNING user_id
                """,
                new MapSqlParameterSource("hash", sha256(token)),
                (rs, i) -> rs.getString("user_id"));

        if (claimed.isEmpty()) {
            // Unknown, already used, or expired — reported identically. Telling
            // the caller which one distinguishes a guessed token from a stale
            // one, which is exactly what a brute-force needs to know.
            throw new TokenService.InvalidTokenException("this reset link is no longer valid");
        }

        String userId = claimed.get(0);
        String existingHash = users.currentPasswordHash(userId).orElseThrow(() -> new UserNotFound(userId));

        applyNewPassword(userId, newPassword, existingHash);

        // Everything, including any session the attacker holds. Unlike a
        // deliberate password change, a reset means the user could not sign in,
        // so there is no session of theirs worth preserving.
        tokens.revokeAllForUser(userId, "PASSWORD_RESET");
        log.info("password reset completed for {}", userId);
    }

    @Transactional
    public boolean verifyEmail(String userId) {
        return users.markEmailVerified(userId) == 1;
    }

    // -----------------------------------------------------------------------

    private void applyNewPassword(String userId, String newPassword, String existingHash) {
        var validation = passwords.validate(newPassword, null, users.previousHashes(userId));
        if (!validation.ok()) {
            throw new AuthService.WeakPassword(validation.reasonCode(), validation.message());
        }

        // The current hash is checked separately from the history: it is not in
        // previous_hashes yet, and "change it to the same one" is the single
        // most common attempt.
        if (passwords.verify(newPassword, existingHash)) {
            throw new PasswordReused();
        }

        users.replacePassword(userId, passwords.hash(newPassword), existingHash);
    }

    /**
     * SHA-256, not Argon2id.
     *
     * <p>Deliberate, and the opposite of the right answer for passwords. A
     * reset token is 32 bytes from a CSPRNG, so there is nothing to brute-force
     * and a slow hash buys no security. What it would cost is a lookup: Argon2id
     * is salted per row, so finding a token would mean scanning the table and
     * verifying every candidate. SHA-256 keeps the primary-key lookup and
     * still leaves the stored value useless to anyone who reads the table.
     */
    private static String sha256(String value) {
        try {
            byte[] digest = MessageDigest.getInstance("SHA-256")
                    .digest(value.getBytes(java.nio.charset.StandardCharsets.UTF_8));
            StringBuilder out = new StringBuilder(64);
            for (byte b : digest) {
                out.append(Character.forDigit((b >> 4) & 0xF, 16));
                out.append(Character.forDigit(b & 0xF, 16));
            }
            return out.toString();
        } catch (Exception e) {
            throw new IllegalStateException("SHA-256 unavailable", e);
        }
    }

    private void enqueueResetEmail(String email, String token) {
        String eventId = Ulid.next();
        String payload = """
                {"specversion":"1.0","id":"%s","source":"souq/identity-service",
                 "type":"souq.notify.v1","time":"%s","datacontenttype":"application/json",
                 "data":{"channel":"EMAIL","to":"%s","template":"password_reset",
                         "locale":"en-GB","params":{"token":"%s"},"dedupeKey":"reset:%s"}}
                """.formatted(eventId, Instant.now(), email, token, eventId);

        jdbc.update("""
                INSERT INTO outbox (aggregate_type, aggregate_id, event_id, event_type,
                                    topic, partition_key, payload)
                VALUES ('user', :email, :eventId, 'souq.notify.v1',
                        'souq.notification.commands.v1', :email, :payload::jsonb)
                """,
                new MapSqlParameterSource()
                        .addValue("email", email)
                        .addValue("eventId", eventId)
                        .addValue("payload", payload));
    }

    /** Exposed for the reset controller, which has no other reason to know the format. */
    public List<String> supportedResetChannels() {
        return List.of("EMAIL");
    }
}
