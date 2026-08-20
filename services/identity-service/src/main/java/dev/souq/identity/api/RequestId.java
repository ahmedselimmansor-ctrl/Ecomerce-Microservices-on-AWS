package dev.souq.identity.api;

import java.util.UUID;

import jakarta.servlet.http.HttpServletRequest;

/**
 * The correlation id for this request.
 *
 * <p>Propagated from the caller when present so that one id spans the BFF, the
 * eleven services and the Kafka events they emit. Generated when absent, so
 * there is never a log line without one — an error a user reports by request id
 * is an error someone can actually find.
 */
final class RequestId {

    static final String HEADER = "X-Request-Id";
    private static final int MAX_LENGTH = 64;

    private RequestId() {}

    static String of(HttpServletRequest request) {
        String supplied = request == null ? null : request.getHeader(HEADER);

        if (supplied != null && !supplied.isBlank() && supplied.length() <= MAX_LENGTH
                && supplied.chars().allMatch(RequestId::isSafe)) {
            return supplied;
        }
        // Anything else is discarded rather than sanitised. The header lands in
        // log lines and response bodies, so accepting arbitrary caller-supplied
        // text is log injection.
        return UUID.randomUUID().toString();
    }

    private static boolean isSafe(int c) {
        return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
                || (c >= '0' && c <= '9') || c == '-' || c == '_';
    }
}
