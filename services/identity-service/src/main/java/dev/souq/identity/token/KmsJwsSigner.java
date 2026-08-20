package dev.souq.identity.token;

import java.security.MessageDigest;
import java.util.Set;

import com.nimbusds.jose.JOSEException;
import com.nimbusds.jose.JWSAlgorithm;
import com.nimbusds.jose.JWSHeader;
import com.nimbusds.jose.JWSSigner;
import com.nimbusds.jose.jca.JCAContext;
import com.nimbusds.jose.util.Base64URL;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import software.amazon.awssdk.core.SdkBytes;
import software.amazon.awssdk.services.kms.KmsClient;
import software.amazon.awssdk.services.kms.model.KmsException;
import software.amazon.awssdk.services.kms.model.MessageType;
import software.amazon.awssdk.services.kms.model.SignRequest;
import software.amazon.awssdk.services.kms.model.SigningAlgorithmSpec;

/**
 * Signs a JWS by calling AWS KMS.
 *
 * <p>The private key never exists outside KMS. That is the entire point: an
 * attacker with full control of this pod — a container escape, a leaked
 * kubeconfig, a heap dump — can ask KMS to sign things while the pod lives, but
 * cannot walk away with the key and forge tokens forever afterwards. Recovery
 * from a compromise is revoking an IAM role, not rotating a secret that is
 * already in someone else's hands.
 *
 * <p>Three details are the difference between this working and this being
 * subtly wrong.
 *
 * <p><b>The digest is computed locally and sent as {@code MessageType.DIGEST}.</b>
 * KMS accepts a raw message, but caps it at 4096 bytes — and a JWT signing input
 * grows with the roles and scope claims, so "it fits today" is not a property
 * worth relying on. Hashing here also sends 32 bytes over the wire instead of
 * the whole token.
 *
 * <p><b>The algorithm is checked against the header rather than assumed.</b>
 * {@code RSASSA_PKCS1_V1_5_SHA_256} is RS256 and nothing else. Signing an
 * {@code RS512} header with a SHA-256 signature produces a token that every
 * verifier rejects, at runtime, in production.
 *
 * <p><b>KMS returns a raw PKCS#1 v1.5 signature, which is already what JWS
 * wants for RSASSA-PKCS1.</b> No unwrapping is needed here — unlike ECDSA,
 * where KMS returns DER and JWS wants the concatenated form. Choosing RSA for
 * the signing key avoids that conversion entirely.
 */
public final class KmsJwsSigner implements JWSSigner {

    private static final Logger log = LoggerFactory.getLogger(KmsJwsSigner.class);

    private final KmsClient kms;
    private final String keyId;
    private final JCAContext jcaContext = new JCAContext();

    public KmsJwsSigner(KmsClient kms, String keyId) {
        this.kms = kms;
        this.keyId = keyId;
    }

    @Override
    public Set<JWSAlgorithm> supportedJWSAlgorithms() {
        // Exactly one. docs/CONTRACTS.md §7 pins RS256, every verifier in the
        // platform pins RS256, and a signer that advertises more than it is
        // configured for is a signer that will one day be asked for the rest.
        return Set.of(JWSAlgorithm.RS256);
    }

    @Override
    public JCAContext getJCAContext() {
        return jcaContext;
    }

    @Override
    public Base64URL sign(JWSHeader header, byte[] signingInput) throws JOSEException {
        if (!JWSAlgorithm.RS256.equals(header.getAlgorithm())) {
            throw new JOSEException(
                    "this signer only produces RS256, was asked for " + header.getAlgorithm());
        }

        byte[] digest;
        try {
            digest = MessageDigest.getInstance("SHA-256").digest(signingInput);
        } catch (Exception e) {
            throw new JOSEException("SHA-256 is unavailable", e);
        }

        try {
            var response = kms.sign(SignRequest.builder()
                    .keyId(keyId)
                    .message(SdkBytes.fromByteArray(digest))
                    .messageType(MessageType.DIGEST)
                    .signingAlgorithm(SigningAlgorithmSpec.RSASSA_PKCS1_V1_5_SHA_256)
                    .build());

            return Base64URL.encode(response.signature().asByteArray());

        } catch (KmsException e) {
            // Deliberately not swallowed into a generic failure. A throttle, a
            // disabled key and a missing permission need three different
            // responses from whoever is on call, and the KMS error code is the
            // only thing that distinguishes them.
            log.error("KMS refused to sign with {}: {} ({})",
                    keyId, e.awsErrorDetails().errorCode(), e.awsErrorDetails().errorMessage());
            throw new JOSEException("KMS signing failed: " + e.awsErrorDetails().errorCode(), e);
        }
    }
}
