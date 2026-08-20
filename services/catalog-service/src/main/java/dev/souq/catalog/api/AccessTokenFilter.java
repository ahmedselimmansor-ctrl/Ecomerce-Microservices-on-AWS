package dev.souq.catalog.api;

import java.io.IOException;

import org.springframework.web.filter.OncePerRequestFilter;

import dev.souq.catalog.security.JwksVerifier;
import dev.souq.catalog.security.JwksVerifier.InvalidToken;
import jakarta.servlet.FilterChain;
import jakarta.servlet.ServletException;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletResponse;

/**
 * Attaches the verified principal, and nothing more.
 *
 * <p>It does not decide which paths need authentication. That policy would then
 * live in a pattern list far from the endpoints it protects, and the failure
 * mode is silent — add a controller method, forget the pattern, ship an open
 * write endpoint. Instead each handler asks for what it needs via
 * {@link Caller}, three lines above the code being protected.
 *
 * <p>This matters more here than in identity-service, because most of this
 * service is legitimately public. Browsing does not require a token, so a
 * blanket "authenticated by default" rule would be wrong, and a blanket
 * "public by default" rule is exactly what leaves an admin route exposed.
 */
public class AccessTokenFilter extends OncePerRequestFilter {

    static final String PRINCIPAL_ATTRIBUTE = "souq.principal";

    private final JwksVerifier verifier;

    public AccessTokenFilter(JwksVerifier verifier) {
        this.verifier = verifier;
    }

    @Override
    protected void doFilterInternal(HttpServletRequest request, HttpServletResponse response,
                                    FilterChain chain) throws ServletException, IOException {

        String header = request.getHeader("Authorization");

        if (header != null && header.regionMatches(true, 0, "Bearer ", 0, 7)) {
            try {
                request.setAttribute(PRINCIPAL_ATTRIBUTE, verifier.verify(header.substring(7).trim()));
            } catch (InvalidToken e) {
                logger.debug("rejected an access token: " + e.getMessage());
            }
        }

        // Product responses are cached at CloudFront. Without this, an admin
        // response containing a DRAFT product could be served from the edge to
        // an anonymous shopper.
        response.addHeader("Vary", "Authorization");

        chain.doFilter(request, response);
    }
}
