package dev.souq.catalog.security;

import java.time.Duration;
import java.time.Instant;
import java.util.Date;
import java.util.List;
import java.util.Map;
import java.util.concurrent.atomic.AtomicReference;

import com.nimbusds.jose.JWSAlgorithm;
import com.nimbusds.jose.crypto.RSASSAVerifier;
import com.nimbusds.jose.jwk.JWKSet;
import com.nimbusds.jose.jwk.RSAKey;
import com.nimbusds.jwt.SignedJWT;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.web.client.RestClient;

/**
 * Verifies access tokens against identity-service's published keys.
 *
 * <p>Local verification, per docs/CONTRACTS.md §7. The alternative — asking
 * identity-service to introspect every token — makes it a synchronous
 * dependency of every request in the platform, so its bad afternoon becomes a
 * total outage. The cost is that a revoked token stays valid until it expires,
 * which is why the access TTL is 15 minutes.
 *
 * <p>Four things here are the difference between this working and this being an
 * auth bypass:
 *
 * <ul>
 *   <li><b>The algorithm is pinned before the header is trusted.</b> The JWS
 *       header is attacker-supplied. A verifier that dispatches on {@code alg}
 *       accepts {@code "none"}, and accepts {@code HS256} using the RSA public
 *       key as the HMAC secret — a key we publish at a public URL. Both forge
 *       any token.</li>
 *   <li><b>An unknown {@code kid} triggers at most one refetch, rate-limited.</b>
 *       Without the limit, a stream of tokens carrying random key ids is a
 *       free amplified DoS against identity-service.</li>
 *   <li><b>A failed refetch keeps serving the stale key set.</b> Identity being
 *       briefly unreachable must not log out every user of every service.</li>
 *   <li><b>Expiry has no leeway.</b> Skew allowances silently extend the only
 *       bound on how long a revoked session survives.</li>
 * </ul>
 */
public class JwksVerifier {

    private static final Logger log = LoggerFactory.getLogger(JwksVerifier.class);

    /** Never refetch more often than this, however many unknown kids arrive. */
    private static final Duration MIN_REFETCH_INTERVAL = Duration.ofSeconds(30);

    private final RestClient http;
    private final String jwksUrl;
    private final String issuer;
    private final List<String> audience;
    private final Duration cacheTtl;

    private final AtomicReference<Snapshot> cache = new AtomicReference<>(Snapshot.empty());

    private record Snapshot(Map<String, RSAKey> keys, Instant fetchedAt, Instant lastAttempt) {
        static Snapshot empty() {
            return new Snapshot(Map.of(), Instant.EPOCH, Instant.EPOCH);
        }
        boolean isStale(Duration ttl) {
            return fetchedAt.plus(ttl).isBefore(Instant.now());
        }
    }

    public JwksVerifier(RestClient http, String jwksUrl, String issuer,
                        List<String> audience, Duration cacheTtl) {
        this.http = http;
        this.jwksUrl = jwksUrl;
        this.issuer = issuer;
        this.audience = audience;
        this.cacheTtl = cacheTtl;
    }

    public record Principal(String userId, List<String> roles, List<String> amr,
                            String sessionId, String tokenId) {

        public boolean hasRole(String role) { return roles.contains(role); }

        /** docs/CONTRACTS.md §7: admin surfaces need a privileged role AND an MFA login. */
        public boolean isAdmin() {
            return (hasRole("ADMIN") || hasRole("OPS")) && amr.contains("mfa");
        }
    }

    public static class InvalidToken extends RuntimeException {
        public InvalidToken(String message) { super(message); }
    }

    public Principal verify(String token) {
        SignedJWT jwt;
        try {
            jwt = SignedJWT.parse(token);
        } catch (java.text.ParseException e) {
            throw new InvalidToken("token is not a well-formed JWS");
        }

        // First. Before the header is used for anything else.
        if (!JWSAlgorithm.RS256.equals(jwt.getHeader().getAlgorithm())) {
            throw new InvalidToken("unexpected signing algorithm");
        }

        String kid = jwt.getHeader().getKeyID();
        if (kid == null || kid.isBlank()) {
            throw new InvalidToken("token does not name a signing key");
        }

        RSAKey key = resolve(kid);

        try {
            if (!jwt.verify(new RSASSAVerifier(key.toRSAPublicKey()))) {
                throw new InvalidToken("signature does not verify");
            }
        } catch (com.nimbusds.jose.JOSEException e) {
            throw new InvalidToken("signature could not be checked");
        }

        try {
            var claims = jwt.getJWTClaimsSet();

            if (!issuer.equals(claims.getIssuer())) {
                throw new InvalidToken("wrong issuer");
            }
            List<String> aud = claims.getAudience();
            if (aud == null || aud.stream().noneMatch(audience::contains)) {
                throw new InvalidToken("wrong audience");
            }
            Date expiry = claims.getExpirationTime();
            if (expiry == null || expiry.toInstant().isBefore(Instant.now())) {
                throw new InvalidToken("token has expired");
            }

            return new Principal(claims.getSubject(),
                    stringList(claims.getClaim("roles")),
                    stringList(claims.getClaim("amr")),
                    (String) claims.getClaim("sid"),
                    claims.getJWTID());
        } catch (java.text.ParseException e) {
            throw new InvalidToken("claims are malformed");
        }
    }

    // -----------------------------------------------------------------------

    private RSAKey resolve(String kid) {
        Snapshot current = cache.get();

        RSAKey known = current.keys().get(kid);
        if (known != null && !current.isStale(cacheTtl)) {
            return known;
        }

        // Either the key set has aged out, or a token arrived naming a key we
        // have never seen — which is exactly what a rotation looks like from
        // here.
        if (current.lastAttempt().plus(MIN_REFETCH_INTERVAL).isAfter(Instant.now())) {
            if (known != null) {
                // Stale but present. Serving it is right: the alternative is
                // rejecting valid tokens because a refetch is rate-limited.
                return known;
            }
            throw new InvalidToken("unknown signing key");
        }

        Snapshot refreshed = fetch(current);
        cache.set(refreshed);

        RSAKey resolved = refreshed.keys().get(kid);
        if (resolved == null) {
            throw new InvalidToken("unknown signing key");
        }
        return resolved;
    }

    private Snapshot fetch(Snapshot current) {
        try {
            String body = http.get().uri(jwksUrl).retrieve().body(String.class);
            if (body == null || body.isBlank()) {
                throw new IllegalStateException("empty JWKS document");
            }

            JWKSet parsed = JWKSet.parse(body);
            var keys = new java.util.HashMap<String, RSAKey>();

            for (var jwk : parsed.getKeys()) {
                if (jwk instanceof RSAKey rsa && rsa.getKeyID() != null) {
                    // Public parts only. A JWKS containing private material is
                    // a catastrophic misconfiguration upstream, and taking it
                    // as a signing key here would hide that.
                    keys.put(rsa.getKeyID(), rsa.toPublicJWK());
                }
            }

            if (keys.isEmpty()) {
                throw new IllegalStateException("JWKS contained no usable RSA keys");
            }

            log.info("refreshed JWKS from {}: {} key(s)", jwksUrl, keys.size());
            return new Snapshot(Map.copyOf(keys), Instant.now(), Instant.now());

        } catch (Exception e) {
            // Keep the old keys and record the attempt. Dropping them because
            // identity-service is redeploying would reject every request in
            // this service until it came back.
            log.warn("could not refresh JWKS from {} ({}); continuing with {} cached key(s)",
                    jwksUrl, e.toString(), current.keys().size());
            return new Snapshot(current.keys(), current.fetchedAt(), Instant.now());
        }
    }

    @SuppressWarnings("unchecked")
    private static List<String> stringList(Object claim) {
        if (claim instanceof List<?> list) {
            return list.stream().map(String::valueOf).toList();
        }
        return List.of();
    }

    /** Exposed for the readiness probe: a service with no keys cannot serve an authenticated request. */
    public boolean hasKeys() {
        return !cache.get().keys().isEmpty();
    }
}
