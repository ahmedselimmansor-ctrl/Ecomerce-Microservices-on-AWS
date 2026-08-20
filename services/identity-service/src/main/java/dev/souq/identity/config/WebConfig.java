package dev.souq.identity.config;

import java.util.List;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.boot.web.servlet.FilterRegistrationBean;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Configuration;
import org.springframework.core.Ordered;

import dev.souq.identity.api.AccessTokenFilter;
import dev.souq.identity.token.AccessTokenVerifier;
import dev.souq.identity.token.KmsSigningKeyProvider;

/**
 * HTTP-layer wiring.
 *
 * <p>No Spring Security. This service has exactly one authentication mechanism
 * — a bearer JWT it issued itself — and the filter that reads it is forty
 * lines. Adding the framework would bring a filter chain, a
 * {@code SecurityContextHolder} and a URL-pattern authorisation model whose
 * behaviour is harder to read than the thing it replaces, for a service where
 * every authorisation decision is a single role check written next to the
 * endpoint that needs it.
 */
@Configuration
public class WebConfig {

    @Bean
    public AccessTokenVerifier accessTokenVerifier(
            KmsSigningKeyProvider keys,
            @Value("${souq.jwt.issuer}") String issuer,
            @Value("${souq.jwt.audience}") String audience) {
        return new AccessTokenVerifier(keys, issuer, List.of(audience.split(",")));
    }

    @Bean
    public FilterRegistrationBean<AccessTokenFilter> accessTokenFilter(AccessTokenVerifier verifier) {
        var registration = new FilterRegistrationBean<>(new AccessTokenFilter(verifier));
        registration.addUrlPatterns("/v1/*");
        // Ahead of everything else so the principal is attached before any
        // filter that might want to log who the caller is.
        registration.setOrder(Ordered.HIGHEST_PRECEDENCE + 10);
        return registration;
    }
}
