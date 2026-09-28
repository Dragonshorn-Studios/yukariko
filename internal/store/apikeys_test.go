package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAPIKeyLifecycle(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	hash := "aaaa1111bbbb2222cccc3333dddd4444"
	k := APIKey{
		Name:      "amadeus",
		TokenHash: hash,
		CreatedAt: now,
		ExpiresAt: nil,
	}
	if err := s.CreateAPIKey(ctx, k); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	// Lookup by hash returns the row and assigns an id.
	got, ok, err := s.APIKeyByHash(ctx, hash)
	if err != nil || !ok {
		t.Fatalf("APIKeyByHash = %v, %v; want found", got, err)
	}
	if got.Name != "amadeus" || got.ID == "" {
		t.Errorf("APIKeyByHash = %+v; want name amadeus with id", got)
	}
	if got.Active(now) != true {
		t.Errorf("key without expiry should be active")
	}

	// Same key by name and id.
	byName, ok, err := s.APIKeyByName(ctx, "amadeus")
	if err != nil || !ok || byName.ID != got.ID {
		t.Fatalf("APIKeyByName = %v, %v, %v; want same id", byName, ok, err)
	}
	byID, ok, err := s.APIKeyByID(ctx, got.ID)
	if err != nil || !ok || byID.TokenHash != hash {
		t.Fatalf("APIKeyByID = %v, %v, %v; want same hash", byID, ok, err)
	}

	// Unknown hash: absent, no error.
	if _, ok, err := s.APIKeyByHash(ctx, "nope"); ok || err != nil {
		t.Errorf("APIKeyByHash(unknown) = ok=%v err=%v; want false, nil", ok, err)
	}
}

func TestAPIKeyActiveStates(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)

	cases := []struct {
		name string
		key  APIKey
		want bool
	}{
		{"no expiry", APIKey{}, true},
		{"future expiry", APIKey{ExpiresAt: &future}, true},
		{"expiring exactly now", APIKey{ExpiresAt: &now}, false},
		{"expired", APIKey{ExpiresAt: &past}, false},
		{"revoked", APIKey{RevokedAt: &now}, false},
		{"revoked beats future expiry", APIKey{ExpiresAt: &future, RevokedAt: &now}, false},
	}
	for _, c := range cases {
		if got := c.key.Active(now); got != c.want {
			t.Errorf("%s: Active = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAPIKeyExpiryAtLookup(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	expires := now.Add(10 * time.Minute)
	if err := s.CreateAPIKey(ctx, APIKey{Name: "temp", TokenHash: "eeee0000", CreatedAt: now, ExpiresAt: &expires}); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	got, ok, err := s.APIKeyByHash(ctx, "eeee0000")
	if err != nil || !ok {
		t.Fatalf("APIKeyByHash before expiry: ok=%v err=%v", ok, err)
	}
	if !got.Active(now.Add(9 * time.Minute)) {
		t.Error("key should be active 9 minutes in")
	}
	if got.Active(now.Add(11 * time.Minute)) {
		t.Error("key should be expired 11 minutes in")
	}
}

func TestAPIKeyRevoke(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	if err := s.CreateAPIKey(ctx, APIKey{Name: "amadeus", TokenHash: "aaaa1111", CreatedAt: now}); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	got, ok, err := s.APIKeyByName(ctx, "amadeus")
	if err != nil || !ok {
		t.Fatalf("APIKeyByName: ok=%v err=%v", ok, err)
	}

	// Revoke by the resolved id.
	ok, err = s.RevokeAPIKey(ctx, got.ID, now)
	if err != nil || !ok {
		t.Fatalf("RevokeAPIKey(id) = %v, %v; want true", ok, err)
	}
	after, ok, err := s.APIKeyByName(ctx, "amadeus")
	if err != nil || !ok {
		t.Fatalf("APIKeyByName after revoke: ok=%v err=%v", ok, err)
	}
	if after.Active(now) {
		t.Error("revoked key must not be active")
	}
	if after.RevokedAt == nil {
		t.Error("revoked_at should be recorded")
	}

	// Revoke again is a no-op (already revoked).
	ok, err = s.RevokeAPIKey(ctx, got.ID, now)
	if err != nil || ok {
		t.Errorf("RevokeAPIKey(already revoked) = %v, %v; want false, nil", ok, err)
	}

	// Unknown id: false, nil.
	ok, err = s.RevokeAPIKey(ctx, "ghost", now)
	if err != nil || ok {
		t.Errorf("RevokeAPIKey(unknown) = %v, %v; want false, nil", ok, err)
	}
}

// Revocation is keyed on the id alone: a second key whose NAME equals the
// first key's id must never be swept up by the same revoke.
func TestAPIKeyRevokeDoesNotTouchNameCollidingKey(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	if err := s.CreateAPIKey(ctx, APIKey{Name: "target", TokenHash: "hash-target", CreatedAt: now}); err != nil {
		t.Fatalf("CreateAPIKey(target): %v", err)
	}
	target, ok, err := s.APIKeyByName(ctx, "target")
	if err != nil || !ok {
		t.Fatalf("APIKeyByName(target): ok=%v err=%v", ok, err)
	}
	if err := s.CreateAPIKey(ctx, APIKey{Name: target.ID, TokenHash: "hash-collide", CreatedAt: now}); err != nil {
		t.Fatalf("CreateAPIKey(name=target.id): %v", err)
	}

	if ok, err := s.RevokeAPIKey(ctx, target.ID, now); err != nil || !ok {
		t.Fatalf("RevokeAPIKey(target.id) = %v, %v; want true", ok, err)
	}

	collide, ok, err := s.APIKeyByName(ctx, target.ID)
	if err != nil || !ok {
		t.Fatalf("APIKeyByName(collision): ok=%v err=%v", ok, err)
	}
	if collide.RevokedAt != nil {
		t.Error("revoking a key by id must not touch another key named after that id")
	}
}

// A row corrupted outside Yukariko (foreign write, tampering) must error
// with the typed sentinel — never read as "never expires" or folded into a
// generic store outage.
func TestAPIKeyCorruptRowIsTyped(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	if err := s.CreateAPIKey(ctx, APIKey{Name: "amadeus", TokenHash: "hash-x", CreatedAt: now}); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE api_keys SET expires_at = 'not-a-timestamp'`); err != nil {
		t.Fatalf("corrupt row: %v", err)
	}
	_, _, err := s.APIKeyByHash(ctx, "hash-x")
	if !errors.Is(err, ErrCorruptAPIKey) {
		t.Fatalf("APIKeyByHash(corrupt) err = %v; want ErrCorruptAPIKey", err)
	}
}

func TestAPIKeyListAndTouch(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	for i, name := range []string{"zeta", "alpha"} {
		created := now.Add(time.Duration(i) * time.Minute)
		if err := s.CreateAPIKey(ctx, APIKey{Name: name, TokenHash: name + "-hash", CreatedAt: created}); err != nil {
			t.Fatalf("CreateAPIKey(%s): %v", name, err)
		}
	}

	keys, err := s.ListAPIKeys(ctx)
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("ListAPIKeys len = %d, want 2", len(keys))
	}
	if keys[0].Name != "zeta" || keys[1].Name != "alpha" {
		t.Errorf("ListAPIKeys order = [%s, %s]; want [zeta alpha] (created_at order)", keys[0].Name, keys[1].Name)
	}
	for i, k := range keys {
		if k.TokenHash != []string{"zeta-hash", "alpha-hash"}[i] {
			t.Errorf("ListAPIKeys[%d].TokenHash = %q", i, k.TokenHash)
		}
	}

	// Touch moves last_used_at.
	if err := s.TouchAPIKey(ctx, keys[0].ID, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("TouchAPIKey: %v", err)
	}
	got, ok, err := s.APIKeyByID(ctx, keys[0].ID)
	if err != nil || !ok {
		t.Fatalf("APIKeyByID after touch: ok=%v err=%v", ok, err)
	}
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(now.Add(2*time.Minute)) {
		t.Errorf("LastUsedAt = %v, want %v", got.LastUsedAt, now.Add(2*time.Minute))
	}

	// Touch on an unknown id is not an error (best-effort by contract).
	if err := s.TouchAPIKey(ctx, "missing", now); err != nil {
		t.Errorf("TouchAPIKey(unknown) = %v; want nil", err)
	}
}
