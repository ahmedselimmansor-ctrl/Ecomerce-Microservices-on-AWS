package dev.souq.identity.token;

import java.sql.Array;
import java.sql.Timestamp;
import java.time.Instant;
import java.util.List;
import java.util.Optional;

import org.springframework.jdbc.core.RowMapper;
import org.springframework.jdbc.core.namedparam.MapSqlParameterSource;
import org.springframework.jdbc.core.namedparam.NamedParameterJdbcTemplate;

import dev.souq.identity.token.TokenService.RefreshToken;
import dev.souq.identity.token.TokenService.RefreshTokenRepository;
import dev.souq.identity.token.TokenService.UserSnapshot;

/**
 * Refresh tokens, on JDBC rather than JPA.
 *
 * <p>Deliberate: these queries are hot, simple, and none of them wants an
 * entity graph, dirty checking, or a first-level cache. JPA on this table
 * would add a persistence context around a lookup-by-hash that runs on every
 * token refresh in the platform.
 */
public class JdbcRefreshTokenRepository implements RefreshTokenRepository {

    private final NamedParameterJdbcTemplate jdbc;

    public JdbcRefreshTokenRepository(NamedParameterJdbcTemplate jdbc) {
        this.jdbc = jdbc;
    }

    private static final RowMapper<RefreshToken> MAPPER = (rs, i) -> {
        Array amr = rs.getArray("amr");
        return new RefreshToken(
                rs.getString("id"),
                rs.getString("user_id"),
                rs.getString("session_id"),
                rs.getString("parent_id"),
                rs.getString("token_hash"),
                RefreshToken.State.valueOf(rs.getString("state")),
                rs.getString("device_fingerprint"),
                amr == null ? List.of() : List.of((String[]) amr.getArray()),
                rs.getTimestamp("created_at").toInstant(),
                rs.getTimestamp("expires_at").toInstant());
    };

    @Override
    public Optional<RefreshToken> findByTokenHash(String hash) {
        // Indexed unique lookup — the hot path, hit on every refresh.
        var rows = jdbc.query(
                """
                SELECT id, user_id, session_id, parent_id, token_hash, state,
                       device_fingerprint, amr, created_at, expires_at
                  FROM refresh_tokens WHERE token_hash = :hash
                """,
                new MapSqlParameterSource("hash", hash), MAPPER);
        return rows.stream().findFirst();
    }

    @Override
    public void insert(RefreshToken token) {
        jdbc.update("""
                INSERT INTO refresh_tokens
                  (id, user_id, session_id, parent_id, token_hash, state,
                   device_fingerprint, amr, expires_at)
                VALUES (:id, :userId, :sessionId, :parentId, :hash, :state,
                        :device, :amr, :expiresAt)
                """,
                new MapSqlParameterSource()
                        .addValue("id", token.id())
                        .addValue("userId", token.userId())
                        .addValue("sessionId", token.sessionId())
                        .addValue("parentId", token.parentId())
                        .addValue("hash", token.tokenHash())
                        .addValue("state", token.state().name())
                        .addValue("device", token.deviceFingerprint())
                        .addValue("amr", token.amr().toArray(String[]::new))
                        .addValue("expiresAt", Timestamp.from(token.expiresAt())));
    }

    /**
     * Marks a token used.
     *
     * <p>The {@code AND state = 'ACTIVE'} guard is what makes reuse detection
     * work under concurrency: two simultaneous refreshes with the same token
     * both read {@code ACTIVE}, but only one update affects a row. The loser
     * finds the token already {@code USED} on its next read and triggers the
     * family revocation, which is the correct outcome.
     */
    @Override
    public void markUsed(String id) {
        jdbc.update("""
                UPDATE refresh_tokens
                   SET state = 'USED', used_at = now()
                 WHERE id = :id AND state = 'ACTIVE'
                """,
                new MapSqlParameterSource("id", id));
    }

    @Override
    public int revokeFamily(String sessionId, String reason) {
        return jdbc.update("""
                UPDATE refresh_tokens
                   SET state = 'REVOKED', revoked_reason = :reason
                 WHERE session_id = :sessionId AND state <> 'REVOKED'
                """,
                new MapSqlParameterSource()
                        .addValue("sessionId", sessionId)
                        .addValue("reason", reason));
    }

    @Override
    public int revokeAllForUser(String userId, String reason) {
        return jdbc.update("""
                UPDATE refresh_tokens
                   SET state = 'REVOKED', revoked_reason = :reason
                 WHERE user_id = :userId AND state <> 'REVOKED'
                """,
                new MapSqlParameterSource()
                        .addValue("userId", userId)
                        .addValue("reason", reason));
    }

    /**
     * Loads just enough of the user to mint a new access token.
     *
     * <p>{@code enabled} is read here rather than trusted from the old token:
     * this is the ONE revocation path that works inside the 15-minute access
     * token TTL. A user disabled by support must not get another token.
     */
    @Override
    public Optional<UserSnapshot> loadUserSnapshot(String userId) {
        var rows = jdbc.query("""
                SELECT u.id, u.enabled,
                       coalesce(array_agg(r.role) FILTER (WHERE r.role IS NOT NULL), '{}') AS roles
                  FROM users u
                  LEFT JOIN roles r ON r.user_id = u.id
                 WHERE u.id = :userId
                 GROUP BY u.id, u.enabled
                """,
                new MapSqlParameterSource("userId", userId),
                (rs, i) -> new UserSnapshot(
                        rs.getString("id"),
                        List.of((String[]) rs.getArray("roles").getArray()),
                        rs.getBoolean("enabled")));
        return rows.stream().findFirst();
    }

    /** Trims expired and long-revoked rows. Called nightly. */
    public int purgeExpired() {
        return jdbc.update("""
                DELETE FROM refresh_tokens
                 WHERE expires_at < now() - interval '7 days'
                    OR (state = 'REVOKED' AND created_at < now() - interval '30 days')
                """, new MapSqlParameterSource());
    }

    /** Counts recent reuse detections, for the alert. */
    public int countRecentReuse(Instant since) {
        Integer n = jdbc.queryForObject("""
                SELECT count(*) FROM refresh_tokens
                 WHERE revoked_reason = 'REUSE_DETECTED' AND created_at > :since
                """,
                new MapSqlParameterSource("since", Timestamp.from(since)), Integer.class);
        return n == null ? 0 : n;
    }
}
