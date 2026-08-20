package dev.souq.catalog.api;

import dev.souq.catalog.security.JwksVerifier.Principal;
import jakarta.servlet.http.HttpServletRequest;

/**
 * How a handler asks who is calling.
 *
 * <p>{@link #canSeeUnpublished} is the one that carries the most weight. Draft
 * and archived products must never reach the storefront, and the decision is
 * made here rather than by trusting a query parameter — an endpoint that
 * honours {@code ?includeDrafts=true} from an unauthenticated caller is a
 * catalogue leak that looks like a feature.
 */
public final class Caller {

    private Caller() {}

    public static class NotAuthenticated extends RuntimeException {
        public NotAuthenticated() { super("this endpoint requires a valid access token"); }
    }

    public static class NotPermitted extends RuntimeException {
        public NotPermitted(String required) { super("this endpoint requires " + required); }
    }

    public static Principal optional(HttpServletRequest request) {
        Object attribute = request.getAttribute(AccessTokenFilter.PRINCIPAL_ATTRIBUTE);
        return attribute instanceof Principal principal ? principal : null;
    }

    public static Principal required(HttpServletRequest request) {
        Principal principal = optional(request);
        if (principal == null) {
            throw new NotAuthenticated();
        }
        return principal;
    }

    /**
     * Catalogue writes.
     *
     * <p>ADMIN or MERCHANT. Unlike the admin dashboard's own routes this does
     * not require MFA in {@code amr}: merchants manage their own listings from
     * ordinary sessions, and requiring a second factor for a price edit would
     * push the work into a shared account.
     */
    public static Principal writer(HttpServletRequest request) {
        Principal principal = required(request);
        if (!principal.hasRole("ADMIN") && !principal.hasRole("MERCHANT")) {
            throw new NotPermitted("the ADMIN or MERCHANT role");
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

    /** True only for a caller entitled to see DRAFT and ARCHIVED products. */
    public static boolean canSeeUnpublished(HttpServletRequest request) {
        Principal principal = optional(request);
        return principal != null
                && (principal.hasRole("ADMIN") || principal.hasRole("MERCHANT")
                    || principal.hasRole("SUPPORT"));
    }
}
