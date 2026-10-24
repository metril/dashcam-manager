// auth.go is the data-access layer for schema v11's optional OIDC login
// (internal/auth): server-side sessions behind the dcm_session cookie and
// the short-lived single-use login states bridging /auth/login and
// /auth/callback. Keys are hex sha256 hashes, never the raw cookie/state.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AuthSession is one signed-in browser session. Groups is the JSON array
// of group names from the ID token, kept so the gate can re-authorize
// against the current allow-lists on every request.
type AuthSession struct {
	TokenHash     string
	Subject       string
	Email         string
	Name          string
	EmailVerified bool
	GroupsJSON    string
	IDToken       string // for RP-initiated logout's id_token_hint
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

// AuthLoginState is the per-login state recorded at /auth/login and
// consumed (once) at /auth/callback.
type AuthLoginState struct {
	StateHash    string
	Nonce        string
	PKCEVerifier string
	NextPath     string
	ExpiresAt    time.Time
	// Purpose is "login" (default) or "oidc_test", the settings UI's
	// test-before-save login.
	Purpose string
}

// AuthSessionCreate inserts s.
func (d *DB) AuthSessionCreate(ctx context.Context, s AuthSession) error {
	groups := s.GroupsJSON
	if groups == "" {
		groups = "[]"
	}
	_, err := d.Write().ExecContext(ctx,
		`INSERT INTO auth_sessions (token_hash, subject, email, name, email_verified, groups_json, id_token, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.TokenHash, s.Subject, s.Email, s.Name, boolInt(s.EmailVerified), groups, s.IDToken,
		s.CreatedAt.Unix(), s.ExpiresAt.Unix())
	if err != nil {
		return fmt.Errorf("db: create auth session: %w", err)
	}
	return nil
}

// AuthSessionGet returns the session for tokenHash, ignoring expired rows.
func (d *DB) AuthSessionGet(ctx context.Context, tokenHash string, now time.Time) (AuthSession, bool, error) {
	var s AuthSession
	var verified int
	var created, expires int64
	err := d.Read().QueryRowContext(ctx,
		`SELECT token_hash, subject, email, name, email_verified, groups_json, id_token, created_at, expires_at
		 FROM auth_sessions WHERE token_hash = ? AND expires_at > ?`,
		tokenHash, now.Unix(),
	).Scan(&s.TokenHash, &s.Subject, &s.Email, &s.Name, &verified, &s.GroupsJSON, &s.IDToken, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return AuthSession{}, false, nil
	}
	if err != nil {
		return AuthSession{}, false, fmt.Errorf("db: get auth session: %w", err)
	}
	s.EmailVerified = verified != 0
	s.CreatedAt = time.Unix(created, 0)
	s.ExpiresAt = time.Unix(expires, 0)
	return s, true, nil
}

// AuthSessionDelete removes a session (logout / no longer authorized).
func (d *DB) AuthSessionDelete(ctx context.Context, tokenHash string) error {
	if _, err := d.Write().ExecContext(ctx, `DELETE FROM auth_sessions WHERE token_hash = ?`, tokenHash); err != nil {
		return fmt.Errorf("db: delete auth session: %w", err)
	}
	return nil
}

// AuthLoginStatePut records st.
func (d *DB) AuthLoginStatePut(ctx context.Context, st AuthLoginState) error {
	purpose := st.Purpose
	if purpose == "" {
		purpose = "login"
	}
	_, err := d.Write().ExecContext(ctx,
		`INSERT INTO auth_login_states (state_hash, nonce, pkce_verifier, next_path, expires_at, purpose) VALUES (?, ?, ?, ?, ?, ?)`,
		st.StateHash, st.Nonce, st.PKCEVerifier, st.NextPath, st.ExpiresAt.Unix(), purpose)
	if err != nil {
		return fmt.Errorf("db: put auth login state: %w", err)
	}
	return nil
}

// AuthLoginStateTake atomically deletes and returns the unexpired state, so
// a replayed callback finds nothing.
func (d *DB) AuthLoginStateTake(ctx context.Context, stateHash string, now time.Time) (AuthLoginState, bool, error) {
	var st AuthLoginState
	var expires int64
	err := d.Write().QueryRowContext(ctx,
		`DELETE FROM auth_login_states WHERE state_hash = ? RETURNING state_hash, nonce, pkce_verifier, next_path, expires_at, purpose`,
		stateHash,
	).Scan(&st.StateHash, &st.Nonce, &st.PKCEVerifier, &st.NextPath, &expires, &st.Purpose)
	if errors.Is(err, sql.ErrNoRows) {
		return AuthLoginState{}, false, nil
	}
	if err != nil {
		return AuthLoginState{}, false, fmt.Errorf("db: take auth login state: %w", err)
	}
	st.ExpiresAt = time.Unix(expires, 0)
	if !st.ExpiresAt.After(now) {
		return AuthLoginState{}, false, nil
	}
	return st, true, nil
}

// AuthPurgeExpired deletes expired sessions and login states, returning the
// number of rows removed.
func (d *DB) AuthPurgeExpired(ctx context.Context, now time.Time) (int64, error) {
	var total int64
	for _, q := range []string{
		`DELETE FROM auth_sessions WHERE expires_at <= ?`,
		`DELETE FROM auth_login_states WHERE expires_at <= ?`,
		`DELETE FROM oidc_pending WHERE expires_at <= ?`,
	} {
		res, err := d.Write().ExecContext(ctx, q, now.Unix())
		if err != nil {
			return total, fmt.Errorf("db: purge auth: %w", err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
