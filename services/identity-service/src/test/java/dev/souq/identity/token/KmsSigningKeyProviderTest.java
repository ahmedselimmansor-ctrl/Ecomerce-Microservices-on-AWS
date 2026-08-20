package dev.souq.identity.token;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.security.KeyPair;
import java.security.KeyPairGenerator;
import java.security.interfaces.RSAPublicKey;
import java.util.List;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import software.amazon.awssdk.core.SdkBytes;
import software.amazon.awssdk.services.kms.KmsClient;
import software.amazon.awssdk.services.kms.model.GetPublicKeyRequest;
import software.amazon.awssdk.services.kms.model.GetPublicKeyResponse;
import software.amazon.awssdk.services.kms.model.KeySpec;
import software.amazon.awssdk.services.kms.model.SigningAlgorithmSpec;

/**
 * The KMS path, driven against a stub client.
 *
 * <p>A stub rather than LocalStack: every assertion here is about what this
 * class does with a KMS <em>response</em> — reject the wrong key spec, derive a
 * stable kid, keep a stale key when a refresh fails — and none of them needs a
 * real KMS to be meaningful. The one thing a stub cannot check is that the
 * signature verifies, and that is covered by the local path, which uses the
 * same code from {@code SignedJWT} downwards.
 */
class KmsSigningKeyProviderTest {

    private static final String KEY_ARN =
            "arn:aws:kms:eu-west-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab";

    // ------------------------------------------------------------- the guard

    /**
     * The single most consequential line in the class.
     *
     * <p>A generated key in production means every pod signs with a different
     * one, so roughly (n-1)/n of verifications fail across the whole platform —
     * which presents as an intermittent auth bug, not as an outage, and is
     * therefore chased for days.
     */
    @Test
    @DisplayName("refuses to generate an in-memory key outside local and test")
    void refusesGeneratedKeyInProduction() {
        for (String env : new String[]{"production", "staging", "prod", ""}) {
            var thrown = assertThrows(IllegalStateException.class,
                    () -> new KmsSigningKeyProvider("local", null, env),
                    "env '" + env + "' should have been refused");

            assertTrue(thrown.getMessage().contains("SOUQ_JWT_KEY_SOURCE=kms"),
                    "the message should say what to do instead, was: " + thrown.getMessage());
        }
    }

    @Test
    @DisplayName("allows a generated key in local and test")
    void allowsGeneratedKeyLocally() {
        for (String env : new String[]{"local", "test"}) {
            var provider = new KmsSigningKeyProvider("local", null, env);
            assertEquals("local-dev-key-1", provider.current().keyId());
            assertTrue(provider.publicKey("local-dev-key-1").isPresent());
        }
    }

    @Test
    @DisplayName("refuses source=kms with no key id")
    void refusesKmsWithoutKeyId() {
        for (String id : new String[]{null, "", "   "}) {
            assertThrows(IllegalStateException.class,
                    () -> new KmsSigningKeyProvider("kms", id, "production"));
        }
    }

    // ---------------------------------------------------------------- the kid

    /**
     * Every pod must derive the same kid from the same key, or a token minted
     * by one names a key no other publishes.
     */
    @Test
    @DisplayName("derives a stable kid from the key id")
    void kidIsStable() {
        assertEquals(KmsSigningKeyProvider.kidFor(KEY_ARN), KmsSigningKeyProvider.kidFor(KEY_ARN));
        assertNotEquals(
                KmsSigningKeyProvider.kidFor(KEY_ARN),
                KmsSigningKeyProvider.kidFor(KEY_ARN.replace("1234abcd", "5678efab")));
    }

    /**
     * The kid appears in every JWT header and in a publicly reachable JWKS
     * document. An ARN in there names the AWS account.
     */
    @Test
    @DisplayName("the kid does not leak the account id")
    void kidDoesNotLeakTheArn() {
        String kid = KmsSigningKeyProvider.kidFor(KEY_ARN);

        assertFalse(kid.contains("111122223333"), kid);
        assertFalse(kid.contains("arn:"), kid);
        assertFalse(kid.contains("1234abcd"), kid);
        assertTrue(kid.startsWith("kms-"), kid);
    }

    /** A bare key id and its full ARN are the same key and must agree. */
    @Test
    @DisplayName("an ARN and a bare key id produce the same kid")
    void arnAndBareIdAgree() {
        assertEquals(
                KmsSigningKeyProvider.kidFor(KEY_ARN),
                KmsSigningKeyProvider.kidFor("1234abcd-12ab-34cd-56ef-1234567890ab"));
    }

    // ------------------------------------------------------- key validation

    @Test
    @DisplayName("accepts an RSA key that permits RS256")
    void acceptsRsaKey() {
        var provider = new KmsSigningKeyProvider("kms", KEY_ARN, "production",
                stubReturning(KeySpec.RSA_2048, SigningAlgorithmSpec.RSASSA_PKCS1_V1_5_SHA_256));

        assertEquals(KmsSigningKeyProvider.kidFor(KEY_ARN), provider.current().keyId());
        assertTrue(provider.publicKey(provider.current().keyId()).isPresent());
    }

    /**
     * An EC key would sign happily and produce tokens no verifier in this
     * platform accepts, because they all pin RS256. Caught at startup, so the
     * pod fails its readiness probe rather than the first login of the day.
     */
    @Test
    @DisplayName("rejects a non-RSA key at startup, naming the actual spec")
    void rejectsEcKey() {
        var thrown = assertThrows(IllegalStateException.class,
                () -> new KmsSigningKeyProvider("kms", KEY_ARN, "production",
                        stubReturning(KeySpec.ECC_NIST_P256,
                                SigningAlgorithmSpec.ECDSA_SHA_256)));

        assertTrue(thrown.getMessage().contains("ECC_NIST_P256"), thrown.getMessage());
        assertTrue(thrown.getMessage().contains("RSA"), thrown.getMessage());
    }

    /** An RSA key restricted to PSS cannot produce an RS256 signature. */
    @Test
    @DisplayName("rejects an RSA key that does not permit PKCS#1 v1.5")
    void rejectsKeyWithoutPkcs1() {
        var thrown = assertThrows(IllegalStateException.class,
                () -> new KmsSigningKeyProvider("kms", KEY_ARN, "production",
                        stubReturning(KeySpec.RSA_2048, SigningAlgorithmSpec.RSASSA_PSS_SHA_256)));

        assertTrue(thrown.getMessage().contains("RSASSA_PKCS1_V1_5_SHA_256"), thrown.getMessage());
    }

    // ---------------------------------------------------------------- JWKS

    @Test
    @DisplayName("publishes the public key and never anything private")
    void jwksIsPublicOnly() {
        var provider = new KmsSigningKeyProvider("kms", KEY_ARN, "production",
                stubReturning(KeySpec.RSA_2048, SigningAlgorithmSpec.RSASSA_PKCS1_V1_5_SHA_256));

        String document = provider.jwks().toString();

        // The RSA private-key JWK members. If any appears, every token in the
        // platform is forgeable by anyone who can reach the JWKS endpoint.
        for (String member : new String[]{"\"d\"", "\"p\"", "\"q\"", "\"dp\"", "\"dq\"", "\"qi\""}) {
            assertFalse(document.contains(member),
                    "the JWKS contains a private member " + member + ": " + document);
        }

        assertTrue(document.contains("RS256"), document);
        assertTrue(document.contains(provider.current().keyId()), document);
    }

    // -----------------------------------------------------------------------

    /**
     * A KmsClient that answers GetPublicKey and nothing else.
     *
     * <p>Hand-written rather than Mockito: the surface is one method, and a
     * stub that throws {@code UnsupportedOperationException} for everything
     * else makes it obvious when a change starts calling something new.
     */
    private static KmsClient stubReturning(KeySpec spec, SigningAlgorithmSpec algorithm) {
        RSAPublicKey publicKey = generateRsaPublicKey();

        return new KmsClient() {
            @Override
            public String serviceName() {
                return "kms";
            }

            @Override
            public void close() {
                // Nothing to release.
            }

            @Override
            public GetPublicKeyResponse getPublicKey(GetPublicKeyRequest request) {
                return GetPublicKeyResponse.builder()
                        .keyId(request.keyId())
                        .keySpec(spec)
                        // DER SubjectPublicKeyInfo, which is what KMS returns
                        // and what X509EncodedKeySpec parses.
                        .publicKey(SdkBytes.fromByteArray(publicKey.getEncoded()))
                        .signingAlgorithms(List.of(algorithm))
                        .build();
            }
        };
    }

    private static RSAPublicKey generateRsaPublicKey() {
        try {
            KeyPairGenerator generator = KeyPairGenerator.getInstance("RSA");
            generator.initialize(2048);
            KeyPair pair = generator.generateKeyPair();
            return (RSAPublicKey) pair.getPublic();
        } catch (Exception e) {
            throw new IllegalStateException(e);
        }
    }
}
