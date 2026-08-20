package dev.souq.identity.config;

import java.time.Duration;
import java.util.List;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Configuration;
import org.springframework.jdbc.core.namedparam.NamedParameterJdbcTemplate;

import dev.souq.identity.auth.AuthService;
import dev.souq.identity.auth.PasswordService;
import dev.souq.identity.auth.ProfileService;
import dev.souq.identity.mfa.MfaService;
import dev.souq.identity.token.JdbcRefreshTokenRepository;
import dev.souq.identity.token.KmsSigningKeyProvider;
import dev.souq.identity.token.TokenService;
import dev.souq.identity.user.JdbcUserRepository;

/**
 * Explicit bean wiring.
 *
 * <p>Constructor injection with plain values rather than {@code @Value} inside
 * each class. Two reasons: the domain classes stay constructible in a unit test
 * with no Spring context at all, and every tunable that affects security is
 * visible in one file instead of scattered across the ones it configures.
 */
@Configuration
public class IdentityConfig {

    @Bean
    public PasswordService passwordService(
            @Value("${souq.security.breach-check-enabled:true}") boolean breachCheck) {
        return new PasswordService(breachCheck);
    }

    @Bean
    public KmsSigningKeyProvider signingKeyProvider(
            @Value("${souq.jwt.signing-key-source}") String source,
            @Value("${souq.jwt.kms-key-id:}") String kmsKeyId,
            @Value("${SOUQ_ENV:local}") String env) {
        return new KmsSigningKeyProvider(source, kmsKeyId, env);
    }

    @Bean
    public JdbcRefreshTokenRepository refreshTokenRepository(NamedParameterJdbcTemplate jdbc) {
        return new JdbcRefreshTokenRepository(jdbc);
    }

    /**
     * The TTLs are constants inside {@link TokenService}, not configuration.
     *
     * <p>docs/CONTRACTS.md §7 fixes them at 15 minutes and 30 days, and every
     * other service caches JWKS and verifies locally on that basis. An operator
     * who could raise the access TTL by setting an environment variable would
     * be silently widening the revocation window across the whole platform, in
     * one service, with nothing to review.
     */
    @Bean
    public TokenService tokenService(KmsSigningKeyProvider keys,
                                     JdbcRefreshTokenRepository repository,
                                     @Value("${souq.jwt.issuer}") String issuer,
                                     @Value("${souq.jwt.audience}") String audience) {
        return new TokenService(keys, repository, issuer, List.of(audience.split(",")));
    }

    @Bean
    public ProfileService profileService(NamedParameterJdbcTemplate jdbc,
                                         JdbcUserRepository users,
                                         PasswordService passwords,
                                         TokenService tokens) {
        return new ProfileService(jdbc, users, passwords, tokens);
    }

    @Bean
    public MfaService mfaService(NamedParameterJdbcTemplate jdbc,
                                 TokenService tokens,
                                 @Value("${souq.mfa.issuer-label:SOUQ}") String issuerLabel) {
        // The issuer label is what an authenticator app shows next to the code.
        // "SOUQ" rather than a hostname: a user with entries for three
        // environments needs to tell them apart, and the label is the only
        // thing they will see.
        return new MfaService(jdbc, tokens, issuerLabel);
    }

    @Bean
    public AuthService authService(NamedParameterJdbcTemplate jdbc,
                                   PasswordService passwords,
                                   TokenService tokens,
                                   @Value("${souq.security.max-failed-logins-per-account}") int perAccount,
                                   @Value("${souq.security.max-failed-logins-per-ip}") int perIp,
                                   @Value("${souq.security.lockout-window-minutes}") int windowMinutes) {
        return new AuthService(jdbc, passwords, tokens, perAccount, perIp,
                Duration.ofMinutes(windowMinutes));
    }
}
