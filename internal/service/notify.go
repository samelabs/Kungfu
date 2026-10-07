package service

// Notify (D7) — the accelerator, never the fact source (kungfu.md
// §8; PRD A21). Registration probes the endpoint with a challenge;
// dispatches carry {account, kind, count} and an HMAC signature, no
// content. The outbox is written in the same transaction as the
// facts; a ticker delivers best-effort; a lost notification is
// always harmless because todo_list recomputes from facts.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"

	"github.com/jackc/pgx/v5"
)

const notifyMaxURL = 2048

// NotifyRegister verifies the endpoint (challenge probe must answer
// 2xx) and stores it as the account's accelerator target.
func NotifyRegister(ctx context.Context, pool *pg.Pool, botID int64, rawURL string) (map[string]any, error) {
	u := strings.TrimSpace(rawURL)
	if len(u) > notifyMaxURL {
		return nil, errors.New(422, "VALIDATION_FAILED", "url too long")
	}
	parsed, err := url.Parse(u)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, errors.New(422, "VALIDATION_FAILED", "url must be a valid https:// endpoint")
	}
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error generating secret")
	}
	secret := hex.EncodeToString(secretBytes)
	challenge := hex.EncodeToString(secretBytes[:16])

	// ownership probe: the endpoint must answer 2xx to the challenge
	probe, err := http.NewRequestWithContext(ctx, http.MethodPost, u,
		strings.NewReader(`{"type":"verification","challenge":"`+challenge+`"}`))
	if err != nil {
		return nil, errors.New(422, "VALIDATION_FAILED", "url is not usable")
	}
	probe.Header.Set("Content-Type", "application/json")
	resp, err := notifyHTTPClient.Do(probe)
	if err != nil || resp.StatusCode < 200 || resp.StatusCode > 299 {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return nil, errors.New(422, "VALIDATION_FAILED",
			"endpoint did not answer 2xx to the verification challenge")
	}
	_ = resp.Body.Close()

	if err := repository.UpsertAccountNotify(ctx, pool, botID, u, secret); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error storing endpoint")
	}
	return map[string]any{"url": u, "verified": true}, nil
}

// NotifyDelete drops the accelerator target; facts and todo_list are
// unaffected.
func NotifyDelete(ctx context.Context, pool *pg.Pool, botID int64) (map[string]any, error) {
	ok, err := repository.DeleteAccountNotify(ctx, pool, botID)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error deleting endpoint")
	}
	return map[string]any{"deleted": ok}, nil
}

var notifyHTTPClient = &http.Client{Timeout: 5 * time.Second}

// DispatchNotifyOutbox delivers queued signals best-effort. One
// pass: claim a batch, POST each to the account's verified endpoint
// with an HMAC signature, mark sent (or count the attempt).
func DispatchNotifyOutbox(ctx context.Context, pool *pg.Pool) (int, error) {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return 0, err
	}
	batch, err := repository.ListUnsentOutbox(ctx, tx, 50)
	if err != nil {
		_ = pg.Rollback(tx)
		return 0, err
	}
	if len(batch) == 0 {
		_ = pg.Rollback(tx)
		return 0, nil
	}
	type delivery struct {
		row  repository.OutboxRow
		url  string
		hmac string
	}
	targets := make([]delivery, 0, len(batch))
	for _, r := range batch {
		u, secret, ok, err := repository.FindAccountNotify(ctx, tx, r.AccountID)
		if err != nil {
			continue
		}
		if !ok {
			// no endpoint: the signal dies here, harmless by design
			if err := repository.MarkOutboxSent(ctx, tx, r.ID, true); err != nil {
				continue
			}
			continue
		}
		targets = append(targets, delivery{row: r, url: u, hmac: secret})
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}

	sent := 0
	for _, d := range targets {
		body, _ := json.Marshal(map[string]any{
			"account": d.row.AccountID, "kind": d.row.Kind, "count": d.row.Count,
		})
		mac := hmac.New(sha256.New, []byte(d.hmac))
		mac.Write(body)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Kungfu-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		resp, err := notifyHTTPClient.Do(req)
		ok := err == nil && resp.StatusCode >= 200 && resp.StatusCode <= 299
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err := repository.MarkOutboxSent(context.Background(), pool, d.row.ID, ok); err == nil && ok {
			sent++
		}
	}
	return sent, nil
}

// enqueueNotify is the in-transaction hook the fact writers call.
func enqueueNotify(ctx context.Context, tx pgx.Tx, accountID int64, kind string, count int) {
	// never fails the parent action: the accelerator must not be
	// able to break the fact it decorates
	_ = repository.InsertNotifyOutbox(ctx, tx, accountID, kind, count)
}
