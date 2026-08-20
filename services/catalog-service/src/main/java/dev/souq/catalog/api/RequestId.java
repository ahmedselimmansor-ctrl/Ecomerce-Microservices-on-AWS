package dev.souq.catalog.api;

import java.util.UUID;

import jakarta.servlet.http.HttpServletRequest;

/**
 * The correlation id for this request.
 *
 * <p>Propagated when the caller supplies one so a single id spans the BFF, the
 * services it fans out to and the Kafka events they emit; generated otherwise,
 * so no log line is ever without one.
 *
 * <p>A caller-supplied value that is not plainly safe is discarded rather than
 * sanitised. This string lands in log lines and in response bodies, so
 * accepting arbitrary text is log injection.
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
        return UUID.randomUUID().toString();
    }

    private static boolean isSafe(int c) {
        return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
                || (c >= '0' && c <= '9') || c == '-' || c == '_';
    }
}
