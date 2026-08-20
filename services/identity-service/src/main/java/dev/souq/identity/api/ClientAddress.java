package dev.souq.identity.api;

import jakarta.servlet.http.HttpServletRequest;

/**
 * The caller's IP address, as far as it can be trusted.
 *
 * <p>Behind an ALB, {@code getRemoteAddr()} is the load balancer, so every
 * request appears to come from one address and per-IP lockout would either
 * never fire or lock out the entire internet at once. The real address is in
 * {@code X-Forwarded-For}.
 *
 * <p>The subtlety is <b>which</b> entry to take. The header is a client-supplied
 * list that proxies append to, so a caller can prepend anything it likes:
 *
 * <pre>X-Forwarded-For: 1.2.3.4, 203.0.113.9</pre>
 *
 * <p>Taking the <em>first</em> entry — the common mistake — lets an attacker
 * send a fresh fake address on every request and defeat rate limiting entirely.
 * The <em>last</em> entry is the one our own trusted proxy appended and is the
 * only value in the header that was not chosen by the client.
 */
final class ClientAddress {

    private ClientAddress() {}

    static String of(HttpServletRequest request) {
        String forwarded = request.getHeader("X-Forwarded-For");

        if (forwarded != null && !forwarded.isBlank()) {
            String[] hops = forwarded.split(",");
            String last = hops[hops.length - 1].trim();
            if (isPlausible(last)) {
                return last;
            }
        }

        String remote = request.getRemoteAddr();
        // The column is INET; a value Postgres cannot parse would fail the
        // insert and, since the insert is the lockout audit trail, would let
        // the attempt go uncounted. Unknown addresses become a documented
        // sentinel instead.
        return isPlausible(remote) ? remote : "0.0.0.0";
    }

    private static boolean isPlausible(String candidate) {
        if (candidate == null || candidate.isBlank() || candidate.length() > 45) {
            return false;
        }
        for (int i = 0; i < candidate.length(); i++) {
            char c = candidate.charAt(i);
            boolean allowed = (c >= '0' && c <= '9')
                    || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
                    || c == '.' || c == ':';
            if (!allowed) return false;
        }
        return true;
    }
}
