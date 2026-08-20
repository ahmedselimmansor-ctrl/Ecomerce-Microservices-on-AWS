package dev.souq.identity.api;

import dev.souq.identity.token.AccessTokenVerifier.Principal;
import jakarta.servlet.http.HttpServletRequest;

/**
 * How a handler asks for the caller.
 *
 * <p>{@link #required} throws when there is no verified principal, which the
 * exception handler turns into a 401. Making the check a method call at the top
 * of each protected handler — rather than a path pattern in a config class —
 * means the authorisation decision sits three lines above the code it protects,
 * where a reviewer reading the endpoint cannot miss its absence.
 */
public final class AuthenticatedUser {

    private AuthenticatedUser() {}

    /** Signals "no valid token", which the handler renders as 401 UNAUTHENTICATED. */
    public static class NotAuthenticated extends RuntimeException {
        public NotAuthenticated() { super("this endpoint requires a valid access token"); }
    }

    /** Signals "valid token, insufficient rights", which renders as 403 FORBIDDEN. */
    public static class NotPermitted extends RuntimeException {
        public NotPermitted(String required) {
            super("this endpoint requires " + required);
        }
    }

    public static Principal required(HttpServletRequest request) {
        Object attribute = request.getAttribute(AccessTokenFilter.PRINCIPAL_ATTRIBUTE);
        if (attribute instanceof Principal principal) {
            return principal;
        }
        throw new NotAuthenticated();
    }

    public static Principal optional(HttpServletRequest request) {
        Object attribute = request.getAttribute(AccessTokenFilter.PRINCIPAL_ATTRIBUTE);
        return attribute instanceof Principal principal ? principal : null;
    }

    public static Principal withRole(HttpServletRequest request, String role) {
        Principal principal = required(request);
        if (!principal.hasRole(role)) {
            throw new NotPermitted("the " + role + " role");
        }
        return principal;
    }

    /** docs/CONTRACTS.md §7: ADMIN or OPS <em>and</em> an MFA login. */
    public static Principal admin(HttpServletRequest request) {
        Principal principal = required(request);
        if (!principal.isAdmin()) {
            throw new NotPermitted("an ADMIN or OPS role with multi-factor authentication");
        }
        return principal;
    }
}
