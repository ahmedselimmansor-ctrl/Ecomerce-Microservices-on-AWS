package dev.souq.catalog.api;

import java.time.Instant;
import java.util.List;
import java.util.Map;

import com.fasterxml.jackson.annotation.JsonAnyGetter;
import com.fasterxml.jackson.annotation.JsonInclude;

/**
 * The RFC 9457 Problem Details envelope from docs/CONTRACTS.md §2.2.
 *
 * <p>Identical in shape to identity-service's, and to the Go, Python, Node and
 * C++ equivalents. That uniformity is the point: the storefront has one error
 * path for eleven services in five languages, and it switches on {@code code}
 * rather than on prose that changes with the next copy edit.
 *
 * <p>Extension members serialise <em>flat</em>, alongside the standard ones, as
 * the RFC specifies — not nested under an "extensions" object no other
 * conforming server produces.
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
        @JsonAnyGetter Map<String, Object> extensions) {

    public record FieldError(String field, String message) {}

    private static final String BASE_URI = "https://errors.souq.dev/catalog/";

    public static Problem of(int status, String code, String title, String detail,
                             String instance, String requestId) {
        return new Problem(BASE_URI + code.toLowerCase().replace('_', '-'), title, status,
                detail, instance, code, requestId, Instant.now().toString(), null, null);
    }

    public Problem withFieldErrors(List<FieldError> fieldErrors) {
        return new Problem(type, title, status, detail, instance, code, requestId,
                timestamp, fieldErrors, extensions);
    }

    public Problem withExtensions(Map<String, Object> extra) {
        return new Problem(type, title, status, detail, instance, code, requestId,
                timestamp, errors, extra);
    }
}
