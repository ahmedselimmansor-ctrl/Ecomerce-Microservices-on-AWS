package dev.souq.identity.auth;

import java.time.Duration;
import java.time.Instant;
import java.util.List;
import java.util.Optional;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.jdbc.core.namedparam.MapSqlParameterSource;
import org.springframework.jdbc.core.namedparam.NamedParameterJdbcTemplate;
import org.springframework.transaction.annotation.Transactional;

import dev.souq.identity.token.TokenService;
import dev.souq.identity.token.TokenService.TokenPair;

/**
 * Registration and login.
 *
 * <p>Three things in here are less obvious than they look, and each exists
 * because the obvious version leaks information or is exploitable.
 *
 * <ol>
 *   <li><b>Login never reveals whether an account exists.</b> An unknown email
 *       and a wrong password produce the same response, in the same time, with
 *       the same error code. Returning "no such user" turns login into an
 *       account-enumeration oracle, and the timing difference does the same
 *       thing even when the message does not.</li>
 *   <li><b>Registration does the same.</b> "That email is taken" tells an
 *       attacker who has an account here. The response is identical either way
 *       and the existing account is emailed instead.</li>
 *   <li><b>Lockout is per-account AND per-IP.</b> Credential stuffing spreads
 *       across thousands of accounts from a handful of IPs, so per-account
 *       counting never trips. A targeted attack does the opposite.</li>
 * </ol>
 */
public class AuthService {

    private static final Logger log = LoggerFactory.getLogger(AuthService.class);

    private final NamedParameterJdbcTemplate jdbc;
    private final PasswordService passwords;
    private final TokenService tokens;
    private final int maxFailuresPerAccount;
    private final int maxFailuresPerIp;
    private final Duration lockoutWindow;

    public AuthService(NamedParameterJdbcTemplate jdbc,
                       PasswordService passwords,
                       TokenService tokens,
                       int maxFailuresPerAccount,
                       int maxFailuresPerIp,
                       Duration lockoutWindow) {
        this.jdbc = jdbc;
        this.passwords = passwords;
        this.tokens = tokens;
        this.maxFailuresPerAccount = maxFailuresPerAccount;
        this.maxFailuresPerIp = maxFailuresPerIp;
        this.lockoutWindow = lockoutWindow;
    }

    // -----------------------------------------------------------------------

    public record RegisterCommand(String email, String password, String fullName,
                                  String locale, String acceptedTermsVersion,
                                  String ipAddress) {}

    public record LoginCommand(String email, String password, String mfaCode,
                               String ipAddress, String userAgent,
                               String deviceFingerprint) {}

    public record AuthResult(TokenPair tokens, String userId, List<String> roles) {}

    /** Signals a rejected login. Deliberately carries no detail about why. */
    public static class AuthenticationFailed extends RuntimeException {
        public AuthenticationFailed() { super("email or password is incorrect"); }
    }

    public static class AccountLocked extends RuntimeException {
        private final Duration retryAfter;
        public AccountLocked(Duration retryAfter) {
            super("too many failed attempts");
            this.retryAfter = retryAfter;
        }
        public Duration retryAfter() { return retryAfter; }
    }

    public static class WeakPassword extends RuntimeException {
        private final String reasonCode;
        public WeakPassword(String reasonCode, String message) {
            super(message);
            this.reasonCode = reasonCode;
        }
        public String reasonCode() { return reasonCode; }
    }

    public static class MfaRequired extends RuntimeException {
        public MfaRequired() { super("a multi-factor code is required"); }
    }

    // -----------------------------------------------------------------------

    /**
     * Registers a user, or silently does nothing if the email already exists.
     *
     * <p>Returns no indication of which happened. The caller always responds
     * "check your email"; a real registration gets a verification link and a
     * duplicate gets a "someone tried to register with your address" notice.
     * That is the only way to make the two indistinguishable to an attacker
     * while still being useful to the real owner.
     */
    @Transactional
    public void register(RegisterCommand cmd) {
        var validation = passwords.validate(cmd.password(), cmd.email(), List.of());
        if (!validation.ok()) {
            // A weak password IS reported. It is about this request, not about
            // whether an account exists, so it leaks nothing.
            throw new WeakPassword(validation.reasonCode(), validation.message());
        }

        String userId = "usr_" + Ulid.next();
        String hash = passwords.hash(cmd.password());

        // ON CONFLICT DO NOTHING against the case-insensitive unique index.
        // Two accounts differing only by capitalisation is an account-takeover
        // vector at the password-reset step.
        int inserted = jdbc.update("""
                INSERT INTO users (id, email, full_name, locale, accepted_terms_version)
                VALUES (:id, :email, :name, :locale, :terms)
                ON CONFLICT (lower(email)) DO NOTHING
                """,
                new MapSqlParameterSource()
                        .addValue("id", userId)
                        .addValue("email", cmd.email())
                        .addValue("name", cmd.fullName())
                        .addValue("locale", cmd.locale())
                        .addValue("terms", cmd.acceptedTermsVersion()));

        if (inserted == 0) {
            log.info("registration attempted for an existing address; notifying the owner");
            enqueueNotification(cmd.email(), "duplicate_registration_attempt");
            return;
        }

        jdbc.update("INSERT INTO credentials (user_id, password_hash) VALUES (:id, :hash)",
                new MapSqlParameterSource().addValue("id", userId).addValue("hash", hash));
        jdbc.update("INSERT INTO roles (user_id, role) VALUES (:id, 'CUSTOMER')",
                new MapSqlParameterSource("id", userId));

        enqueueNotification(cmd.email(), "email_verification");
        log.info("user registered: {}", userId);
    }

    /**
     * Authenticates.
     *
     * <p>Every failure path costs the same: the lockout check runs first, the
     * password verification always runs (against a dummy hash for an unknown
     * email), and every rejection throws the same exception.
     */
    @Transactional
    public AuthResult login(LoginCommand cmd) {
        Duration lockout = lockoutRemaining(cmd.email(), cmd.ipAddress());
        if (!lockout.isZero()) {
            recordAttempt(cmd, null, false, "LOCKED_OUT");
            throw new AccountLocked(lockout);
        }

        var found = loadForLogin(cmd.email());

        if (found.isEmpty()) {
            // Burn the same CPU as a real verification. Without this, an
            // unknown email returns in microseconds and a wrong password takes
            // ~50ms, which is a reliable enumeration oracle.
            passwords.verifyAgainstDummy();
            recordAttempt(cmd, null, false, "NO_SUCH_USER");
            throw new AuthenticationFailed();
        }

        var user = found.get();

        if (!passwords.verify(cmd.password(), user.passwordHash())) {
            recordAttempt(cmd, user.id(), false, "BAD_PASSWORD");
            throw new AuthenticationFailed();
        }

        if (!user.enabled()) {
            // Same exception as a bad password. Telling a disabled account
            // holder that their account exists but is disabled is more
            // information than a stranger should get.
            recordAttempt(cmd, user.id(), false, "ACCOUNT_DISABLED");
            throw new AuthenticationFailed();
        }

        List<String> amr = new java.util.ArrayList<>(List.of("pwd"));

        if (user.mfaEnabled()) {
            if (cmd.mfaCode() == null || cmd.mfaCode().isBlank()) {
                // Reported honestly: the password was already correct, so this
                // reveals nothing a successful login would not.
                recordAttempt(cmd, user.id(), false, "MFA_REQUIRED");
                throw new MfaRequired();
            }
            if (!Totp.verify(user.mfaSecret(), cmd.mfaCode())) {
                recordAttempt(cmd, user.id(), false, "BAD_MFA_CODE");
                throw new AuthenticationFailed();
            }
            // The admin dashboard requires "mfa" in amr — see CONTRACTS.md §7.
            amr.add("mfa");
        }

        // Cost parameters were raised since this hash was made. Re-hash now,
        // while we have the plaintext; there is no other opportunity.
        if (passwords.needsRehash(user.passwordHash())) {
            jdbc.update("""
                    UPDATE credentials SET password_hash = :hash, needs_rehash = FALSE
                     WHERE user_id = :id
                    """,
                    new MapSqlParameterSource()
                            .addValue("hash", passwords.hash(cmd.password()))
                            .addValue("id", user.id()));
            log.info("re-hashed the password for {} at the current cost", user.id());
        }

        recordAttempt(cmd, user.id(), true, null);

        var pair = tokens.issue(user.id(), user.roles(), amr, cmd.deviceFingerprint());
        return new AuthResult(pair, user.id(), user.roles());
    }

    // -----------------------------------------------------------------------

    private record LoginRow(String id, String passwordHash, boolean enabled,
                            boolean mfaEnabled, String mfaSecret, List<String> roles) {}

    private Optional<LoginRow> loadForLogin(String email) {
        var rows = jdbc.query("""
                SELECT u.id, c.password_hash, u.enabled, u.mfa_enabled, u.mfa_secret,
                       coalesce(array_agg(r.role) FILTER (WHERE r.role IS NOT NULL), '{}') AS roles
                  FROM users u
                  JOIN credentials c ON c.user_id = u.id
                  LEFT JOIN roles r ON r.user_id = u.id
                 WHERE lower(u.email) = lower(:email)
                 GROUP BY u.id, c.password_hash, u.enabled, u.mfa_enabled, u.mfa_secret
                """,
                new MapSqlParameterSource("email", email),
                (rs, i) -> new LoginRow(
                        rs.getString("id"), rs.getString("password_hash"),
                        rs.getBoolean("enabled"), rs.getBoolean("mfa_enabled"),
                        rs.getString("mfa_secret"),
                        List.of((String[]) rs.getArray("roles").getArray())));
        return rows.stream().findFirst();
    }

    /**
     * How long the caller must wait, or ZERO.
     *
     * <p>Both counts in one query. Per-account catches a targeted attack;
     * per-IP catches credential stuffing, which spreads across so many accounts
     * that no single account ever trips its own limit.
     */
    private Duration lockoutRemaining(String email, String ip) {
        Instant since = Instant.now().minus(lockoutWindow);

        var counts = jdbc.queryForObject("""
                SELECT
                  (SELECT count(*) FROM login_attempts
                    WHERE lower(email) = lower(:email) AND NOT succeeded AND created_at > :since) AS by_account,
                  (SELECT count(*) FROM login_attempts
                    WHERE ip_address = :ip::inet AND NOT succeeded AND created_at > :since) AS by_ip
                """,
                new MapSqlParameterSource()
                        .addValue("email", email)
                        .addValue("since", java.sql.Timestamp.from(since))
                        .addValue("ip", ip),
                (rs, i) -> new int[]{rs.getInt("by_account"), rs.getInt("by_ip")});

        if (counts == null) return Duration.ZERO;

        if (counts[0] >= maxFailuresPerAccount || counts[1] >= maxFailuresPerIp) {
            log.warn("login locked out: {} failures for this account, {} for this IP",
                    counts[0], counts[1]);
            return lockoutWindow;
        }
        return Duration.ZERO;
    }

    private void recordAttempt(LoginCommand cmd, String userId, boolean succeeded, String reason) {
        jdbc.update("""
                INSERT INTO login_attempts (email, user_id, succeeded, failure_reason, ip_address, user_agent)
                VALUES (:email, :userId, :ok, :reason, :ip::inet, :ua)
                """,
                new MapSqlParameterSource()
                        .addValue("email", cmd.email())
                        .addValue("userId", userId)
                        .addValue("ok", succeeded)
                        .addValue("reason", reason)
                        .addValue("ip", cmd.ipAddress())
                        .addValue("ua", cmd.userAgent()));
    }

    /**
     * Writes a notification command to the outbox.
     *
     * <p>Outbox, not a direct call. The registration row and the "verify your
     * email" command commit together or not at all — otherwise a crash between
     * them leaves an account nobody can verify.
     */
    private void enqueueNotification(String email, String template) {
        String eventId = Ulid.next();
        String payload = """
                {"specversion":"1.0","id":"%s","source":"souq/identity-service",
                 "type":"souq.notify.v1","time":"%s","datacontenttype":"application/json",
                 "data":{"channel":"EMAIL","to":"%s","template":"%s","locale":"en-GB",
                         "params":{},"dedupeKey":"%s:%s"}}
                """.formatted(eventId, Instant.now(), email, template, template, email);

        jdbc.update("""
                INSERT INTO outbox (aggregate_type, aggregate_id, event_id, event_type,
                                    topic, partition_key, payload)
                VALUES ('user', :email, :eventId, 'souq.notify.v1',
                        'souq.notification.commands.v1', :email, :payload::jsonb)
                ON CONFLICT (event_id) DO NOTHING
                """,
                new MapSqlParameterSource()
                        .addValue("email", email)
                        .addValue("eventId", eventId)
                        .addValue("payload", payload));
    }
}
