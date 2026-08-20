package dev.souq.identity.api;

import java.util.Map;

import org.springframework.http.CacheControl;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.RestController;

import dev.souq.identity.token.KmsSigningKeyProvider;

/**
 * Publishes the public keys every other service verifies against.
 *
 * <p>This endpoint is the reason ten services can verify a JWT without asking
 * identity-service anything (docs/CONTRACTS.md §7). It is also unauthenticated,
 * which is correct — the document contains public keys and nothing else, and
 * requiring a token to fetch the keys needed to verify tokens does not
 * terminate.
 *
 * <p>The five-minute cache is load-bearing in both directions. Longer, and a
 * rotated-out key keeps being trusted past the point we stopped publishing it.
 * Shorter, and every service in the platform polls this endpoint hard enough
 * that identity-service becomes the synchronous dependency local verification
 * exists to avoid.
 */
@RestController
public class JwksController {

    private final KmsSigningKeyProvider keys;

    public JwksController(KmsSigningKeyProvider keys) {
        this.keys = keys;
    }

    @GetMapping(value = "/v1/.well-known/jwks.json", produces = "application/json")
    public ResponseEntity<Map<String, Object>> jwks() {
        return ResponseEntity.ok()
                .cacheControl(CacheControl.maxAge(java.time.Duration.ofMinutes(5)).cachePublic())
                .body(keys.jwks());
    }

    /**
     * The OIDC discovery document.
     *
     * <p>Not used by our own services, which have the issuer configured. It is
     * here because every standard client library looks for it first, and an
     * integration that cannot discover the JWKS URL tends to get "solved" by
     * someone hardcoding a key.
     */
    @GetMapping(value = "/v1/.well-known/openid-configuration", produces = "application/json")
    public ResponseEntity<Map<String, Object>> discovery(
            @org.springframework.beans.factory.annotation.Value("${souq.jwt.issuer}") String issuer) {

        return ResponseEntity.ok()
                .cacheControl(CacheControl.maxAge(java.time.Duration.ofHours(1)).cachePublic())
                .body(Map.of(
                        "issuer", issuer,
                        "jwks_uri", issuer + "/v1/.well-known/jwks.json",
                        "token_endpoint", issuer + "/v1/auth/login",
                        "id_token_signing_alg_values_supported", java.util.List.of("RS256"),
                        "grant_types_supported", java.util.List.of("password", "refresh_token"),
                        "response_types_supported", java.util.List.of("token"),
                        "subject_types_supported", java.util.List.of("public"),
                        "claims_supported",
                        java.util.List.of("sub", "iss", "aud", "exp", "iat", "jti",
                                "roles", "scope", "sid", "amr", "ver")));
    }
}
