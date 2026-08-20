package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Idempotency for HTTP mutations (docs/CONTRACTS.md §5.3).
//
// The structure here follows payment-service internal/psp/paymob_test.go directly: claim the
// key with a single atomic INSERT rather than SELECT-then-INSERT, because two
// concurrent retries both observing an absent row is a real interleaving that
// the explorer finds in five steps.

var (
	// ErrKeyReused means the same key arrived with a different body. That is a
	// client bug and must be rejected loudly — replaying the stored response
	// would silently discard whatever the client actually asked for the second
	// time.
	ErrKeyReused = errors.New("idempotency key reused with a different request body")

	// ErrInProgress means an identical request is mid-flight. The client
	// should back off and retry, not be served a half-built response.
	ErrInProgress = errors.New("an identical request is already in progress")
)

// Claim result.
type ClaimOutcome int

const (
	// ClaimedFresh: this caller owns the key and must do the work.
	ClaimedFresh ClaimOutcome = iota
	// ClaimedReplay: the work is already done; ResponseCode/ResponseBody hold it.
	ClaimedReplay
)

type Replay struct {
	Outcome      ClaimOutcome
	ResponseCode int
	ResponseBody []byte
}

// HashRequest canonicalises a request body for comparison. Marshalling through
// a map sorts the keys, so a client that reorders JSON fields between retries
// is not treated as having sent a different request.
func HashRequest(body []byte) (string, error) {
	var canonical any
	if err := json.Unmarshal(body, &canonical); err != nil {
		// Not JSON: hash the bytes as they came.
		sum := sha256.Sum256(body)
		return hex.EncodeToString(sum[:]), nil
	}
	normalised, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(normalised)
	return hex.EncodeToString(sum[:]), nil
}

// ClaimKey attempts to take ownership of an idempotency key.
//
// One statement decides the race. `ON CONFLICT DO NOTHING` plus a check of
// rows-affected means the primary key itself is the mutual exclusion; there is
// no window between "is it there?" and "put it there" for a second request to
// slip through.
func ClaimKey(ctx context.Context, tx pgx.Tx, key, userID, endpoint, requestHash string) (Replay, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys (key, user_id, endpoint, request_hash, state)
		VALUES ($1, $2, $3, $4, 'IN_PROGRESS')
		ON CONFLICT (key) DO NOTHING`,
		key, userID, endpoint, requestHash)
	if err != nil {
		return Replay{}, fmt.Errorf("claim idempotency key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return Replay{Outcome: ClaimedFresh}, nil
	}

	// Somebody else owns it. Find out who, and whether they finished.
	var (
		existingHash string
		existingUser string
		state        string
		code         *int
		body         []byte
	)
	err = tx.QueryRow(ctx, `
		SELECT request_hash, user_id, state, response_code, response_body
		  FROM idempotency_keys WHERE key = $1`, key).
		Scan(&existingHash, &existingUser, &state, &code, &body)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Expired between the INSERT and this SELECT. Vanishingly rare;
			// treat as a conflict and let the client retry cleanly.
			return Replay{}, ErrInProgress
		}
		return Replay{}, fmt.Errorf("load idempotency key: %w", err)
	}

	// A key belonging to another user is not a replay, it is either a bug or
	// an attempt to read someone else's response. Reject it as reuse.
	if existingUser != userID || existingHash != requestHash {
		return Replay{}, ErrKeyReused
	}

	if state == "IN_PROGRESS" {
		return Replay{}, ErrInProgress
	}

	r := Replay{Outcome: ClaimedReplay, ResponseBody: body}
	if code != nil {
		r.ResponseCode = *code
	}
	return r, nil
}

// CompleteKey stores the response so subsequent retries replay it verbatim.
// Called in the same transaction as the work it describes, so a crash between
// doing the work and recording it is impossible.
func CompleteKey(ctx context.Context, tx pgx.Tx, key string, code int, body []byte) error {
	_, err := tx.Exec(ctx, `
		UPDATE idempotency_keys
		   SET state = 'COMPLETED', response_code = $2, response_body = $3
		 WHERE key = $1`, key, code, body)
	if err != nil {
		return fmt.Errorf("complete idempotency key: %w", err)
	}
	return nil
}

// ReleaseKey removes a claim whose work failed, so the client can retry
// immediately instead of waiting out the 24h TTL.
//
// Only safe for failures with NO external side effect — a validation error, a
// database rollback. Never call it after a payment or reservation attempt: the
// crash-and-reap path in payment-service internal/psp/paymob_test.go §4 is exactly how a
// released key turns into a second charge.
func ReleaseKey(ctx context.Context, tx pgx.Tx, key string) error {
	_, err := tx.Exec(ctx,
		`DELETE FROM idempotency_keys WHERE key = $1 AND state = 'IN_PROGRESS'`, key)
	return err
}

// PurgeExpiredKeys is the TTL reaper. Runs nightly.
func (s *Store) PurgeExpiredKeys(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM idempotency_keys WHERE expires_at < now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
