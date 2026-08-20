package dev.souq.identity.api;

import java.time.Instant;
import java.util.List;
import java.util.Map;

import com.fasterxml.jackson.annotation.JsonAnyGetter;
import com.fasterxml.jackson.annotation.JsonInclude;

/**
 * The RFC 9457 Problem Details envelope from docs/CONTRACTS.md §2.2.
 *
 * <p>Every 4xx and 5xx from every service in every language serialises to
 * exactly this shape. The frontend switches on {@code code} — never on
 * {@code detail}, which is prose and changes with the next copy edit.
 *
 * <p>{@code code} values must also exist in {@code ERROR_CODES} in
 * libs/ts-contracts/src/primitives.ts. A code that is missing there renders as
 * a generic failure in the storefront: degraded rather than broken, but still
 * wrong.
 *
 * <p>{@code NON_NULL} matters: RFC 9457 members are optional, and a body full
 * of {@code "errors": null} is both noise and a hint about internal structure.
 */
@JsonInclude(JsonInclude.Include.NON_NULL)
public record Problem(
        String type,
        String title,
        int status,
        String detail,
        String instance,
        String code,
        String requestId,
        String timestamp,
        List<FieldError> errors,
        /**
         * RFC 9457 extension members. Serialised <em>flat</em> at the top level
         * — {@code "retryAfterSeconds": 900}, not
         * {@code "extensions": {"retryAfterSeconds": 900}} — because the spec
         * puts extension members alongside the standard ones, and a client
         * reading a nested object would be reading a shape no other
         * conforming server produces.
         */
        @JsonAnyGetter Map<String, Object> extensions) {

    public record FieldError(String field, String message) {}

    private static final String BASE_URI = "https://errors.souq.dev/identity/";

    public static Problem of(int status, String code, String title, String detail,
                             String instance, String requestId) {
        return new Problem(BASE_URI + slug(code), title, status, detail, instance,
                code, requestId, Instant.now().toString(), null, null);
    }

    public Problem withFieldErrors(List<FieldError> fieldErrors) {
        return new Problem(type, title, status, detail, instance, code, requestId,
                timestamp, fieldErrors, extensions);
    }

    public Problem withExtensions(Map<String, Object> extra) {
        return new Problem(type, title, status, detail, instance, code, requestId,
                timestamp, errors, extra);
    }

    /** {@code REFRESH_TOKEN_REUSED} becomes {@code refresh-token-reused}. */
    private static String slug(String code) {
        return code.toLowerCase().replace('_', '-');
    }
}
