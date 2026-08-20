package dev.souq.identity.token;

import java.security.KeyFactory;
import java.security.KeyPair;
import java.security.KeyPairGenerator;
import java.security.interfaces.RSAPrivateKey;
import java.security.interfaces.RSAPublicKey;
import java.security.spec.X509EncodedKeySpec;
import java.time.Duration;
import java.time.Instant;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.concurrent.atomic.AtomicReference;

import com.nimbusds.jose.JWSAlgorithm;
import com.nimbusds.jose.JWSSigner;
import com.nimbusds.jose.crypto.RSASSASigner;
import com.nimbusds.jose.jwk.JWKSet;
import com.nimbusds.jose.jwk.KeyUse;
import com.nimbusds.jose.jwk.RSAKey;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import dev.souq.identity.token.TokenService.SigningKeyProvider;
import software.amazon.awssdk.services.kms.KmsClient;
import software.amazon.awssdk.services.kms.model.GetPublicKeyRequest;
import software.amazon.awssdk.services.kms.model.KeySpec;

/**
 * Supplies the JWT signing key and publishes the JWKS.
 *
 * <p>Two sources, and the distinction is enforced rather than documented:
 *
 * <ul>
 *   <li><b>local</b> — generates an RSA pair in memory at startup. Convenient,
 *       and catastrophic in production: every pod would sign with a different
 *       key, so roughly {@code (n-1)/n} of all verifications across the
 *       platform would fail — an outage that presents as an intermittent auth
 *       bug rather than as an outage. The constructor <em>refuses</em> to
 *       generate outside local/test.</li>
 *   <li><b>kms</b> — the private key never leaves AWS KMS. Signing is an API
 *       call; the key material cannot be exfiltrated even with full control of
 *       the pod, so recovering from a compromise is revoking an IAM role rather
 *       than rotating a secret somebody already has.</li>
 * </ul>
 *
 * <p>The interface deals in {@link JWSSigner}, not in a private key, and that
 * is what makes the KMS path expressible at all. An interface returning
 * {@code RSAPrivateKey} can only ever describe a key this process holds.
 *
 * <p><b>What KMS costs.</b> Every token issue becomes a network round trip —
 * roughly 10&ndash;20&nbsp;ms and a metered API call. That is acceptable
 * precisely because it is on <em>issue</em> and never on <em>verify</em>: every
 * other service checks signatures locally against the cached JWKS, so the KMS
 * call happens once per login and once per refresh (at most every 15 minutes
 * per active session), not once per request.
 *
 * <p>Rotation is why {@link #jwks()} publishes more than one key. A verifier
 * caches the JWKS for five minutes, so a new key must be published
 * <em>before</em> it is used to sign, and the old key must stay published until
 * every token it signed has expired. Removing the old key at rotation time
 * invalidates every live session.
 */
public class KmsSigningKeyProvider implements SigningKeyProvider {

    private static final Logger log = LoggerFactory.getLogger(KmsSigningKeyProvider.class);

    /**
     * How long a fetched KMS public key is trusted before being re-read.
     *
     * <p>Long, deliberately. An asymmetric KMS key's public half does not change
     * — AWS does not rotate asymmetric keys in place — so this only bounds how
     * stale the cache can be after a manual key replacement. Making it short
     * would put a KMS call in front of every JWKS request from every pod of
     * every service.
     */
    private static final Duration PUBLIC_KEY_TTL = Duration.ofHours(12);

    private final Key current;
    private final KmsClient kms;
    private final String kmsKeyId;

    /** Present immediately for the local path; fetched lazily for KMS. */
    private final AtomicReference<CachedPublicKey> publicKey = new AtomicReference<>(null);

    /** Retired keys still published so tokens they signed keep verifying. */
    private final Map<String, RSAPublicKey> retired;

    private record CachedPublicKey(RSAPublicKey key, Instant fetchedAt) {
        boolean isStale() {
            return fetchedAt.plus(PUBLIC_KEY_TTL).isBefore(Instant.now());
        }
    }

    public KmsSigningKeyProvider(String source, String kmsKeyId, String env) {
        this(source, kmsKeyId, env, null);
    }

    /**
     * @param kms injected so a test can drive the KMS path against a stub. Null
     *            means "build the default client", which is what production does
     *            — the SDK picks up region and credentials from the IRSA-provided
     *            web identity token.
     */
    public KmsSigningKeyProvider(String source, String kmsKeyId, String env, KmsClient kms) {
        if ("kms".equals(source)) {
            if (kmsKeyId == null || kmsKeyId.isBlank()) {
                throw new IllegalStateException(
                        "SOUQ_JWT_KEY_SOURCE=kms requires SOUQ_JWT_KMS_KEY_ID");
            }

            this.kms = kms != null ? kms : KmsClient.create();
            this.kmsKeyId = kmsKeyId;
            this.retired = Map.of();

            // Read the public key at startup rather than on first use. A
            // misconfigured key id, a missing kms:GetPublicKey permission or an
            // EC key where RSA was expected should all fail the pod's readiness
            // probe immediately — not the first login of the day.
            RSAPublicKey fetched = fetchPublicKey();
            this.publicKey.set(new CachedPublicKey(fetched, Instant.now()));

            // The kid is derived from the key id, so it is stable across pods
            // and across restarts. A random kid per process would mean a token
            // minted by one pod names a key no other pod publishes.
            this.current = new Key(kidFor(kmsKeyId), new KmsJwsSigner(this.kms, kmsKeyId));

            log.info("signing with AWS KMS key {} (kid={})", kmsKeyId, this.current.keyId());
            return;
        }

        if (!"local".equals(env) && !"test".equals(env)) {
            // The guard that matters. See the class comment.
            throw new IllegalStateException(
                    "A generated in-memory signing key cannot be used in the '" + env
                            + "' environment. Every pod would sign with a different key. "
                            + "Set SOUQ_JWT_KEY_SOURCE=kms and SOUQ_JWT_KMS_KEY_ID.");
        }

        this.kms = null;
        this.kmsKeyId = null;
        this.retired = Map.of();

        try {
            KeyPairGenerator generator = KeyPairGenerator.getInstance("RSA");
            generator.initialize(2048);
            KeyPair pair = generator.generateKeyPair();

            this.current = new Key("local-dev-key-1",
                    new RSASSASigner((RSAPrivateKey) pair.getPrivate()));
            this.publicKey.set(new CachedPublicKey((RSAPublicKey) pair.getPublic(), Instant.now()));

            log.warn("using a GENERATED in-memory signing key — local development only. "
                    + "Tokens will not verify across a restart.");
        } catch (Exception e) {
            throw new IllegalStateException("could not generate a signing key", e);
        }
    }

    @Override
    public Key current() {
        return current;
    }

    /**
     * The public key for a {@code kid}, including retired ones.
     *
     * <p>This service verifies its own tokens the same way every other service
     * does — by key id against the published set — rather than reaching for the
     * current key directly. Special-casing it would mean the one service that
     * mints tokens is also the one service not exercising the verification
     * path, so a rotation bug would surface everywhere else first.
     */
    public Optional<RSAPublicKey> publicKey(String keyId) {
        if (current.keyId().equals(keyId)) {
            return Optional.of(currentPublicKey());
        }
        return Optional.ofNullable(retired.get(keyId));
    }

    /**
     * The JWKS every other service fetches and caches.
     *
     * <p>Public keys only. If a private key ever appeared in this document every
     * token in the platform would be forgeable, so the construction below builds
     * from {@link RSAPublicKey} exclusively — on the KMS path there is no
     * private key in this process to leak even by accident.
     */
    public Map<String, Object> jwks() {
        var keys = new java.util.ArrayList<RSAKey>();

        keys.add(new RSAKey.Builder(currentPublicKey())
                .keyID(current.keyId())
                .keyUse(KeyUse.SIGNATURE)
                .algorithm(JWSAlgorithm.RS256)
                .build());

        retired.forEach((kid, pub) -> keys.add(new RSAKey.Builder(pub)
                .keyID(kid)
                .keyUse(KeyUse.SIGNATURE)
                .algorithm(JWSAlgorithm.RS256)
                .build()));

        return new JWKSet(List.copyOf(keys)).toJSONObject();
    }

    // -----------------------------------------------------------------------

    private RSAPublicKey currentPublicKey() {
        CachedPublicKey cached = publicKey.get();

        if (cached != null && (kms == null || !cached.isStale())) {
            return cached.key();
        }

        try {
            RSAPublicKey fetched = fetchPublicKey();
            publicKey.set(new CachedPublicKey(fetched, Instant.now()));
            return fetched;
        } catch (RuntimeException e) {
            if (cached != null) {
                // Serve the stale key rather than failing. It has not changed —
                // AWS does not rotate an asymmetric key in place — and refusing
                // to publish a JWKS because KMS was briefly unreachable would
                // break verification across every service in the platform.
                log.warn("could not refresh the KMS public key ({}); serving the cached one",
                        e.toString());
                return cached.key();
            }
            throw e;
        }
    }

    private RSAPublicKey fetchPublicKey() {
        var response = kms.getPublicKey(GetPublicKeyRequest.builder().keyId(kmsKeyId).build());

        // A signing key that is not RSA_2048 would silently produce signatures
        // no verifier in this platform accepts, because they all pin RS256.
        // Caught here, at startup, with a message that names the actual spec.
        if (response.keySpec() != KeySpec.RSA_2048
                && response.keySpec() != KeySpec.RSA_3072
                && response.keySpec() != KeySpec.RSA_4096) {
            throw new IllegalStateException(
                    "KMS key " + kmsKeyId + " has spec " + response.keySpec()
                            + "; RS256 requires an RSA key");
        }

        if (!response.signingAlgorithms().contains(
                software.amazon.awssdk.services.kms.model.SigningAlgorithmSpec
                        .RSASSA_PKCS1_V1_5_SHA_256)) {
            throw new IllegalStateException(
                    "KMS key " + kmsKeyId + " does not permit RSASSA_PKCS1_V1_5_SHA_256, "
                            + "which is what RS256 requires");
        }

        try {
            // KMS returns DER-encoded SubjectPublicKeyInfo, which is exactly
            // what X509EncodedKeySpec parses.
            var spec = new X509EncodedKeySpec(response.publicKey().asByteArray());
            return (RSAPublicKey) KeyFactory.getInstance("RSA").generatePublic(spec);
        } catch (Exception e) {
            throw new IllegalStateException("could not parse the KMS public key", e);
        }
    }

    /**
     * A stable, non-secret key id.
     *
     * <p>Derived from the KMS key id so every pod agrees without coordinating,
     * and truncated so a full ARN — which names the account — does not end up in
     * every JWT header and in a publicly reachable JWKS document.
     */
    static String kidFor(String kmsKeyId) {
        String tail = kmsKeyId.contains("/")
                ? kmsKeyId.substring(kmsKeyId.lastIndexOf('/') + 1)
                : kmsKeyId;

        try {
            byte[] digest = java.security.MessageDigest.getInstance("SHA-256")
                    .digest(tail.getBytes(java.nio.charset.StandardCharsets.UTF_8));
            return "kms-" + java.util.HexFormat.of().formatHex(digest).substring(0, 16);
        } catch (Exception e) {
            throw new IllegalStateException("SHA-256 unavailable", e);
        }
    }
}
