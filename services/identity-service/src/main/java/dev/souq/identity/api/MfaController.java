package dev.souq.identity.api;

import java.util.List;

import org.springframework.http.HttpHeaders;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.DeleteMapping;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RestController;

import dev.souq.identity.mfa.MfaService;
import dev.souq.identity.user.JdbcUserRepository;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.validation.Valid;
import jakarta.validation.constraints.Pattern;

/**
 * Two-factor enrolment.
 *
 * <p>Every response here is {@code no-store}, and on this controller that is
 * not boilerplate. The enrolment secret and the recovery codes are each as
 * sensitive as a password, and a cached copy in a browser or an intermediary is
 * a second factor somebody else can install.
 */
@RestController
@RequestMapping("/v1/me/mfa")
public class MfaController {

    private final MfaService mfa;
    private final JdbcUserRepository users;

    public MfaController(MfaService mfa, JdbcUserRepository users) {
        this.mfa = mfa;
        this.users = users;
    }

    public record EnrolmentResponse(String secret, String uri) {}

    public record CodeRequest(
            @Pattern(regexp = "^[0-9]{6}$", message = "must be six digits") String code) {}

    public record ConfirmResponse(List<String> recoveryCodes) {}

    /**
     * Issue a secret.
     *
     * <p>Does not enable anything. The secret is pending until a generated code
     * proves the authenticator holds it — see {@code MfaService}.
     */
    @PostMapping
    public ResponseEntity<EnrolmentResponse> begin(HttpServletRequest http) {
        var principal = AuthenticatedUser.required(http);

        var profile = users.findById(principal.userId())
                .orElseThrow(AuthenticatedUser.NotAuthenticated::new);

        var enrolment = mfa.begin(principal.userId(), profile.email());

        return noStore(new EnrolmentResponse(enrolment.secret(), enrolment.uri()));
    }

    /**
     * Confirm, and receive the recovery codes.
     *
     * <p>The codes are in this response and nowhere else afterwards — only their
     * hashes are stored. There is deliberately no endpoint to fetch them again.
     */
    @PostMapping("/confirm")
    public ResponseEntity<ConfirmResponse> confirm(@Valid @RequestBody CodeRequest body,
                                                   HttpServletRequest http) {
        var principal = AuthenticatedUser.required(http);
        return noStore(new ConfirmResponse(mfa.confirm(principal.userId(), body.code())));
    }

    /**
     * Turn it off.
     *
     * <p>Requires a current code or an unused recovery code, because otherwise a
     * stolen access token can disable the protection that makes a stolen access
     * token survivable.
     */
    @DeleteMapping
    public ResponseEntity<Void> disable(@Valid @RequestBody CodeRequest body,
                                        HttpServletRequest http) {
        var principal = AuthenticatedUser.required(http);
        mfa.disable(principal.userId(), body.code());
        return ResponseEntity.noContent().build();
    }

    private static <T> ResponseEntity<T> noStore(T body) {
        return ResponseEntity.ok()
                .header(HttpHeaders.CACHE_CONTROL, "no-store")
                .header(HttpHeaders.PRAGMA, "no-cache")
                .body(body);
    }
}
