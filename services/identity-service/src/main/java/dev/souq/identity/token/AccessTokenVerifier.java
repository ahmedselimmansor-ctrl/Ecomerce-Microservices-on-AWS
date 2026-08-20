package dev.souq.identity.token;

import java.time.Instant;
import java.util.Date;
import java.util.List;

import com.nimbusds.jose.JWSAlgorithm;
import com.nimbusds.jose.crypto.RSASSAVerifier;
import com.nimbusds.jwt.SignedJWT;

/**
 * Verifies an access token this service issued.
 *
 * <p>Every check below has a failure mode that is a real, published attack:
 *
 * <ul>
 *   <li><b>Algorithm is pinned to RS256 before anything else.</b> The header is
 *       attacker-controlled. A verifier that reads {@code alg} and dispatches on
 *       it accepts {@code alg:"none"} (no signature at all) and {@code alg:"HS256"}
 *       with the RSA <em>public</em> key as the HMAC secret — and that key is
 *       published at the JWKS endpoint. Both are complete auth bypasses.</li>
 *   <li><b>Issuer and audience.</b> Without them a valid token minted for a
 *       different system, by an issuer we happen to also trust, authenticates
 *       here.</li>
 *   <li><b>Expiry with no leeway.</b> The 15-minute TTL is the only bound on how
 *       long a revoked user keeps working. Clock skew allowances quietly extend
 *       it; NTP is a hard requirement on these nodes instead.</li>
 * </ul>
 */
public final class AccessTokenVerifier {

    private final KmsSigningKeyProvider keys;
    private final String issuer;
    private final List<String> audience;

    public AccessTokenVerifier(KmsSigningKeyProvider keys, String issuer, List<String> audience) {
        this.keys = keys;
        this.issuer = issuer;
        this.audience = audience;
    }

    /** What a verified token says. Roles drive authorisation; sid allows revocation lookups. */
    public record Principal(String userId, List<String> roles, List<String> amr,
                            String sessionId, String tokenId) {

        public boolean hasRole(String role) {
            return roles.contains(role);
        }

        /** docs/CONTRACTS.md §7: admin surfaces need a privileged role AND an MFA login. */
        public boolean isAdmin() {
            return (hasRole("ADMIN") || hasRole("OPS")) && amr.contains("mfa");
        }
    }

    public static class InvalidAccessToken extends RuntimeException {
        public InvalidAccessToken(String message) { super(message); }
    }

    public Principal verify(String token) {
        SignedJWT jwt;
        try {
            jwt = SignedJWT.parse(token);
        } catch (java.text.ParseException e) {
            throw new InvalidAccessToken("token is not a well-formed JWS");
        }

        // First, before touching anything the token claims about itself.
        if (!JWSAlgorithm.RS256.equals(jwt.getHeader().getAlgorithm())) {
            throw new InvalidAccessToken("unexpected signing algorithm");
        }

        String kid = jwt.getHeader().getKeyID();
        var publicKey = keys.publicKey(kid)
                .orElseThrow(() -> new InvalidAccessToken("unknown signing key"));

        try {
            if (!jwt.verify(new RSASSAVerifier(publicKey))) {
                throw new InvalidAccessToken("signature does not verify");
            }
        } catch (com.nimbusds.jose.JOSEException e) {
            throw new InvalidAccessToken("signature could not be checked");
        }

        // Only now are the claims worth reading.
        try {
            var claims = jwt.getJWTClaimsSet();

            if (!issuer.equals(claims.getIssuer())) {
                throw new InvalidAccessToken("wrong issuer");
            }
            List<String> aud = claims.getAudience();
            if (aud == null || aud.stream().noneMatch(audience::contains)) {
                throw new InvalidAccessToken("wrong audience");
            }

            Date expiry = claims.getExpirationTime();
            if (expiry == null || expiry.toInstant().isBefore(Instant.now())) {
                throw new InvalidAccessToken("token has expired");
            }

            return new Principal(
                    claims.getSubject(),
                    stringList(claims.getClaim("roles")),
                    stringList(claims.getClaim("amr")),
                    (String) claims.getClaim("sid"),
                    claims.getJWTID());
        } catch (java.text.ParseException e) {
            throw new InvalidAccessToken("claims are malformed");
        }
    }

    @SuppressWarnings("unchecked")
    private static List<String> stringList(Object claim) {
        if (claim instanceof List<?> list) {
            return list.stream().map(String::valueOf).toList();
        }
        return List.of();
    }
}
