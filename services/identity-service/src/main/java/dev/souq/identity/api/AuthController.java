package dev.souq.identity.api;

import java.time.Duration;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.http.HttpHeaders;
import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseCookie;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.CookieValue;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RequestHeader;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RestController;

import dev.souq.identity.api.Dtos.AcceptedResponse;
import dev.souq.identity.api.Dtos.LoginRequest;
import dev.souq.identity.api.Dtos.RefreshRequest;
import dev.souq.identity.api.Dtos.RegisterRequest;
import dev.souq.identity.api.Dtos.TokenResponse;
import dev.souq.identity.auth.AuthService;
import dev.souq.identity.token.TokenService;
import dev.souq.identity.token.TokenService.TokenPair;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.validation.Valid;

/**
 * The auth API.
 *
 * <p>The refresh token is carried in an HttpOnly, Secure, SameSite=Strict
 * cookie and never appears in a response body. That single decision drives
 * most of what looks unusual here:
 *
 * <ul>
 *   <li>{@code /refresh} reads the cookie, not the body — the body field exists
 *       only for non-browser clients (the mobile app), and the cookie wins when
 *       both are present.</li>
 *   <li>Every response that mints a token also sets the cookie, because
 *       rotation means the old value is dead the moment the new one is issued.
 *       Forgetting the {@code Set-Cookie} on one path logs the user out.</li>
 *   <li>{@code Path=/v1/auth} scopes the cookie to the three endpoints that
 *       need it, so it is not attached to every catalogue request the browser
 *       makes.</li>
 * </ul>
 */
@RestController
@RequestMapping("/v1/auth")
public class AuthController {

    private static final Logger log = LoggerFactory.getLogger(AuthController.class);

    /** Matches the refresh TTL in docs/CONTRACTS.md §7. */
    private static final Duration REFRESH_COOKIE_MAX_AGE = Duration.ofDays(30);
    private static final String REFRESH_COOKIE = "souq_rt";

    private final AuthService auth;
    private final TokenService tokens;
    private final dev.souq.identity.auth.ProfileService profiles;
    private final boolean secureCookies;

    public AuthController(AuthService auth, TokenService tokens,
                          dev.souq.identity.auth.ProfileService profiles,
                          @org.springframework.beans.factory.annotation.Value(
                                  "${souq.security.secure-cookies:true}") boolean secureCookies) {
        this.auth = auth;
        this.tokens = tokens;
        this.profiles = profiles;
        this.secureCookies = secureCookies;
    }

    /**
     * Registers a user.
     *
     * <p>Always 202, always the same body — see {@code AuthService#register}.
     * A 409 here would turn this endpoint into an account-enumeration oracle
     * that anyone can query without authenticating.
     */
    @PostMapping("/register")
    public ResponseEntity<AcceptedResponse> register(@Valid @RequestBody RegisterRequest body,
                                                     HttpServletRequest http) {
        auth.register(new AuthService.RegisterCommand(
                body.email(), body.password(), body.fullName(),
                body.localeOrDefault(), body.acceptedTermsVersion(),
                ClientAddress.of(http)));

        return ResponseEntity.accepted().body(AcceptedResponse.checkYourEmail());
    }

    @PostMapping("/login")
    public ResponseEntity<TokenResponse> login(@Valid @RequestBody LoginRequest body,
                                               @RequestHeader(value = "User-Agent", required = false) String userAgent,
                                               HttpServletRequest http) {
        var result = auth.login(new AuthService.LoginCommand(
                body.email(), body.password(), body.mfaCode(),
                ClientAddress.of(http), userAgent, body.deviceFingerprint()));

        return withRefreshCookie(result.tokens());
    }

    /**
     * Rotates the refresh token.
     *
     * <p>The presented token is invalidated whether or not this succeeds. If it
     * had already been used, {@code TokenService} revokes the whole family and
     * throws — the standard response to a replayed refresh token, because
     * either the legitimate client or an attacker has a copy and there is no
     * way to tell which.
     */
    @PostMapping("/refresh")
    public ResponseEntity<TokenResponse> refresh(
            @CookieValue(value = REFRESH_COOKIE, required = false) String cookieToken,
            @RequestBody(required = false) @Valid RefreshRequest body,
            HttpServletRequest http) {

        // Cookie first. A body value is only for clients that cannot hold
        // cookies; when a browser sends both, the cookie is the one the
        // SameSite protection actually applies to.
        String presented = cookieToken != null && !cookieToken.isBlank()
                ? cookieToken
                : (body == null ? null : body.refreshToken());

        if (presented == null || presented.isBlank()) {
            throw new TokenService.InvalidTokenException("no refresh token was presented");
        }

        String fingerprint = body == null ? null : body.deviceFingerprint();
        String requestId = RequestId.of(http);

        TokenPair pair = tokens.refresh(presented, fingerprint, requestId);
        return withRefreshCookie(pair);
    }

    /**
     * Ends one session.
     *
     * <p>Idempotent and always 204. A logout that can fail is a logout users
     * will abandon halfway, leaving the session alive — and there is nothing
     * useful a client can do with the error anyway.
     */
    @PostMapping("/logout")
    public ResponseEntity<Void> logout(
            @CookieValue(value = REFRESH_COOKIE, required = false) String cookieToken) {

        if (cookieToken != null && !cookieToken.isBlank()) {
            try {
                tokens.revokeByToken(cookieToken, "user_logout");
            } catch (RuntimeException e) {
                // An unknown or already-revoked token is the desired end state.
                log.debug("logout presented a token that was already invalid");
            }
        }

        return ResponseEntity.noContent()
                .header(HttpHeaders.SET_COOKIE, clearedCookie().toString())
                .build();
    }

    /**
     * Starts a password reset.
     *
     * <p>202 with a fixed body whether or not the address has an account, for
     * the same reason registration does — this endpoint takes no credentials,
     * so any observable difference is an enumeration oracle anyone can query.
     */
    @PostMapping("/forgot-password")
    public ResponseEntity<AcceptedResponse> forgotPassword(
            @Valid @RequestBody Dtos.ForgotPasswordRequest body) {
        profiles.requestPasswordReset(body.email());
        return ResponseEntity.accepted().body(AcceptedResponse.checkYourEmail());
    }

    /**
     * Completes a password reset.
     *
     * <p>No tokens are issued on success. Signing the user in from a link in an
     * email means anyone who can read the inbox — or who intercepted the link
     * at any point in its life — is signed in too. They re-enter the password
     * they just chose, which also confirms they typed what they meant to.
     */
    @PostMapping("/reset-password")
    public ResponseEntity<Void> resetPassword(@Valid @RequestBody Dtos.ResetPasswordRequest body) {
        profiles.completePasswordReset(body.token(), body.newPassword());

        return ResponseEntity.noContent()
                .header(HttpHeaders.SET_COOKIE, clearedCookie().toString())
                .build();
    }

    // -----------------------------------------------------------------------

    private ResponseEntity<TokenResponse> withRefreshCookie(TokenPair pair) {
        return ResponseEntity.status(HttpStatus.OK)
                .header(HttpHeaders.SET_COOKIE, refreshCookie(pair.refreshToken()).toString())
                // Tokens must never sit in a shared cache or a CDN edge.
                .header(HttpHeaders.CACHE_CONTROL, "no-store")
                .header(HttpHeaders.PRAGMA, "no-cache")
                .body(TokenResponse.of(pair.accessToken(), pair.expiresInSeconds(),
                        pair.userId(), pair.roles()));
    }

    private ResponseCookie refreshCookie(String value) {
        return ResponseCookie.from(REFRESH_COOKIE, value)
                .httpOnly(true)          // unreadable from JavaScript, so XSS cannot exfiltrate it
                .secure(secureCookies)   // false only for plain-HTTP local dev
                .sameSite("Strict")      // never sent on a cross-site request, so CSRF cannot use it
                .path("/v1/auth")        // scoped to the endpoints that need it
                .maxAge(REFRESH_COOKIE_MAX_AGE)
                .build();
    }

    private ResponseCookie clearedCookie() {
        // Same name, path and attributes with maxAge=0. A mismatch on any of
        // them and the browser keeps the original cookie alongside the deletion.
        return ResponseCookie.from(REFRESH_COOKIE, "")
                .httpOnly(true).secure(secureCookies).sameSite("Strict")
                .path("/v1/auth").maxAge(0).build();
    }
}
