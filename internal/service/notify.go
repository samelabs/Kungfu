package service

// Notify (D7) — the accelerator, never the fact source (kungfu.md
// §8; PRD A21). Registration probes the endpoint with a challenge;
// dispatches carry {account, kind, count} and an HMAC signature, no
// content. The outbox is written in the same transaction as the
// facts; a ticker delivers best-effort; a lost notification is
// always harmless because todo_list recomputes from facts.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strings"

	"kungfu.md/internal/delivery"
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

	// ownership probe over the hardened outbound path (external audit
	// P1-1): private/loopback/link-local targets are refused, redirects
	// forbidden — the same authority the Task receiver deliveries use.
	// The endpoint must answer 2xx AND echo the challenge in its body
	// (P1-2): a bare 2xx proves nothing about control of the endpoint.
	probeBody := []byte(`{"type":"verification","challenge":"` + challenge + `"}`)
	probe := delivery.PostJSON(ctx, u, probeBody, map[string]string{"Content-Type": "application/json"},
		delivery.ErrorConfig{})
	if probe.ResponseCode == nil || *probe.ResponseCode < 200 || *probe.ResponseCode > 299 {
		return nil, errors.New(422, "VALIDATION_FAILED",
			"endpoint did not answer 2xx to the verification challenge (SSRF policy may have refused it)")
	}
	echoed := probe.ResponseBody != nil && strings.Contains(*probe.ResponseBody, challenge)
	if !echoed {
		return nil, errors.New(422, "VALIDATION_FAILED",
			"endpoint answered 2xx but did not echo the challenge — verification incomplete")
	}

	if err := repository.UpsertAccountNotify(ctx, pool, botID, u, secret); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error storing endpoint")
	}
	// the signing secret is disclosed ONCE, here (P1-2 closure): the
	// receiver verifies dispatches as sha256 HMAC over the raw body
	// with this secret, sent as X-Kungfu-Signature
	return map[string]any{"url": u, "verified": true, "secret": secret}, nil
}

// NotifyTest enqueues a kind="test" signal to the account's registered
// endpoint so the whole chain (challenge → signature → wake) can be
// verified end to end without a collaborator having to create a real
// obligation. The signal is a doorbell like any other: no content,
// dispatched by the same best-effort ticker.
func NotifyTest(ctx context.Context, pool *pg.Pool, botID int64) (map[string]any, error) {
	_, _, ok, err := repository.FindAccountNotify(ctx, pool, botID)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error checking endpoint")
	}
	if !ok {
		return nil, errors.New(422, "VALIDATION_FAILED", "no endpoint registered; notify_register first")
	}
	if err := repository.InsertNotifyOutbox(ctx, pool, botID, "test", 0); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error queueing test signal")
	}
	return map[string]any{
		"queued": true, "kind": "test",
		"dispatched_within": 30,
		"note":              "watch your endpoint for a signed {account,kind,count} POST; a lost test signal is harmless — retry",
	}, nil
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

// DispatchNotifyOutbox delivers queued signals best-effort. One
// pass: drop abandoned rows, claim a batch, POST each to the
// account's verified endpoint with an HMAC signature, delete the
// delivered ones (or count the attempt).
func DispatchNotifyOutbox(ctx context.Context, pool *pg.Pool) (int, error) {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return 0, err
	}
	// bounded recycling first: rows whose attempts ran out leave
	if err := repository.DeleteAbandonedOutbox(ctx, tx); err != nil {
		_ = pg.Rollback(tx)
		return 0, err
	}
	batch, err := repository.ListUnsentOutbox(ctx, tx, 50)
	if err != nil {
		_ = pg.Rollback(tx)
		return 0, err
	}
	if len(batch) == 0 {
		// nothing to deliver: COMMIT — the abandoned-rows cleanup
		// above stands (rolling back would resurrect them)
		return 0, tx.Commit(ctx)
	}
	type notifyTarget struct {
		row  repository.OutboxRow
		url  string
		hmac string
	}
	targets := make([]notifyTarget, 0, len(batch))
	for _, r := range batch {
		u, secret, ok, err := repository.FindAccountNotify(ctx, tx, r.AccountID)
		if err != nil {
			continue
		}
		if !ok {
			// no endpoint (unregistered since the write, or a row from
			// before the write gate): the signal dies and goes here
			_ = repository.DeleteOutboxRow(ctx, tx, r.ID)
			continue
		}
		targets = append(targets, notifyTarget{row: r, url: u, hmac: secret})
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
		res := delivery.PostJSON(ctx, d.url, body, map[string]string{
			"Content-Type":       "application/json",
			"X-Kungfu-Signature": "sha256=" + hex.EncodeToString(mac.Sum(nil)),
		}, delivery.ErrorConfig{})
		ok := res.ResponseCode != nil && *res.ResponseCode >= 200 && *res.ResponseCode <= 299
		if ok {
			// delivered: the completed signal leaves nothing behind
			if err := repository.DeleteOutboxRow(context.Background(), pool, d.row.ID); err == nil {
				sent++
			}
			continue
		}
		_ = repository.MarkOutboxSent(context.Background(), pool, d.row.ID, false)
	}
	return sent, nil
}

// enqueueNotify is the in-transaction hook the fact writers call.
func enqueueNotify(ctx context.Context, tx pgx.Tx, accountID int64, kind string, count int) {
	// never fails the parent action: the accelerator must not be able
	// to break the fact it decorates; the write itself is gated on a
	// registered endpoint (repository.InsertNotifyOutboxIfRegistered)
	repository.InsertNotifyOutboxIfRegistered(ctx, tx, accountID, kind, count)
}
