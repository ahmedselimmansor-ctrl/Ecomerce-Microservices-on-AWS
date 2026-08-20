package dev.souq.identity.api;

import org.springframework.http.HttpHeaders;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.PatchMapping;
import org.springframework.web.bind.annotation.PathVariable;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RestController;

import dev.souq.identity.api.Dtos.ChangePasswordRequest;
import dev.souq.identity.api.Dtos.MeResponse;
import dev.souq.identity.api.Dtos.UpdateProfileRequest;
import dev.souq.identity.auth.ProfileService;
import dev.souq.identity.user.JdbcUserRepository;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.validation.Valid;

/**
 * Profile endpoints.
 *
 * <p>Everything is scoped to {@code /v1/me} rather than {@code /v1/users/{id}}.
 * The id in a path is caller-supplied, so an endpoint shaped that way needs an
 * ownership check on every method and is broken by whichever one forgets. With
 * {@code /me} the subject comes from the verified token and there is no id to
 * check.
 *
 * <p>The one {@code /v1/users/{id}} route below is for support staff and
 * requires an ADMIN role — the check is explicit and it is the only place the
 * question arises.
 */
@RestController
@RequestMapping("/v1")
public class UserController {

    private final JdbcUserRepository users;
    private final ProfileService profiles;

    public UserController(JdbcUserRepository users, ProfileService profiles) {
        this.users = users;
        this.profiles = profiles;
    }

    @GetMapping("/me")
    public ResponseEntity<MeResponse> me(HttpServletRequest http) {
        var principal = AuthenticatedUser.required(http);

        // Read through to the database rather than reflecting the token's
        // claims back. A token is up to 15 minutes old; a profile page that
        // shows a name the user changed 30 seconds ago looks broken.
        var profile = users.findById(principal.userId())
                .orElseThrow(AuthenticatedUser.NotAuthenticated::new);

        return ResponseEntity.ok()
                .header(HttpHeaders.CACHE_CONTROL, "private, no-store")
                .body(new MeResponse(profile.id(), profile.email(), profile.fullName(),
                        profile.locale(), profile.phone(), profile.emailVerified(),
                        profile.mfaEnabled(), profile.roles()));
    }

    @PatchMapping("/me")
    public ResponseEntity<MeResponse> updateMe(@Valid @RequestBody UpdateProfileRequest body,
                                               HttpServletRequest http) {
        var principal = AuthenticatedUser.required(http);
        users.updateProfile(principal.userId(), body.fullName(), body.locale(), body.phone());
        return me(http);
    }

    /**
     * Changes the password.
     *
     * <p>Requires the current password even though the caller already holds a
     * valid token. A stolen access token is a 15-minute problem; a stolen token
     * that can set a new password is a permanent account takeover.
     *
     * <p>Every other session is revoked on success — see {@code ProfileService}.
     */
    @PostMapping("/me/password")
    public ResponseEntity<Void> changePassword(@Valid @RequestBody ChangePasswordRequest body,
                                               HttpServletRequest http) {
        var principal = AuthenticatedUser.required(http);
        profiles.changePassword(principal.userId(), principal.sessionId(),
                body.currentPassword(), body.newPassword());
        return ResponseEntity.noContent().build();
    }

    /** Support lookup. ADMIN only, and the check is right here where it can be seen. */
    @GetMapping("/users/{userId}")
    public ResponseEntity<MeResponse> byId(@PathVariable String userId, HttpServletRequest http) {
        AuthenticatedUser.withRole(http, "ADMIN");

        var profile = users.findById(userId)
                .orElseThrow(() -> new ProfileService.UserNotFound(userId));

        return ResponseEntity.ok()
                .header(HttpHeaders.CACHE_CONTROL, "private, no-store")
                .body(new MeResponse(profile.id(), profile.email(), profile.fullName(),
                        profile.locale(), profile.phone(), profile.emailVerified(),
                        profile.mfaEnabled(), profile.roles()));
    }
}
