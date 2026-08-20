package dev.souq.identity.api;

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
import org.springframework.web.servlet.NoHandlerFoundException;

import dev.souq.identity.auth.AuthService.AccountLocked;
import dev.souq.identity.auth.AuthService.AuthenticationFailed;
import dev.souq.identity.auth.AuthService.MfaRequired;
import dev.souq.identity.auth.AuthService.WeakPassword;
import dev.souq.identity.auth.ProfileService;
import dev.souq.identity.mfa.MfaService;
import dev.souq.identity.token.TokenService.InvalidTokenException;
import dev.souq.identity.token.TokenService.TokenReuseException;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.validation.ConstraintViolationException;

/**
 * Turns every exception into the RFC 9457 envelope.
 *
 * <p>Two rules run through all of it.
 *
 * <p><b>Nothing internal reaches the client.</b> The last handler catches
 * {@code Exception} and returns a fixed string plus a request id. A stack trace
 * or a JDBC message in a response body tells an attacker the framework, the
 * driver, the table names and often the query — and it is never anything the
 * user could act on.
 *
 * <p><b>4xx are logged at INFO without a stack trace, 5xx at ERROR with one.</b>
 * A client sending bad input is not an operational event. Logging it at ERROR
 * fills the dashboard with things nobody should act on, and a dashboard full of
 * noise is one where the real 500 goes unnoticed.
 */
@RestControllerAdvice
public class GlobalExceptionHandler {

    private static final Logger log = LoggerFactory.getLogger(GlobalExceptionHandler.class);

    // -------------------------------------------------------------- 401 / 403

    /**
     * Wrong password, unknown account, and disabled account all land here with
     * an identical body. See {@code AuthService#login} — distinguishing them
     * turns login into an account-enumeration oracle.
     */
    @ExceptionHandler(AuthenticationFailed.class)
    public ResponseEntity<Problem> onAuthenticationFailed(AuthenticationFailed e, HttpServletRequest req) {
        return respond(HttpStatus.UNAUTHORIZED, "UNAUTHENTICATED", "Authentication failed",
                "The email address or password is incorrect.", req);
    }

    @ExceptionHandler(MfaRequired.class)
    public ResponseEntity<Problem> onMfaRequired(MfaRequired e, HttpServletRequest req) {
        return respond(HttpStatus.UNAUTHORIZED, "MFA_REQUIRED", "Multi-factor code required",
                "Send the six-digit code from your authenticator app as mfaCode.", req);
    }

    /**
     * A replayed refresh token. The family is already revoked by the time this
     * runs, so the client must re-authenticate — and the cookie is cleared here
     * so it does not keep presenting a token that will never work again.
     */
    @ExceptionHandler(TokenReuseException.class)
    public ResponseEntity<Problem> onTokenReuse(TokenReuseException e, HttpServletRequest req) {
        log.warn("refresh token reuse: session {} for user {} — all sessions revoked",
                e.sessionId(), e.userId());

        var problem = Problem.of(401, "REFRESH_TOKEN_REUSED", "Session ended",
                "This session has been ended for security. Please sign in again.",
                req.getRequestURI(), RequestId.of(req));

        return ResponseEntity.status(HttpStatus.UNAUTHORIZED)
                .header(HttpHeaders.SET_COOKIE,
                        "souq_rt=; Path=/v1/auth; Max-Age=0; HttpOnly; SameSite=Strict")
                .contentType(MediaType.APPLICATION_PROBLEM_JSON)
                .body(problem);
    }

    @ExceptionHandler(InvalidTokenException.class)
    public ResponseEntity<Problem> onInvalidToken(InvalidTokenException e, HttpServletRequest req) {
        // The message says why (expired, revoked, unrecognised). Unlike login,
        // this reveals nothing: the caller already had to present a token, and
        // knowing that it expired is what tells the client to re-authenticate
        // rather than retry.
        return respond(HttpStatus.UNAUTHORIZED, "TOKEN_EXPIRED", "Invalid refresh token",
                e.getMessage(), req);
    }

    @ExceptionHandler(AuthenticatedUser.NotAuthenticated.class)
    public ResponseEntity<Problem> onNotAuthenticated(AuthenticatedUser.NotAuthenticated e,
                                                      HttpServletRequest req) {
        return ResponseEntity.status(HttpStatus.UNAUTHORIZED)
                .header(HttpHeaders.WWW_AUTHENTICATE, "Bearer")
                .contentType(MediaType.APPLICATION_PROBLEM_JSON)
                .body(Problem.of(401, "UNAUTHENTICATED", "Not signed in", e.getMessage(),
                        req.getRequestURI(), RequestId.of(req)));
    }

    /**
     * Authenticated but not allowed.
     *
     * <p>403, not 404. Hiding the endpoint's existence would be the stronger
     * posture, but these are fixed, documented routes — the caller can read
     * docs/CONTRACTS.md — so a 404 would only mislead a legitimate user with
     * the wrong role into filing a bug.
     */
    @ExceptionHandler(AuthenticatedUser.NotPermitted.class)
    public ResponseEntity<Problem> onNotPermitted(AuthenticatedUser.NotPermitted e,
                                                  HttpServletRequest req) {
        return respond(HttpStatus.FORBIDDEN, "FORBIDDEN", "Not permitted", e.getMessage(), req);
    }

    @ExceptionHandler(ProfileService.WrongCurrentPassword.class)
    public ResponseEntity<Problem> onWrongCurrentPassword(ProfileService.WrongCurrentPassword e,
                                                          HttpServletRequest req) {
        // 403 rather than 401: the access token is fine, the extra proof is not.
        // A 401 would make a browser client try to refresh and then retry, and
        // the retry fails identically.
        return respond(HttpStatus.FORBIDDEN, "FORBIDDEN", "Current password incorrect",
                "The current password you entered is incorrect.", req);
    }

    @ExceptionHandler(ProfileService.PasswordReused.class)
    public ResponseEntity<Problem> onPasswordReused(ProfileService.PasswordReused e,
                                                    HttpServletRequest req) {
        var problem = Problem.of(422, "WEAK_PASSWORD", "Password rejected",
                        "That is your current password. Choose a different one.",
                        req.getRequestURI(), RequestId.of(req))
                .withExtensions(Map.of("reasonCode", "PASSWORD_REUSED"));

        return problemResponse(HttpStatus.UNPROCESSABLE_ENTITY, problem);
    }

    @ExceptionHandler(ProfileService.UserNotFound.class)
    public ResponseEntity<Problem> onUserNotFound(ProfileService.UserNotFound e,
                                                  HttpServletRequest req) {
        return respond(HttpStatus.NOT_FOUND, "VALIDATION_FAILED", "No such user",
                "No user with that id exists.", req);
    }

    // ------------------------------------------------------------------ MFA

    @ExceptionHandler(MfaService.AlreadyEnrolled.class)
    public ResponseEntity<Problem> onAlreadyEnrolled(MfaService.AlreadyEnrolled e,
                                                     HttpServletRequest req) {
        return respond(HttpStatus.CONFLICT, "VALIDATION_FAILED", "Already set up",
                "Two-factor authentication is already on. Turn it off before setting it up again.",
                req);
    }

    @ExceptionHandler(MfaService.NoPendingEnrolment.class)
    public ResponseEntity<Problem> onNoPendingEnrolment(MfaService.NoPendingEnrolment e,
                                                        HttpServletRequest req) {
        return respond(HttpStatus.CONFLICT, "VALIDATION_FAILED", "Start again",
                e.getMessage(), req);
    }

    /**
     * A rejected code.
     *
     * <p>Reported the same way whether the code was wrong, expired or reused —
     * unlike most of this handler, where distinguishing cases helps. Here it
     * would tell someone brute-forcing six digits whether they were close.
     */
    @ExceptionHandler(MfaService.BadCode.class)
    public ResponseEntity<Problem> onBadMfaCode(MfaService.BadCode e, HttpServletRequest req) {
        return respond(HttpStatus.UNAUTHORIZED, "MFA_REQUIRED", "Code not accepted",
                "That code was not accepted. Check your authenticator app and try again.", req);
    }

    // -------------------------------------------------------------- 423 / 429

    /**
     * Locked out. {@code Retry-After} in seconds, per RFC 9110 — without it a
     * client has no way to know whether to wait a second or a quarter of an
     * hour, and typically retries immediately and extends the lockout.
     */
    @ExceptionHandler(AccountLocked.class)
    public ResponseEntity<Problem> onLocked(AccountLocked e, HttpServletRequest req) {
        long seconds = e.retryAfter().toSeconds();

        var problem = Problem.of(429, "ACCOUNT_LOCKED", "Too many attempts",
                        "Too many failed sign-in attempts. Try again in %d minutes."
                                .formatted(Math.max(1, seconds / 60)),
                        req.getRequestURI(), RequestId.of(req))
                .withExtensions(Map.of("retryAfterSeconds", seconds));

        return ResponseEntity.status(HttpStatus.TOO_MANY_REQUESTS)
                .header(HttpHeaders.RETRY_AFTER, Long.toString(seconds))
                .contentType(MediaType.APPLICATION_PROBLEM_JSON)
                .body(problem);
    }

    // ---------------------------------------------------------------- 400/422

    @ExceptionHandler(WeakPassword.class)
    public ResponseEntity<Problem> onWeakPassword(WeakPassword e, HttpServletRequest req) {
        var problem = Problem.of(422, "WEAK_PASSWORD", "Password rejected",
                        e.getMessage(), req.getRequestURI(), RequestId.of(req))
                // The reason code is what lets the UI show a specific hint
                // ("this password has appeared in a breach") without parsing
                // the prose, which is localised.
                .withExtensions(Map.of("reasonCode", e.reasonCode()));

        return ResponseEntity.unprocessableEntity()
                .contentType(MediaType.APPLICATION_PROBLEM_JSON)
                .body(problem);
    }

    /** Bean Validation failures on a {@code @RequestBody}. */
    @ExceptionHandler(MethodArgumentNotValidException.class)
    public ResponseEntity<Problem> onInvalidBody(MethodArgumentNotValidException e, HttpServletRequest req) {
        List<Problem.FieldError> fields = e.getBindingResult().getFieldErrors().stream()
                .map(f -> new Problem.FieldError(f.getField(),
                        f.getDefaultMessage() == null ? "is invalid" : f.getDefaultMessage()))
                // Bounded. A caller that posts a thousand-element array should
                // not be able to make us build a thousand-entry response.
                .limit(20)
                .toList();

        var problem = Problem.of(400, "VALIDATION_FAILED", "Request validation failed",
                        "One or more fields are invalid.", req.getRequestURI(), RequestId.of(req))
                .withFieldErrors(fields);

        return problemResponse(HttpStatus.BAD_REQUEST, problem);
    }

    @ExceptionHandler(ConstraintViolationException.class)
    public ResponseEntity<Problem> onConstraintViolation(ConstraintViolationException e, HttpServletRequest req) {
        List<Problem.FieldError> fields = e.getConstraintViolations().stream()
                .map(v -> new Problem.FieldError(v.getPropertyPath().toString(), v.getMessage()))
                .limit(20)
                .toList();

        var problem = Problem.of(400, "VALIDATION_FAILED", "Request validation failed",
                        "One or more parameters are invalid.", req.getRequestURI(), RequestId.of(req))
                .withFieldErrors(fields);

        return problemResponse(HttpStatus.BAD_REQUEST, problem);
    }

    @ExceptionHandler(HttpMessageNotReadableException.class)
    public ResponseEntity<Problem> onUnreadableBody(HttpMessageNotReadableException e, HttpServletRequest req) {
        // Deliberately not e.getMessage(): Jackson's parse errors quote the
        // offending input back, which for this service means echoing a password
        // into a response body and into the client's logs.
        return respond(HttpStatus.BAD_REQUEST, "VALIDATION_FAILED", "Malformed request body",
                "The request body is not valid JSON, or a required field is missing.", req);
    }

    @ExceptionHandler(MissingServletRequestParameterException.class)
    public ResponseEntity<Problem> onMissingParam(MissingServletRequestParameterException e, HttpServletRequest req) {
        return respond(HttpStatus.BAD_REQUEST, "VALIDATION_FAILED", "Missing parameter",
                "Required parameter '%s' is missing.".formatted(e.getParameterName()), req);
    }

    // -------------------------------------------------------------------- 404

    @ExceptionHandler(NoHandlerFoundException.class)
    public ResponseEntity<Problem> onNoHandler(NoHandlerFoundException e, HttpServletRequest req) {
        return respond(HttpStatus.NOT_FOUND, "VALIDATION_FAILED", "No such endpoint",
                "%s %s is not an endpoint on this service.".formatted(e.getHttpMethod(), e.getRequestURL()), req);
    }

    @ExceptionHandler(HttpRequestMethodNotSupportedException.class)
    public ResponseEntity<Problem> onWrongMethod(HttpRequestMethodNotSupportedException e, HttpServletRequest req) {
        return respond(HttpStatus.METHOD_NOT_ALLOWED, "VALIDATION_FAILED", "Method not allowed",
                "%s is not supported on this endpoint.".formatted(e.getMethod()), req);
    }

    // -------------------------------------------------------------------- 5xx

    /**
     * A constraint the application should have checked first.
     *
     * <p>Reaching here means a race got past a pre-check, which is a bug in the
     * calling code rather than in the request. The database's message names
     * tables and columns, so it is logged and not returned.
     */
    @ExceptionHandler(DataIntegrityViolationException.class)
    public ResponseEntity<Problem> onIntegrityViolation(DataIntegrityViolationException e, HttpServletRequest req) {
        String requestId = RequestId.of(req);
        log.error("database constraint violated (requestId={})", requestId, e);

        return problemResponse(HttpStatus.CONFLICT,
                Problem.of(409, "INTERNAL_ERROR", "Conflict",
                        "The request conflicts with the current state. Please try again.",
                        req.getRequestURI(), requestId));
    }

    @ExceptionHandler(Exception.class)
    public ResponseEntity<Problem> onUnexpected(Exception e, HttpServletRequest req) {
        String requestId = RequestId.of(req);
        log.error("unhandled exception (requestId={})", requestId, e);

        return problemResponse(HttpStatus.INTERNAL_SERVER_ERROR,
                Problem.of(500, "INTERNAL_ERROR", "Internal error",
                        // Fixed text. The request id is the only handle the
                        // user gets, and it is the one that finds the log line.
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
                .body(problem);
    }
}
