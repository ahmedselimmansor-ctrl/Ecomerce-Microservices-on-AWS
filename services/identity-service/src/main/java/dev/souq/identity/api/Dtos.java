package dev.souq.identity.api;

import java.util.List;

import jakarta.validation.constraints.Email;
import jakarta.validation.constraints.NotBlank;
import jakarta.validation.constraints.Pattern;
import jakarta.validation.constraints.Size;

/**
 * Wire types for the auth API.
 *
 * <p>Records, so they are immutable and cannot be half-populated by a setter
 * that a later refactor forgets to call. Validation lives on the DTO rather
 * than in the controller body because a constraint written next to the field
 * is a constraint that survives someone adding a second call site.
 *
 * <p>Note what is <em>not</em> validated here: password strength. A
 * {@code @Pattern} on the password field would (a) put the rule in two places
 * and (b) report it as a field error rather than the structured reason code
 * {@code PasswordService} produces. Length is checked to bound the Argon2id
 * input; everything else is the domain's decision.
 */
public final class Dtos {

    private Dtos() {}

    public record RegisterRequest(
            @NotBlank @Email @Size(max = 254) String email,
            // The upper bound matters: Argon2id hashes whatever it is given, so
            // an unbounded password field is a CPU-exhaustion vector.
            @NotBlank @Size(min = 1, max = 1024) String password,
            @NotBlank @Size(max = 200) String fullName,
            @Pattern(regexp = "^[a-z]{2}-[A-Z]{2}$", message = "must be a BCP 47 tag such as en-GB")
            String locale,
            @NotBlank String acceptedTermsVersion) {

        public String localeOrDefault() {
            return locale == null || locale.isBlank() ? "en-GB" : locale;
        }
    }

    public record LoginRequest(
            @NotBlank @Email @Size(max = 254) String email,
            @NotBlank @Size(max = 1024) String password,
            @Pattern(regexp = "^[0-9]{6}$", message = "must be six digits") String mfaCode,
            @Size(max = 128) String deviceFingerprint) {}

    public record RefreshRequest(
            @Size(max = 512) String refreshToken,
            @Size(max = 128) String deviceFingerprint) {}

    /**
     * The login/refresh response.
     *
     * <p>The refresh token is deliberately absent. It goes back as an HttpOnly
     * cookie and never enters a JSON body, because a body is readable by any
     * script on the page and a 30-day credential in {@code localStorage} is one
     * XSS away from a permanent account takeover. See docs/CONTRACTS.md §8.
     */
    public record TokenResponse(
            String accessToken,
            String tokenType,
            long expiresIn,
            String userId,
            List<String> roles) {

        public static TokenResponse of(String accessToken, long expiresIn,
                                       String userId, List<String> roles) {
            return new TokenResponse(accessToken, "Bearer", expiresIn, userId, roles);
        }
    }

    /** Registration is deliberately uninformative — see {@code AuthService#register}. */
    public record AcceptedResponse(String status, String message) {
        public static AcceptedResponse checkYourEmail() {
            return new AcceptedResponse("accepted",
                    "If that address can receive mail, a message is on its way to it.");
        }
    }

    public record MeResponse(
            String id,
            String email,
            String fullName,
            String locale,
            String phone,
            boolean emailVerified,
            boolean mfaEnabled,
            List<String> roles) {}

    public record UpdateProfileRequest(
            @Size(max = 200) String fullName,
            @Pattern(regexp = "^[a-z]{2}-[A-Z]{2}$") String locale,
            @Pattern(regexp = "^\\+[1-9][0-9]{7,14}$", message = "must be E.164") String phone) {}

    public record ChangePasswordRequest(
            @NotBlank @Size(max = 1024) String currentPassword,
            @NotBlank @Size(min = 1, max = 1024) String newPassword) {}

    public record ForgotPasswordRequest(@NotBlank @Email @Size(max = 254) String email) {}

    public record ResetPasswordRequest(
            @NotBlank @Size(max = 512) String token,
            @NotBlank @Size(min = 1, max = 1024) String newPassword) {}
}
