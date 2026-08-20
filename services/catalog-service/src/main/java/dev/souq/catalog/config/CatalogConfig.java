package dev.souq.catalog.config;

import java.time.Duration;
import java.util.List;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.boot.web.servlet.FilterRegistrationBean;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Configuration;
import org.springframework.core.Ordered;
import org.springframework.web.client.RestClient;

import dev.souq.catalog.api.AccessTokenFilter;
import dev.souq.catalog.catalog.JdbcCategoryRepository;
import dev.souq.catalog.catalog.JdbcProductRepository;
import dev.souq.catalog.catalog.ProductService;
import dev.souq.catalog.event.CatalogEvents;
import dev.souq.catalog.security.JwksVerifier;

/**
 * Explicit wiring.
 *
 * <p>Values are injected here and passed to constructors rather than read with
 * {@code @Value} inside each class. The domain classes stay constructible in a
 * plain unit test with no Spring context, and every security-relevant tunable
 * is visible in one file instead of scattered across the classes it configures.
 */
@Configuration
public class CatalogConfig {

    /**
     * Hosts an admin may reference in a product image.
     *
     * <p>An allow-list, not a format check. Product images are rendered on
     * every storefront page for that product, so an arbitrary origin means a
     * compromised admin account can point every image at a third-party server
     * that sees the IP and referrer of every shopper — and can change what it
     * serves afterwards.
     */
    @Bean
    public List<String> allowedImageHosts(
            @Value("${souq.catalog.image-hosts:cdn.souq.dev,souq-media.s3.amazonaws.com}")
            String hosts) {
        return List.of(hosts.split(","));
    }

    @Bean
    public ProductService productService(JdbcProductRepository products,
                                         JdbcCategoryRepository categories,
                                         CatalogEvents events,
                                         List<String> allowedImageHosts) {
        return new ProductService(products, categories, events, allowedImageHosts);
    }

    /**
     * The client that fetches identity-service's JWKS.
     *
     * <p>Timeouts are short and explicit. The default {@code RestClient} has no
     * read timeout at all, so a JWKS endpoint that accepts a connection and
     * then hangs would block a request thread indefinitely — and since every
     * authenticated request can trigger a refetch, that is the whole pool.
     */
    @Bean
    public RestClient jwksClient() {
        var factory = new org.springframework.http.client.SimpleClientHttpRequestFactory();
        factory.setConnectTimeout(1_000);
        factory.setReadTimeout(2_000);
        return RestClient.builder().requestFactory(factory).build();
    }

    @Bean
    public JwksVerifier jwksVerifier(RestClient jwksClient,
                                     @Value("${souq.jwt.jwks-url}") String jwksUrl,
                                     @Value("${souq.jwt.issuer}") String issuer,
                                     @Value("${souq.jwt.audience}") String audience,
                                     @Value("${souq.jwt.jwks-cache-minutes}") int cacheMinutes) {
        return new JwksVerifier(jwksClient, jwksUrl, issuer,
                List.of(audience.split(",")), Duration.ofMinutes(cacheMinutes));
    }

    @Bean
    public FilterRegistrationBean<AccessTokenFilter> accessTokenFilter(JwksVerifier verifier) {
        var registration = new FilterRegistrationBean<>(new AccessTokenFilter(verifier));
        registration.addUrlPatterns("/v1/*");
        registration.setOrder(Ordered.HIGHEST_PRECEDENCE + 10);
        return registration;
    }
}
