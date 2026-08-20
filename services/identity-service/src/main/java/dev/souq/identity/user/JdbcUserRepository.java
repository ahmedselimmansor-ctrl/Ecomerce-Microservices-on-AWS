package dev.souq.identity.user;

import java.util.List;
import java.util.Optional;

import org.springframework.jdbc.core.namedparam.MapSqlParameterSource;
import org.springframework.jdbc.core.namedparam.NamedParameterJdbcTemplate;
import org.springframework.stereotype.Repository;

/**
 * Reads and writes the user profile.
 *
 * <p>JDBC rather than JPA, matching {@code JdbcRefreshTokenRepository}. Every
 * query here is a single statement whose exact SQL matters — the conditional
 * updates in particular are correct only because they are one statement, and
 * an ORM that decides to split one into a select-then-update reintroduces the
 * race the {@code WHERE} clause exists to close.
 */
@Repository
public class JdbcUserRepository {

    private final NamedParameterJdbcTemplate jdbc;

    public JdbcUserRepository(NamedParameterJdbcTemplate jdbc) {
        this.jdbc = jdbc;
    }

    public record UserProfile(
            String id, String email, String fullName, String locale, String phone,
            boolean emailVerified, boolean phoneVerified, boolean enabled,
            boolean mfaEnabled, List<String> roles) {}

    public Optional<UserProfile> findById(String userId) {
        var rows = jdbc.query("""
                SELECT u.id, u.email, u.full_name, u.locale, u.phone,
                       u.email_verified, u.phone_verified, u.enabled, u.mfa_enabled,
                       coalesce(array_agg(r.role) FILTER (WHERE r.role IS NOT NULL), '{}') AS roles
                  FROM users u
                  LEFT JOIN roles r ON r.user_id = u.id
                 WHERE u.id = :id
                 GROUP BY u.id
                """,
                new MapSqlParameterSource("id", userId),
                (rs, i) -> new UserProfile(
                        rs.getString("id"), rs.getString("email"), rs.getString("full_name"),
                        rs.getString("locale"), rs.getString("phone"),
                        rs.getBoolean("email_verified"), rs.getBoolean("phone_verified"),
                        rs.getBoolean("enabled"), rs.getBoolean("mfa_enabled"),
                        List.of((String[]) rs.getArray("roles").getArray())));

        return rows.stream().findFirst();
    }

    public Optional<String> findIdByEmail(String email) {
        return jdbc.query("SELECT id FROM users WHERE lower(email) = lower(:email)",
                        new MapSqlParameterSource("email", email),
                        (rs, i) -> rs.getString("id"))
                .stream().findFirst();
    }

    /**
     * Updates the mutable parts of a profile.
     *
     * <p>{@code coalesce(:field, column)} leaves a field alone when the caller
     * sends null, so a PATCH that names one field does not blank the other two.
     * The alternative — reading the row, merging in Java, writing it back —
     * loses any concurrent update between the read and the write.
     *
     * <p>Changing the phone number clears {@code phone_verified}. Carrying the
     * old verification over to a new number would let anyone move a verified
     * flag onto a number they do not control.
     */
    public int updateProfile(String userId, String fullName, String locale, String phone) {
        return jdbc.update("""
                UPDATE users
                   SET full_name = coalesce(:fullName, full_name),
                       locale    = coalesce(:locale, locale),
                       phone     = coalesce(:phone, phone),
                       phone_verified = CASE
                           WHEN :phone IS NOT NULL AND :phone IS DISTINCT FROM phone THEN FALSE
                           ELSE phone_verified
                       END,
                       updated_at = now()
                 WHERE id = :id
                """,
                new MapSqlParameterSource()
                        .addValue("id", userId)
                        .addValue("fullName", fullName)
                        .addValue("locale", locale)
                        .addValue("phone", phone));
    }

    /** Returns the current hash, or empty if the user does not exist. */
    public Optional<String> currentPasswordHash(String userId) {
        return jdbc.query("SELECT password_hash FROM credentials WHERE user_id = :id",
                        new MapSqlParameterSource("id", userId),
                        (rs, i) -> rs.getString("password_hash"))
                .stream().findFirst();
    }

    /**
     * Replaces the password and remembers the old hash.
     *
     * <p>{@code previous_hashes} keeps the last five so a "reset" that sets the
     * same password back is caught. Five, not all of them: the list is checked
     * with Argon2id, so an unbounded array turns every password change into an
     * unbounded amount of deliberately expensive work.
     */
    public void replacePassword(String userId, String newHash, String oldHash) {
        jdbc.update("""
                UPDATE credentials
                   SET password_hash = :newHash,
                       previous_hashes = (array_prepend(:oldHash, previous_hashes))[1:5],
                       needs_rehash = FALSE,
                       changed_at = now()
                 WHERE user_id = :id
                """,
                new MapSqlParameterSource()
                        .addValue("id", userId)
                        .addValue("newHash", newHash)
                        .addValue("oldHash", oldHash));
    }

    public List<String> previousHashes(String userId) {
        var rows = jdbc.query("SELECT previous_hashes FROM credentials WHERE user_id = :id",
                new MapSqlParameterSource("id", userId),
                (rs, i) -> List.of((String[]) rs.getArray("previous_hashes").getArray()));
        return rows.isEmpty() ? List.of() : rows.get(0);
    }

    /**
     * Marks an email verified.
     *
     * <p>The {@code AND NOT email_verified} guard makes this return 0 on a
     * replayed link rather than silently succeeding, so the caller can tell a
     * first click from a second one.
     */
    public int markEmailVerified(String userId) {
        return jdbc.update("""
                UPDATE users SET email_verified = TRUE, updated_at = now()
                 WHERE id = :id AND NOT email_verified
                """,
                new MapSqlParameterSource("id", userId));
    }

    public int setEnabled(String userId, boolean enabled) {
        return jdbc.update("UPDATE users SET enabled = :enabled, updated_at = now() WHERE id = :id",
                new MapSqlParameterSource().addValue("id", userId).addValue("enabled", enabled));
    }
}
