package dev.souq.identity.api;

import java.io.IOException;

import org.springframework.web.filter.OncePerRequestFilter;

import dev.souq.identity.token.AccessTokenVerifier;
import dev.souq.identity.token.AccessTokenVerifier.InvalidAccessToken;
import jakarta.servlet.FilterChain;
import jakarta.servlet.ServletException;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletResponse;

/**
 * Attaches the verified principal to the request, and nothing more.
 *
 * <p>It deliberately does <b>not</b> reject unauthenticated requests. A filter
 * that decides which paths need auth holds that policy in a pattern list far
 * away from the endpoints it protects, and the failure mode is silent: add a
 * controller method, forget the pattern, ship an open endpoint. Here a handler
 * asks for the principal via {@link AuthenticatedUser}, and a handler that
 * needs one and does not ask for it does not compile into anything that works.
 *
 * <p>A malformed or expired token is left unauthenticated rather than rejected
 * outright, so endpoints that are legitimately public still serve a browser
 * holding a stale token instead of failing its whole session.
 */
public class AccessTokenFilter extends OncePerRequestFilter {

    static final String PRINCIPAL_ATTRIBUTE = "souq.principal";

    private final AccessTokenVerifier verifier;

    public AccessTokenFilter(AccessTokenVerifier verifier) {
        this.verifier = verifier;
    }

    @Override
    protected void doFilterInternal(HttpServletRequest request, HttpServletResponse response,
                                    FilterChain chain) throws ServletException, IOException {

        String header = request.getHeader("Authorization");

        if (header != null && header.regionMatches(true, 0, "Bearer ", 0, 7)) {
            String token = header.substring(7).trim();
            try {
                request.setAttribute(PRINCIPAL_ATTRIBUTE, verifier.verify(token));
            } catch (InvalidAccessToken e) {
                logger.debug("rejected an access token: " + e.getMessage());
            }
        }

        // The response varies by Authorization header, so a shared cache must
        // not serve one user's /v1/me to another. Set unconditionally: the
        // dangerous case is exactly the one where no token was present and a
        // cache would decide the response is public.
        response.addHeader("Vary", "Authorization");

        chain.doFilter(request, response);
    }
}
