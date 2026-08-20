package dev.souq.identity.token;

import java.security.MessageDigest;
import java.security.SecureRandom;
import java.time.Duration;
import java.time.Instant;
import java.util.HexFormat;
import java.util.List;
import java.util.Optional;
import java.util.UUID;

import com.nimbusds.jose.JOSEException;
import com.nimbusds.jose.JWSAlgorithm;
import com.nimbusds.jose.JWSHeader;
import com.nimbusds.jwt.JWTClaimsSet;
import com.nimbusds.jwt.SignedJWT;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.transaction.annotation.Transactional;

/**
 * Issues and rotates tokens.
 *
 * <p>Two decisions here carry most of the platform's security posture, and both are the
 * unobvious option:
 *
 * <p><b>1. Access tokens are verified locally, everywhere.</b> Every service checks the RS256
 * signature against a cached JWKS rather than calling this service. The alternative — an
 * introspection call per request — would make identity-service a synchronous dependency of
 * literally every request in the platform, so its bad day becomes everyone's outage. The cost is
 * that a revoked token stays valid until it expires, which is why the TTL is 15 minutes and not
 * 15 hours.
 *
 * <p><b>2. Refresh tokens rotate, with reuse detection.</b> Each refresh mints a new token and
 * retires the old one. Presenting a retired token means either an attacker replaying a stolen
 * one or the legitimate user replaying after a network failure — and we cannot tell which. So we
 * revoke the entire family, which logs the user out everywhere. That is a deliberate, occasional
 * inconvenience in exchange for capping stolen-token damage at one use. See
 * {@link #refresh(String, String, String)}.
 */
public class TokenService {

    private static final Logger log = LoggerFactory.getLogger(TokenService.class);

    /** Short, because local verification means we cannot revoke an access token early. */
    static final Duration ACCESS_TTL = Duration.ofMinutes(15);
    static final Duration REFRESH_TTL = Duration.ofDays(30);

    private final SigningKeyProvider keys;
    private final RefreshTokenRepository refreshTokens;
    private final SecureRandom random = new SecureRandom();
    private final String issuer;
    private final List<String> audience;

    public TokenService(SigningKeyProvider keys,
                        RefreshTokenRepository refreshTokens,
                        String issuer,
                        List<String> audience) {
        this.keys = keys;
        this.refreshTokens = refreshTokens;
        this.issuer = issuer;
        this.audience = audience;
    }

    /**
     * A freshly minted pair, plus who it is for.
     *
     * <p>{@code userId} and {@code roles} are carried here rather than left for
     * the caller to re-derive. The alternative is parsing the access token we
     * just signed to read back claims we already had in hand — work that also
     * needs a "trust this one, we made it" exception to the verification path.
     * Returning them is strictly cheaper and removes that exception.
     */
    public record TokenPair(String accessToken, String refreshToken, long expiresInSeconds,
                            String userId, List<String> roles) {}

    /**
     * Mints a fresh pair at the start of a session.
     *
     * @param sessionId groups every token derived from one login. Revoking a family revokes this.
     */
    @Transactional
    public TokenPair issue(String userId, List<String> roles, List<String> amr, String deviceFingerprint) {
        String sessionId = UUID.randomUUID().toString();
        String access = signAccessToken(userId, roles, amr, sessionId);
        String refresh = mintRefreshToken(userId, sessionId, null, deviceFingerprint);
        return new TokenPair(access, refresh, ACCESS_TTL.toSeconds(), userId, roles);
    }

    /**
     * Rotates a refresh token.
     *
     * <p>The reuse-detection branch is the important one. A refresh token is single-use: once
     * exchanged, it is marked {@code USED} and its successor takes over. If a {@code USED} token
     * comes back, exactly one of two things happened:
     *
     * <ul>
     *   <li>an attacker stole it and is replaying it, or</li>
     *   <li>the legitimate client never received our response and is retrying.</li>
     * </ul>
     *
     * <p>We cannot distinguish them, and guessing wrong in the permissive direction hands an
     * attacker a live session. So both are treated as compromise: the whole family is revoked and
     * the user re-authenticates. This is the OAuth 2.1 recommendation and it is worth the
     * occasional spurious logout.
     *
     * @throws TokenReuseException when a retired token is presented; the caller must return 401
     *                             with {@code REFRESH_TOKEN_REUSED} and clear the cookie.
     */
    @Transactional
    public TokenPair refresh(String presentedToken, String deviceFingerprint, String requestId) {
        String hash = hash(presentedToken);

        RefreshToken stored = refreshTokens.findByTokenHash(hash)
                .orElseThrow(() -> new InvalidTokenException("refresh token not recognised"));

        if (stored.state() == RefreshToken.State.USED) {
            // Reuse. Burn the family.
            int revoked = refreshTokens.revokeFamily(stored.sessionId(), "REUSE_DETECTED");
            log.warn("refresh token reuse detected; revoked {} tokens in session {} for user {} (requestId={})",
                    revoked, stored.sessionId(), stored.userId(), requestId);
            throw new TokenReuseException(stored.userId(), stored.sessionId());
        }

        if (stored.state() == RefreshToken.State.REVOKED) {
            throw new InvalidTokenException("refresh token has been revoked");
        }

        if (stored.expiresAt().isBefore(Instant.now())) {
            throw new InvalidTokenException("refresh token has expired");
        }

        // A token presented from a different device is suspicious but not
        // conclusive: real users change networks, and browsers change their
        // user agent on update. Log it for the risk engine rather than
        // blocking, because a false positive here locks out a paying customer.
        if (deviceFingerprint != null
                && stored.deviceFingerprint() != null
                && !stored.deviceFingerprint().equals(deviceFingerprint)) {
            log.info("refresh from a changed device fingerprint for user {} (session {})",
                    stored.userId(), stored.sessionId());
        }

        refreshTokens.markUsed(stored.id());

        var user = refreshTokens.loadUserSnapshot(stored.userId())
                .orElseThrow(() -> new InvalidTokenException("user no longer exists"));

        // A user disabled since the last refresh must not get a new access
        // token — this is the one revocation path that works within the TTL.
        if (!user.enabled()) {
            refreshTokens.revokeFamily(stored.sessionId(), "USER_DISABLED");
            throw new InvalidTokenException("account is disabled");
        }

        String access = signAccessToken(user.id(), user.roles(), stored.amr(), stored.sessionId());
        String next = mintRefreshToken(user.id(), stored.sessionId(), stored.id(), deviceFingerprint);

        return new TokenPair(access, next, ACCESS_TTL.toSeconds(), user.id(), user.roles());
    }

    /**
     * Ends the session a presented refresh token belongs to.
     *
     * <p>This is what logout calls. It revokes the whole family rather than the
     * single presented token: the client holds exactly one live token per
     * session, so anything else in the family is either already used or a copy
     * someone else is holding — and on logout we want both gone.
     *
     * <p>Unknown tokens are ignored rather than reported. Logout has no
     * business telling a caller whether a token it presented was real.
     */
    @Transactional
    public void revokeByToken(String presentedToken, String reason) {
        refreshTokens.findByTokenHash(hash(presentedToken))
                .ifPresent(stored -> revokeSession(stored.sessionId(), reason));
    }

    /** Ends one session. Used by support tooling and by password change. */
    @Transactional
    public void revokeSession(String sessionId, String reason) {
        int n = refreshTokens.revokeFamily(sessionId, reason);
        log.info("revoked {} refresh tokens in session {} ({})", n, sessionId, reason);
    }

    /** Ends every session for a user. Used on password change and by support. */
    @Transactional
    public void revokeAllForUser(String userId, String reason) {
        int n = refreshTokens.revokeAllForUser(userId, reason);
        log.warn("revoked all {} refresh tokens for user {} ({})", n, userId, reason);
    }

    // -----------------------------------------------------------------------

    private String signAccessToken(String userId, List<String> roles, List<String> amr, String sessionId) {
        Instant now = Instant.now();

        JWTClaimsSet claims = new JWTClaimsSet.Builder()
                .subject(userId)
                .issuer(issuer)
                .audience(audience)
                .issueTime(java.util.Date.from(now))
                .expirationTime(java.util.Date.from(now.plus(ACCESS_TTL)))
                // jti lets a specific token be denylisted in the rare case
                // that 15 minutes is too long to wait.
                .jwtID(UUID.randomUUID().toString())
                .claim("roles", roles)
                // sid ties the access token to its refresh family, so revoking
                // the family is auditable against the access tokens it issued.
                .claim("sid", sessionId)
                // amr records HOW the user authenticated. The admin dashboard
                // requires "mfa" to be present (docs/CONTRACTS.md §7) — without
                // this claim that check is impossible to make.
                .claim("amr", amr)
                .claim("scope", String.join(" ", scopesFor(roles)))
                .build();

        var signingKey = keys.current();

        // kid is not optional. Without it a verifier cannot pick the right key
        // during a rotation, and every token minted mid-rotation fails.
        JWSHeader header = new JWSHeader.Builder(JWSAlgorithm.RS256)
                .keyID(signingKey.keyId())
                .type(com.nimbusds.jose.JOSEObjectType.JWT)
                .build();

        SignedJWT jwt = new SignedJWT(header, claims);
        try {
            // On the KMS path this is a network call, so it can fail for
            // reasons that have nothing to do with the token: a throttle, a
            // disabled key, an expired IRSA credential. Wrapped rather than
            // propagated raw, because the caller's contract is "issuing failed"
            // and the KMS detail is already in the log at ERROR.
            jwt.sign(signingKey.signer());
        } catch (JOSEException e) {
            throw new IllegalStateException("could not sign the access token", e);
        }
        return jwt.serialize();
    }

    /**
     * Mints an opaque refresh token.
     *
     * <p>Opaque, not a JWT. A refresh token is a bearer credential with a 30-day life; making it
     * self-describing means a leaked one is usable without us ever seeing it. Opaque means every
     * use hits this database, which is what makes rotation and reuse detection possible at all.
     *
     * <p>Stored as a SHA-256 hash. A database dump must not hand over live sessions. SHA-256
     * rather than Argon2id here specifically: these are 256 bits of {@link SecureRandom} output,
     * so there is no dictionary to attack and no reason to pay a slow KDF on a hot path.
     * (Passwords are a different matter and use Argon2id.)
     */
    private String mintRefreshToken(String userId, String sessionId, String parentId, String deviceFingerprint) {
        byte[] raw = new byte[32];
        random.nextBytes(raw);
        String token = java.util.Base64.getUrlEncoder().withoutPadding().encodeToString(raw);

        refreshTokens.insert(new RefreshToken(
                UUID.randomUUID().toString(),
                userId,
                sessionId,
                parentId,
                hash(token),
                RefreshToken.State.ACTIVE,
                deviceFingerprint,
                List.of(),
                Instant.now(),
                Instant.now().plus(REFRESH_TTL)
        ));

        return token;
    }

    private static String hash(String token) {
        try {
            MessageDigest md = MessageDigest.getInstance("SHA-256");
            return HexFormat.of().formatHex(md.digest(token.getBytes(java.nio.charset.StandardCharsets.UTF_8)));
        } catch (Exception e) {
            throw new IllegalStateException("SHA-256 unavailable", e);
        }
    }

    private static List<String> scopesFor(List<String> roles) {
        if (roles.contains("ADMIN")) {
            return List.of("orders:read", "orders:write", "catalog:read", "catalog:write",
                    "inventory:read", "inventory:write", "users:read", "users:write");
        }
        if (roles.contains("OPS")) {
            return List.of("orders:read", "orders:write", "inventory:read", "inventory:write", "saga:read");
        }
        if (roles.contains("SUPPORT")) {
            return List.of("orders:read", "users:read");
        }
        if (roles.contains("MERCHANT")) {
            return List.of("catalog:read", "catalog:write", "orders:read");
        }
        return List.of("orders:read", "orders:write", "catalog:read", "cart:write");
    }

    // -----------------------------------------------------------------------

    /** Signals reuse of a retired refresh token. The caller must clear the cookie. */
    public static class TokenReuseException extends RuntimeException {
        private final String userId;
        private final String sessionId;

        public TokenReuseException(String userId, String sessionId) {
            super("refresh token reuse detected; the session family has been revoked");
            this.userId = userId;
            this.sessionId = sessionId;
        }

        public String userId() { return userId; }
        public String sessionId() { return sessionId; }
    }

    public static class InvalidTokenException extends RuntimeException {
        public InvalidTokenException(String message) { super(message); }
    }

    // -----------------------------------------------------------------------

    public record RefreshToken(
            String id,
            String userId,
            String sessionId,
            String parentId,
            String tokenHash,
            State state,
            String deviceFingerprint,
            List<String> amr,
            Instant createdAt,
            Instant expiresAt
    ) {
        public enum State { ACTIVE, USED, REVOKED }
    }

    public record UserSnapshot(String id, List<String> roles, boolean enabled) {}

    public interface RefreshTokenRepository {
        Optional<RefreshToken> findByTokenHash(String hash);
        void insert(RefreshToken token);
        void markUsed(String id);
        int revokeFamily(String sessionId, String reason);
        int revokeAllForUser(String userId, String reason);
        Optional<UserSnapshot> loadUserSnapshot(String userId);
    }

    /** Supplies the current signing key. Backed by AWS KMS in production. */
    /**
     * Where signatures come from.
     *
     * <p>A {@link com.nimbusds.jose.JWSSigner}, not a private key. That
     * indirection is the whole reason KMS signing is expressible: an interface
     * returning {@code RSAPrivateKey} can only describe a key this process
     * holds, and the entire point of KMS is that it does not.
     *
     * <p>Locally this is an {@code RSASSASigner} over a generated pair; in
     * production it is a {@code KmsJwsSigner} that turns each signature into an
     * API call. {@link TokenService} cannot tell the difference and must not
     * need to.
     */
    public interface SigningKeyProvider {
        record Key(String keyId, com.nimbusds.jose.JWSSigner signer) {}
        Key current();
    }
}
