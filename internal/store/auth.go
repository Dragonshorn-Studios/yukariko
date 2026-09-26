package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AuthPending is one in-flight authorization-code login. The state is the
// single-use key; nonce and PKCE verifier exist only until the callback
// consumes the row.
type AuthPending struct {
	State        string
	Nonce        string
	CodeVerifier string
	RedirectTo   string
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

// WebSession is one authenticated browser session. TokenHash is the SHA-256
// of the cookie token, never the token itself; Claims is a safe display
// subset (subject/groups/email/name), never a raw ID token.
type WebSession struct {
	TokenHash  string
	Subject    string
	Claims     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
}

// CreatePendingAuth records one login attempt. A duplicate state is a
// cryptographic impossibility, not a condition callers handle.
func (s *Store) CreatePendingAuth(ctx context.Context, p AuthPending) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO auth_pending (state, nonce, code_verifier, redirect_to, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		p.State, p.Nonce, p.CodeVerifier, p.RedirectTo, rfc3339(p.CreatedAt), rfc3339(p.ExpiresAt))
	if err != nil {
		return fmt.Errorf("create pending auth: %w", err)
	}
	return nil
}

// ConsumePendingAuth atomically deletes and returns the pending login for
// state. ok=false covers unknown, expired, and already-consumed states —
// single-use is enforced by the DELETE itself, so two concurrent callbacks
// cannot both win.
func (s *Store) ConsumePendingAuth(ctx context.Context, state string, now time.Time) (AuthPending, bool, error) {
	var p AuthPending
	var createdAt, expiresAt string
	err := s.db.QueryRowContext(ctx,
		`DELETE FROM auth_pending
		  WHERE state = ? AND expires_at > ?
		  RETURNING nonce, code_verifier, redirect_to, created_at, expires_at`,
		state, rfc3339(now)).
		Scan(&p.Nonce, &p.CodeVerifier, &p.RedirectTo, &createdAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AuthPending{}, false, nil
	}
	if err != nil {
		return AuthPending{}, false, fmt.Errorf("consume pending auth: %w", err)
	}
	p.State = state
	var err1, err2 error
	p.CreatedAt, err1 = time.Parse(time.RFC3339Nano, createdAt)
	p.ExpiresAt, err2 = time.Parse(time.RFC3339Nano, expiresAt)
	if err1 != nil || err2 != nil {
		return AuthPending{}, false, fmt.Errorf("consume pending auth: parse timestamps: %v / %v", err1, err2)
	}
	return p, true, nil
}

// CreateWebSession stores one session row.
func (s *Store) CreateWebSession(ctx context.Context, sess WebSession) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO web_sessions (token_hash, subject, claims, created_at, expires_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		sess.TokenHash, sess.Subject, sess.Claims, rfc3339(sess.CreatedAt), rfc3339(sess.ExpiresAt), rfc3339(sess.LastSeenAt))
	if err != nil {
		return fmt.Errorf("create web session: %w", err)
	}
	return nil
}

// WebSession returns the live session for a token hash. Expired rows are
// reported as absent and swept separately.
func (s *Store) WebSession(ctx context.Context, tokenHash string, now time.Time) (WebSession, bool, error) {
	var sess WebSession
	var createdAt, expiresAt, lastSeenAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT subject, claims, created_at, expires_at, last_seen_at
		   FROM web_sessions WHERE token_hash = ? AND expires_at > ?`,
		tokenHash, rfc3339(now)).
		Scan(&sess.Subject, &sess.Claims, &createdAt, &expiresAt, &lastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WebSession{}, false, nil
	}
	if err != nil {
		return WebSession{}, false, fmt.Errorf("read web session: %w", err)
	}
	sess.TokenHash = tokenHash
	var err1, err2, err3 error
	sess.CreatedAt, err1 = time.Parse(time.RFC3339Nano, createdAt)
	sess.ExpiresAt, err2 = time.Parse(time.RFC3339Nano, expiresAt)
	sess.LastSeenAt, err3 = time.Parse(time.RFC3339Nano, lastSeenAt)
	if err1 != nil || err2 != nil || err3 != nil {
		return WebSession{}, false, fmt.Errorf("read web session: parse timestamps: %v / %v / %v", err1, err2, err3)
	}
	return sess, true, nil
}

// TouchWebSession updates the last-seen stamp (observability only; expiry is
// absolute, not idle-based).
func (s *Store) TouchWebSession(ctx context.Context, tokenHash string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE web_sessions SET last_seen_at = ? WHERE token_hash = ?`,
		rfc3339(now), tokenHash)
	if err != nil {
		return fmt.Errorf("touch web session: %w", err)
	}
	return nil
}

// DeleteWebSession removes one session (logout). An unknown hash is not an
// error.
func (s *Store) DeleteWebSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM web_sessions WHERE token_hash = ?`, tokenHash)
	if err != nil {
		return fmt.Errorf("delete web session: %w", err)
	}
	return nil
}

// SweepAuth drops expired sessions and pending logins. It runs at startup
// and opportunistically on login; the rows are tiny and bounded by TTL.
func (s *Store) SweepAuth(ctx context.Context, now time.Time) (int64, error) {
	cutoff := rfc3339(now)
	n1, err := s.deleteWhere(ctx, `DELETE FROM web_sessions WHERE expires_at <= ?`, cutoff, "sweep web sessions")
	if err != nil {
		return 0, err
	}
	n2, err := s.deleteWhere(ctx, `DELETE FROM auth_pending WHERE expires_at <= ?`, cutoff, "sweep pending auth")
	if err != nil {
		return n1, err
	}
	return n1 + n2, nil
}

func (s *Store) deleteWhere(ctx context.Context, query, arg, what string) (int64, error) {
	res, err := s.db.ExecContext(ctx, query, arg)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", what, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("%s: %w", what, err)
	}
	return n, nil
}
