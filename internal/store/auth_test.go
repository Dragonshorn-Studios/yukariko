package store

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPendingAuthSingleUse(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	p := AuthPending{
		State: "state-1", Nonce: "nonce-1", CodeVerifier: "verifier-1",
		RedirectTo: "/ui", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := s.CreatePendingAuth(ctx, p); err != nil {
		t.Fatalf("CreatePendingAuth: %v", err)
	}
	got, ok, err := s.ConsumePendingAuth(ctx, p.State, now.Add(time.Second))
	if err != nil || !ok {
		t.Fatalf("ConsumePendingAuth = ok:%v err:%v, want ok:true err:nil", ok, err)
	}
	if got.Nonce != p.Nonce || got.CodeVerifier != p.CodeVerifier || got.RedirectTo != p.RedirectTo {
		t.Errorf("consumed row = %+v, want %+v", got, p)
	}
	// Second consume of the same state must fail: single-use by construction.
	if _, ok, err := s.ConsumePendingAuth(ctx, p.State, now.Add(time.Second)); err != nil || ok {
		t.Errorf("replay consume = ok:%v err:%v, want ok:false err:nil", ok, err)
	}
	// Unknown state is absent, not an error.
	if _, ok, err := s.ConsumePendingAuth(ctx, "never-seen", now.Add(time.Second)); err != nil || ok {
		t.Errorf("unknown consume = ok:%v err:%v, want ok:false err:nil", ok, err)
	}
}

// The single-use guarantee must hold under concurrency: two tabs racing
// the same callback URL (or an attacker replaying it) cannot both consume
// the state. The atomic DELETE ... RETURNING decides, not the callers.
func TestConcurrentConsumePendingAuthHasOneWinner(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.CreatePendingAuth(ctx, AuthPending{
		State: "race", Nonce: "n", CodeVerifier: "v", RedirectTo: "/ui",
		CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}); err != nil {
		t.Fatalf("CreatePendingAuth: %v", err)
	}
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, err := s.ConsumePendingAuth(ctx, "race", now.Add(time.Second))
			if err != nil {
				t.Errorf("concurrent consume: %v", err)
				return
			}
			if ok {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := winners.Load(); got != 1 {
		t.Errorf("concurrent consume produced %d winners, want exactly 1", got)
	}
}

func TestPendingAuthExpiry(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	p := AuthPending{
		State: "state-2", Nonce: "n", CodeVerifier: "v", RedirectTo: "/ui",
		CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := s.CreatePendingAuth(ctx, p); err != nil {
		t.Fatalf("CreatePendingAuth: %v", err)
	}
	// After the expiry the row behaves as unknown.
	if _, ok, err := s.ConsumePendingAuth(ctx, p.State, now.Add(11*time.Minute)); err != nil || ok {
		t.Errorf("expired consume = ok:%v err:%v, want ok:false err:nil", ok, err)
	}
}

func TestWebSessionLifecycle(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	sess := WebSession{
		TokenHash: "hash-1", Subject: "ada", Claims: `{"subject":"ada"}`,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now,
	}
	if err := s.CreateWebSession(ctx, sess); err != nil {
		t.Fatalf("CreateWebSession: %v", err)
	}
	got, ok, err := s.WebSession(ctx, sess.TokenHash, now.Add(time.Minute))
	if err != nil || !ok {
		t.Fatalf("WebSession = ok:%v err:%v, want ok:true err:nil", ok, err)
	}
	if got.Subject != "ada" || got.Claims != sess.Claims {
		t.Errorf("session = %+v, want subject ada and claims preserved", got)
	}
	if err := s.TouchWebSession(ctx, sess.TokenHash, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("TouchWebSession: %v", err)
	}
	got, _, _ = s.WebSession(ctx, sess.TokenHash, now.Add(3*time.Minute))
	if !got.LastSeenAt.After(got.CreatedAt) {
		t.Errorf("last_seen %v not after created %v", got.LastSeenAt, got.CreatedAt)
	}
	// Expired sessions are absent.
	if _, ok, err := s.WebSession(ctx, sess.TokenHash, now.Add(2*time.Hour)); err != nil || ok {
		t.Errorf("expired lookup = ok:%v err:%v, want ok:false err:nil", ok, err)
	}
	// Logout deletes; a second delete is fine.
	if err := s.DeleteWebSession(ctx, sess.TokenHash); err != nil {
		t.Fatalf("DeleteWebSession: %v", err)
	}
	if err := s.DeleteWebSession(ctx, sess.TokenHash); err != nil {
		t.Fatalf("idempotent DeleteWebSession: %v", err)
	}
	if _, ok, _ := s.WebSession(ctx, sess.TokenHash, now); ok {
		t.Error("deleted session still resolves")
	}
}

func TestSweepAuthDropsExpiredOnly(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	live := WebSession{TokenHash: "live", Subject: "a", CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now}
	dead := WebSession{TokenHash: "dead", Subject: "b", CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour), LastSeenAt: now.Add(-2 * time.Hour)}
	for _, sess := range []WebSession{live, dead} {
		if err := s.CreateWebSession(ctx, sess); err != nil {
			t.Fatalf("CreateWebSession(%s): %v", sess.TokenHash, err)
		}
	}
	pending := AuthPending{State: "old", Nonce: "n", CodeVerifier: "v", RedirectTo: "/ui", CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute)}
	if err := s.CreatePendingAuth(ctx, pending); err != nil {
		t.Fatalf("CreatePendingAuth: %v", err)
	}

	n, err := s.SweepAuth(ctx, now)
	if err != nil {
		t.Fatalf("SweepAuth: %v", err)
	}
	if n != 2 {
		t.Errorf("SweepAuth removed %d rows, want 2 (one session, one pending)", n)
	}
	if _, ok, _ := s.WebSession(ctx, live.TokenHash, now); !ok {
		t.Error("sweep removed a live session")
	}
}

func TestMigration3Version(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	v, err := s.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v < 3 {
		t.Errorf("schema version = %d, want at least 3 (auth tables present)", v)
	}
}
