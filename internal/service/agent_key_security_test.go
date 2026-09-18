package service

// S6.1: Agent API key at-rest hardening — registration/reset/metadata
// proofs against real PostgreSQL through the production paths.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/auth"
	apperr "kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/ratelimit"
	"kungfu.md/internal/repository"
)

func s61Pool(t *testing.T) *pg.Pool {
	t.Helper()
	url := strings.TrimSpace(os.Getenv("KF_TEST_DATABASE_URL"))
	if url == "" {
		t.Skip("KF_TEST_DATABASE_URL not set")
	}
	pool, err := pg.NewPool(url)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestS61RegisterStoresHashOnlyAndReturnsRawOnce: registration
// returns the raw key once; the DB holds only the SHA-256 digest +
// last4; there is no plaintext credential column at all.
func TestS61RegisterStoresHashOnlyAndReturnsRawOnce(t *testing.T) {
	pool := s61Pool(t)
	suffix := fmt.Sprint(time.Now().UnixNano())
	name := "s61reg_" + suffix

	res, err := Register(context.Background(), pool, name, "password123", "127.0.0.1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_transactions WHERE bot_id=(SELECT id FROM tb_bots WHERE bot_name=$1)`, name)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_logs WHERE bot_id=(SELECT id FROM tb_bots WHERE bot_name=$1)`, name)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE bot_name=$1`, name)
	})

	if !strings.HasPrefix(res.Key, "kf_live_") || len(res.Key) != 72 {
		t.Fatalf("registration did not return a raw key: %q", res.Key)
	}

	var (
		hashBytes  []byte
		last4      string
		plainCount int
	)
	err = pool.QueryRow(context.Background(),
		`SELECT api_key_hash, api_key_last4 FROM tb_bots WHERE bot_name=$1`, name).
		Scan(&hashBytes, &last4)
	if err != nil {
		t.Fatalf("read stored key material: %v", err)
	}
	if string(hashBytes) != string(auth.HashAgentKey(res.Key)) {
		t.Fatal("stored digest != SHA-256 of returned raw key")
	}
	if len(hashBytes) != 32 {
		t.Fatalf("digest len = %d", len(hashBytes))
	}
	if last4 != res.Key[len(res.Key)-4:] {
		t.Fatalf("last4 = %q", last4)
	}
	if strings.Contains(string(hashBytes), "kf_live_") {
		t.Fatal("digest contains plaintext")
	}
	// the plaintext column is GONE
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM information_schema.columns WHERE table_name='tb_bots' AND column_name='api_key'`).
		Scan(&plainCount)
	if plainCount != 0 {
		t.Fatal("plaintext api_key column still exists")
	}

	// the registered key authenticates via the production mechanism
	bot, err := repository.FindActiveBotByAPIKeyHash(context.Background(), pool, auth.HashAgentKey(res.Key))
	if err != nil || bot == nil {
		t.Fatalf("new key does not authenticate: %v", err)
	}
}

// TestS61OwnerKeyReadNeverReturnsRawKey: /api/key metadata projection
// (service layer) exposes masked metadata only — no key, no hash.
func TestS61OwnerKeyReadNeverReturnsRawKey(t *testing.T) {
	pool := s61Pool(t)
	suffix := fmt.Sprint(time.Now().UnixNano())
	name := "s61key_" + suffix
	res, err := Register(context.Background(), pool, name, "password123", "127.0.0.1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_transactions WHERE bot_id=(SELECT id FROM tb_bots WHERE bot_name=$1)`, name)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_logs WHERE bot_id=(SELECT id FROM tb_bots WHERE bot_name=$1)`, name)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE bot_name=$1`, name)
	})

	var botID int64
	_ = pool.QueryRow(context.Background(), `SELECT id FROM tb_bots WHERE bot_name=$1`, name).Scan(&botID)

	out, err := CurrentOwnerKey(context.Background(), pool, botID)
	if err != nil {
		t.Fatalf("CurrentOwnerKey: %v", err)
	}
	if _, exists := out["key"]; exists {
		t.Fatal("metadata projection returned a raw key field")
	}
	blob, _ := json.Marshal(out)
	if strings.Contains(string(blob), res.Key) {
		t.Fatal("raw key leaked through metadata projection")
	}
	if strings.Contains(string(blob), "api_key_hash") {
		t.Fatal("hash field leaked through metadata projection")
	}
	masked, _ := out["key_masked"].(string)
	if !strings.HasPrefix(masked, "kf_live_****") || !strings.HasSuffix(masked, res.Key[len(res.Key)-4:]) {
		t.Fatalf("key_masked = %q", masked)
	}
}

// TestS61ResetRequiresCurrentRawKey: missing / malformed / wrong
// current key fails closed with ZERO key mutation.
func TestS61ResetRequiresCurrentRawKey(t *testing.T) {
	pool := s61Pool(t)
	suffix := fmt.Sprint(time.Now().UnixNano())
	name := "s61rst_" + suffix
	res, err := Register(context.Background(), pool, name, "password123", "127.0.0.1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_transactions WHERE bot_id=(SELECT id FROM tb_bots WHERE bot_name=$1)`, name)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_logs WHERE bot_id=(SELECT id FROM tb_bots WHERE bot_name=$1)`, name)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE bot_name=$1`, name)
	})
	var botID int64
	_ = pool.QueryRow(context.Background(), `SELECT id FROM tb_bots WHERE bot_name=$1`, name).Scan(&botID)
	limiter := ratelimit.NewLimiter(map[string]ratelimit.Config{})

	cases := []struct {
		label string
		key   string
	}{
		{"missing", "  "},
		{"malformed", "kf_live_short"},
		{"wrong", auth.GenerateKey()},
	}
	for _, tc := range cases {
		_, err := ResetKey(context.Background(), pool, limiter, botID, tc.key)
		ae, ok := apperr.IsAppError(err)
		if !ok {
			t.Fatalf("%s: expected AppError, got %v", tc.label, err)
		}
		if ae.HTTPCode != 400 && ae.HTTPCode != 401 {
			t.Fatalf("%s: status = %d", tc.label, ae.HTTPCode)
		}
		// zero mutation: original key still authenticates
		bot, err2 := repository.FindActiveBotByAPIKeyHash(context.Background(), pool, auth.HashAgentKey(res.Key))
		if err2 != nil || bot == nil {
			t.Fatalf("%s: original key was invalidated by a failed reset", tc.label)
		}
	}
}

// TestS61ResetRotatesHashAndInvalidatesOldKey: successful reset
// returns the new raw key once, rotates digest/last4/issued-time, the
// old key immediately fails, the new key immediately authenticates.
func TestS61ResetRotatesHashAndInvalidatesOldKey(t *testing.T) {
	pool := s61Pool(t)
	suffix := fmt.Sprint(time.Now().UnixNano())
	name := "s61rot_" + suffix
	res, err := Register(context.Background(), pool, name, "password123", "127.0.0.1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_transactions WHERE bot_id=(SELECT id FROM tb_bots WHERE bot_name=$1)`, name)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_logs WHERE bot_id=(SELECT id FROM tb_bots WHERE bot_name=$1)`, name)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE bot_name=$1`, name)
	})
	var botID int64
	_ = pool.QueryRow(context.Background(), `SELECT id FROM tb_bots WHERE bot_name=$1`, name).Scan(&botID)
	limiter := ratelimit.NewLimiter(map[string]ratelimit.Config{})

	var issuedBefore string
	_ = pool.QueryRow(context.Background(), `SELECT to_char(key_issued_at,'YYYY-MM-DD HH24:MI:SS') FROM tb_bots WHERE id=$1`, botID).Scan(&issuedBefore)

	out, err := ResetKey(context.Background(), pool, limiter, botID, res.Key)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	newKey, _ := out["new_key"].(string)
	if !strings.HasPrefix(newKey, "kf_live_") || newKey == res.Key {
		t.Fatalf("reset did not return a new raw key")
	}

	// old key immediately invalid
	if bot, err := repository.FindActiveBotByAPIKeyHash(context.Background(), pool, auth.HashAgentKey(res.Key)); err != nil || bot != nil {
		t.Fatal("old key still authenticates after reset")
	}
	// new key immediately valid
	if bot, err := repository.FindActiveBotByAPIKeyHash(context.Background(), pool, auth.HashAgentKey(newKey)); err != nil || bot == nil {
		t.Fatal("new key does not authenticate")
	}
	// digest/last4/issued rotated
	var (
		hashBytes []byte
		last4     string
		issuedAt  string
	)
	_ = pool.QueryRow(context.Background(),
		`SELECT api_key_hash, api_key_last4, to_char(key_issued_at,'YYYY-MM-DD HH24:MI:SS') FROM tb_bots WHERE id=$1`, botID).
		Scan(&hashBytes, &last4, &issuedAt)
	if string(hashBytes) != string(auth.HashAgentKey(newKey)) {
		t.Fatal("stored digest is not the new key's digest")
	}
	if last4 != newKey[len(newKey)-4:] {
		t.Fatalf("last4 = %q", last4)
	}
	if issuedAt < issuedBefore {
		t.Fatalf("key_issued_at went backwards: %s < %s", issuedAt, issuedBefore)
	}
}
