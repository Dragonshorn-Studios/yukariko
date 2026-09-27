package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// APIKey is one machine credential for the read-only HTTP API. TokenHash is
// the SHA-256 of the ykr_-prefixed bearer token, never the token itself.
// Nullable timestamps (expiry, last use, revocation) stay unset when absent.
type APIKey struct {
	ID         string
	Name       string
	TokenHash  string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// Active reports whether the key currently grants access: not revoked and
// not expired. Validation happens at lookup time — a revoked key stops
// working immediately, with nothing cached anywhere.
func (k APIKey) Active(now time.Time) bool {
	if k.RevokedAt != nil {
		return false
	}
	return k.ExpiresAt == nil || k.ExpiresAt.After(now)
}

// CreateAPIKey stores one key row. The caller owns token generation and
// passes only the hash; the full token is never persisted.
func (s *Store) CreateAPIKey(ctx context.Context, k APIKey) error {
	if k.ID == "" {
		k.ID = newID()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO api_keys (id, name, token_hash, created_at, expires_at, last_used_at, revoked_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.Name, k.TokenHash, rfc3339(k.CreatedAt), nullableTime(k.ExpiresAt), nullableTime(k.LastUsedAt), nullableTime(k.RevokedAt))
	if err != nil {
		return fmt.Errorf("create api key: %w", err)
	}
	return nil
}

// APIKeyByHash returns the row for a token hash regardless of validity;
// callers decide with Active. ok=false covers unknown hashes only — an
// invalid (revoked/expired) key and an unknown one are deliberately
// indistinguishable to the requester.
func (s *Store) APIKeyByHash(ctx context.Context, tokenHash string) (APIKey, bool, error) {
	return s.scanAPIKey(s.db.QueryRowContext(ctx,
		`SELECT id, name, token_hash, created_at, expires_at, last_used_at, revoked_at
		   FROM api_keys WHERE token_hash = ?`, tokenHash))
}

// APIKeyByName returns the row for a human-readable name (CLI pre-checks).
func (s *Store) APIKeyByName(ctx context.Context, name string) (APIKey, bool, error) {
	return s.scanAPIKey(s.db.QueryRowContext(ctx,
		`SELECT id, name, token_hash, created_at, expires_at, last_used_at, revoked_at
		   FROM api_keys WHERE name = ?`, name))
}

// APIKeyByID returns one row by its opaque id.
func (s *Store) APIKeyByID(ctx context.Context, id string) (APIKey, bool, error) {
	return s.scanAPIKey(s.db.QueryRowContext(ctx,
		`SELECT id, name, token_hash, created_at, expires_at, last_used_at, revoked_at
		   FROM api_keys WHERE id = ?`, id))
}

// ListAPIKeys returns every key, oldest first. Revoked keys stay listed
// (soft revoke is also the audit trail) and report their status.
func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, token_hash, created_at, expires_at, last_used_at, revoked_at
		   FROM api_keys ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		k, err := scanAPIKeyRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	return out, nil
}

// RevokeAPIKey soft-revokes by id or name. ok=false means the key does not
// exist or was already revoked — callers distinguish via the lookups above.
func (s *Store) RevokeAPIKey(ctx context.Context, ref string, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE api_keys SET revoked_at = ? WHERE (id = ? OR name = ?) AND revoked_at IS NULL`,
		rfc3339(now), ref, ref)
	if err != nil {
		return false, fmt.Errorf("revoke api key: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("revoke api key: %w", err)
	}
	return n > 0, nil
}

// TouchAPIKey updates the last-used stamp (observability only). Callers
// treat failures as non-fatal.
func (s *Store) TouchAPIKey(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE api_keys SET last_used_at = ? WHERE id = ?`, rfc3339(now), id)
	if err != nil {
		return fmt.Errorf("touch api key: %w", err)
	}
	return nil
}

func (s *Store) scanAPIKey(row *sql.Row) (APIKey, bool, error) {
	var k APIKey
	var created, expires, lastUsed, revoked sql.NullString
	err := row.Scan(&k.ID, &k.Name, &k.TokenHash, &created, &expires, &lastUsed, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return APIKey{}, false, nil
	}
	if err != nil {
		return APIKey{}, false, fmt.Errorf("read api key: %w", err)
	}
	if err := parseAPIKeyTimes(&k, created, expires, lastUsed, revoked); err != nil {
		return APIKey{}, false, err
	}
	return k, true, nil
}

func scanAPIKeyRows(rows *sql.Rows) (APIKey, error) {
	var k APIKey
	var created, expires, lastUsed, revoked sql.NullString
	if err := rows.Scan(&k.ID, &k.Name, &k.TokenHash, &created, &expires, &lastUsed, &revoked); err != nil {
		return APIKey{}, fmt.Errorf("read api key: %w", err)
	}
	if err := parseAPIKeyTimes(&k, created, expires, lastUsed, revoked); err != nil {
		return APIKey{}, err
	}
	return k, nil
}

func parseAPIKeyTimes(k *APIKey, created, expires, lastUsed, revoked sql.NullString) error {
	createdAt, err := time.Parse(time.RFC3339Nano, created.String)
	if err != nil {
		return fmt.Errorf("read api key: parse created_at: %w", err)
	}
	k.CreatedAt = createdAt
	if k.ExpiresAt, err = nullableTimeFrom(expires, "expires_at"); err != nil {
		return err
	}
	if k.LastUsedAt, err = nullableTimeFrom(lastUsed, "last_used_at"); err != nil {
		return err
	}
	if k.RevokedAt, err = nullableTimeFrom(revoked, "revoked_at"); err != nil {
		return err
	}
	return nil
}

// nullableTimeFrom parses a nullable stored timestamp. A malformed value is
// an error, never a silent nil — a corrupt expires_at must not read as
// "never expires".
func nullableTimeFrom(n sql.NullString, column string) (*time.Time, error) {
	if !n.Valid || n.String == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, n.String)
	if err != nil {
		return nil, fmt.Errorf("read api key: parse %s: %w", column, err)
	}
	return &t, nil
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return rfc3339(*t)
}
