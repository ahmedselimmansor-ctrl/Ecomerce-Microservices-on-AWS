package dev.souq.catalog.api;

import java.util.List;
import java.util.Map;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.dao.DataIntegrityViolationException;
import org.springframework.http.HttpHeaders;
import org.springframework.http.HttpStatus;
import org.springframework.http.MediaType;
import org.springframework.http.ResponseEntity;
import org.springframework.http.converter.HttpMessageNotReadableException;
import org.springframework.web.HttpRequestMethodNotSupportedException;
import org.springframework.web.bind.MethodArgumentNotValidException;
import org.springframework.web.bind.MissingServletRequestParameterException;
import org.springframework.web.bind.annotation.ExceptionHandler;
import org.springframework.web.bind.annotation.RestControllerAdvice;
import org.springframework.web.method.annotation.MethodArgumentTypeMismatchException;
import org.springframework.web.servlet.NoHandlerFoundException;

import dev.souq.catalog.catalog.JdbcCategoryRepository;
import dev.souq.catalog.catalog.JdbcProductRepository;
import dev.souq.catalog.catalog.ProductService;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.validation.ConstraintViolationException;

/**
 * Turns every exception into the RFC 9457 envelope from docs/CONTRACTS.md §2.2.
 *
 * <p>Same two rules as identity-service. Nothing internal reaches the client —
 * the terminal handler returns fixed text plus a request id, because a stack
 * trace or a JDBC message names the framework, the driver and usually the
 * query. And 4xx are logged at INFO without a trace while 5xx are logged at
 * ERROR with one, because a client sending bad input is not an operational
 * event and a dashboard full of those is one where a real 500 goes unnoticed.
 */
@RestControllerAdvice
public class GlobalExceptionHandler {

    private static final Logger log = LoggerFactory.getLogger(GlobalExceptionHandler.class);

    // ----------------------------------------------------------- 401 / 403

    @ExceptionHandler(Caller.NotAuthenticated.class)
    public ResponseEntity<Problem> onNotAuthenticated(Caller.NotAuthenticated e,
                                                      HttpServletRequest req) {
        return ResponseEntity.status(HttpStatus.UNAUTHORIZED)
                .header(HttpHeaders.WWW_AUTHENTICATE, "Bearer")
                .contentType(MediaType.APPLICATION_PROBLEM_JSON)
                .body(Problem.of(401, "UNAUTHENTICATED", "Not signed in", e.getMessage(),
                        req.getRequestURI(), RequestId.of(req)));
    }

    @ExceptionHandler(Caller.NotPermitted.class)
    public ResponseEntity<Problem> onNotPermitted(Caller.NotPermitted e, HttpServletRequest req) {
        return respond(HttpStatus.FORBIDDEN, "FORBIDDEN", "Not permitted", e.getMessage(), req);
    }

    // ----------------------------------------------------------------- 404

    @ExceptionHandler(ProductService.NotFound.class)
    public ResponseEntity<Problem> onNotFound(ProductService.NotFound e, HttpServletRequest req) {
        // PRODUCT_NOT_FOUND for both products and categories: it is the code the
        // storefront already handles, and a category miss reaches the user as
        // the same "we could not find that" page.
        return respond(HttpStatus.NOT_FOUND, "PRODUCT_NOT_FOUND", "Not found",
                e.getMessage(), req);
    }

    // ----------------------------------------------------------------- 409

    /**
     * The row moved under the caller.
     *
     * <p>Carries the presented version as an extension member so the admin UI
     * can say "someone else saved this while you were editing" and offer to
     * reload, rather than showing a bare conflict the user can only respond to
     * by retrying blindly.
     */
    @ExceptionHandler(JdbcProductRepository.StaleVersion.class)
    public ResponseEntity<Problem> onStaleVersion(JdbcProductRepository.StaleVersion e,
                                                  HttpServletRequest req) {
        var problem = Problem.of(409, "CART_STALE", "This product has changed",
                        "Someone else saved a change while you were editing. "
                                + "Reload to see the current version.",
                        req.getRequestURI(), RequestId.of(req))
                .withExtensions(Map.of("reason", "STALE_VERSION"));

        return problemResponse(HttpStatus.CONFLICT, problem);
    }

    @ExceptionHandler(JdbcCategoryRepository.CycleWouldBeCreated.class)
    public ResponseEntity<Problem> onCycle(JdbcCategoryRepository.CycleWouldBeCreated e,
                                           HttpServletRequest req) {
        return respond(HttpStatus.CONFLICT, "VALIDATION_FAILED", "Invalid move",
                "A category cannot be moved beneath one of its own descendants.", req);
    }

    // ------------------------------------------------------------ 400 / 422

    @ExceptionHandler(ProductService.Invalid.class)
    public ResponseEntity<Problem> onInvalid(ProductService.Invalid e, HttpServletRequest req) {
        var problem = Problem.of(422, "VALIDATION_FAILED", "Request rejected",
                        "The catalogue rejected this change.", req.getRequestURI(),
                        RequestId.of(req))
                .withFieldErrors(e.problems().stream()
                        .limit(20)
                        .map(p -> new Problem.FieldError("", p))
                        .toList());

        return problemResponse(HttpStatus.UNPROCESSABLE_ENTITY, problem);
    }

    @ExceptionHandler(MethodArgumentNotValidException.class)
    public ResponseEntity<Problem> onInvalidBody(MethodArgumentNotValidException e,
                                                 HttpServletRequest req) {
        List<Problem.FieldError> fields = e.getBindingResult().getFieldErrors().stream()
                .map(f -> new Problem.FieldError(f.getField(),
                        f.getDefaultMessage() == null ? "is invalid" : f.getDefaultMessage()))
                // Bounded: a request with a thousand-element variants array must
                // not be able to make us build a thousand-entry response.
                .limit(20)
                .toList();

        return problemResponse(HttpStatus.BAD_REQUEST,
                Problem.of(400, "VALIDATION_FAILED", "Request validation failed",
                                "One or more fields are invalid.", req.getRequestURI(),
                                RequestId.of(req))
                        .withFieldErrors(fields));
    }

    @ExceptionHandler(ConstraintViolationException.class)
    public ResponseEntity<Problem> onConstraintViolation(ConstraintViolationException e,
                                                         HttpServletRequest req) {
        List<Problem.FieldError> fields = e.getConstraintViolations().stream()
                .map(v -> new Problem.FieldError(v.getPropertyPath().toString(), v.getMessage()))
                .limit(20)
                .toList();

        return problemResponse(HttpStatus.BAD_REQUEST,
                Problem.of(400, "VALIDATION_FAILED", "Request validation failed",
                                "One or more parameters are invalid.", req.getRequestURI(),
                                RequestId.of(req))
                        .withFieldErrors(fields));
    }

    @ExceptionHandler(HttpMessageNotReadableException.class)
    public ResponseEntity<Problem> onUnreadableBody(HttpMessageNotReadableException e,
                                                    HttpServletRequest req) {
        return respond(HttpStatus.BAD_REQUEST, "VALIDATION_FAILED", "Malformed request body",
                "The request body is not valid JSON, or a required field is missing.", req);
    }

    @ExceptionHandler(MethodArgumentTypeMismatchException.class)
    public ResponseEntity<Problem> onTypeMismatch(MethodArgumentTypeMismatchException e,
                                                  HttpServletRequest req) {
        // The name and the expected type, not the value. `?status=<script>`
        // echoed back is reflected XSS in any client that renders detail as HTML.
        return respond(HttpStatus.BAD_REQUEST, "VALIDATION_FAILED", "Invalid parameter",
                "Parameter '%s' is not in the expected format.".formatted(e.getName()), req);
    }

    @ExceptionHandler(IllegalArgumentException.class)
    public ResponseEntity<Problem> onIllegalArgument(IllegalArgumentException e,
                                                     HttpServletRequest req) {
        // Money's constructor and Status.valueOf both throw this. They are
        // caller errors, not faults, so 400 rather than the terminal 500.
        return respond(HttpStatus.BAD_REQUEST, "VALIDATION_FAILED", "Invalid value",
                e.getMessage(), req);
    }

    @ExceptionHandler(MissingServletRequestParameterException.class)
    public ResponseEntity<Problem> onMissingParam(MissingServletRequestParameterException e,
                                                  HttpServletRequest req) {
        return respond(HttpStatus.BAD_REQUEST, "VALIDATION_FAILED", "Missing parameter",
                "Required parameter '%s' is missing.".formatted(e.getParameterName()), req);
    }

    @ExceptionHandler(NoHandlerFoundException.class)
    public ResponseEntity<Problem> onNoHandler(NoHandlerFoundException e, HttpServletRequest req) {
        return respond(HttpStatus.NOT_FOUND, "VALIDATION_FAILED", "No such endpoint",
                "%s %s is not an endpoint on this service.".formatted(
                        e.getHttpMethod(), e.getRequestURL()), req);
    }

    @ExceptionHandler(HttpRequestMethodNotSupportedException.class)
    public ResponseEntity<Problem> onWrongMethod(HttpRequestMethodNotSupportedException e,
                                                 HttpServletRequest req) {
        return respond(HttpStatus.METHOD_NOT_ALLOWED, "VALIDATION_FAILED", "Method not allowed",
                "%s is not supported on this endpoint.".formatted(e.getMethod()), req);
    }

    // ----------------------------------------------------------------- 5xx

    @ExceptionHandler(DataIntegrityViolationException.class)
    public ResponseEntity<Problem> onIntegrityViolation(DataIntegrityViolationException e,
                                                        HttpServletRequest req) {
        String requestId = RequestId.of(req);

        // Reaching here means a CHECK or unique constraint caught something the
        // application should have. That is a bug in the calling code, and the
        // database's message names tables and columns, so it is logged not sent.
        log.error("database constraint violated (requestId={})", requestId, e);

        return problemResponse(HttpStatus.CONFLICT,
                Problem.of(409, "VALIDATION_FAILED", "Conflict",
                        "The change conflicts with a catalogue rule. "
                                + "Check prices, barcodes and slugs for duplicates.",
                        req.getRequestURI(), requestId));
    }

    @ExceptionHandler(Exception.class)
    public ResponseEntity<Problem> onUnexpected(Exception e, HttpServletRequest req) {
        String requestId = RequestId.of(req);
        log.error("unhandled exception (requestId={})", requestId, e);

        return problemResponse(HttpStatus.INTERNAL_SERVER_ERROR,
                Problem.of(500, "INTERNAL_ERROR", "Internal error",
                        "Something went wrong on our side. Quote request id %s if you contact support."
                                .formatted(requestId),
                        req.getRequestURI(), requestId));
    }

    // -----------------------------------------------------------------------

    private ResponseEntity<Problem> respond(HttpStatus status, String code, String title,
                                            String detail, HttpServletRequest req) {
        String requestId = RequestId.of(req);
        log.info("{} {} -> {} {} (requestId={})", req.getMethod(), req.getRequestURI(),
                status.value(), code, requestId);

        return problemResponse(status, Problem.of(status.value(), code, title, detail,
                req.getRequestURI(), requestId));
    }

    private ResponseEntity<Problem> problemResponse(HttpStatus status, Problem problem) {
        return ResponseEntity.status(status)
                .contentType(MediaType.APPLICATION_PROBLEM_JSON)
                .header(RequestId.HEADER, problem.requestId())
                // An error must never be cached. A 404 cached at the edge for a
                // product that is about to go live keeps it invisible.
                .header(HttpHeaders.CACHE_CONTROL, "no-store")
                .body(problem);
    }
}
