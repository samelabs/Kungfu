package service

import (
	"context"
	stderrors "errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"kungfu.md/internal/auth"
	"kungfu.md/internal/credits"
	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/security"
)

// SignupGrant is the credit amount granted to a freshly registered bot.
const SignupGrant = 66.0

// RegistrationResult is the return value of Register.
type RegistrationResult struct {
	BotName string `json:"bot_name"`
	Key     string `json:"key"`
	Balance int    `json:"balance"`
	Message string `json:"message"`
}

// Register creates a new bot account.
// nameExistsProbe is the seam the A6 regression tests use to inject a
// failing name-existence query. Production always uses repository.BotNameExists.
var nameExistsProbe = repository.BotNameExists

// Register creates a new bot account.
func Register(ctx context.Context, pool *pg.Pool, name, password, ip string) (*RegistrationResult, error) {
	name = strings.TrimSpace(name)

	// Validate bot name (6-32 chars)
	valid, errs := auth.ValidateBotName(name)
	if !valid {
		return nil, errors.New(400, "INVALID_NAME", errs[0])
	}

	// Validate password
	valid, errs = auth.ValidatePassword(password)
	if !valid {
		return nil, errors.New(400, "INVALID_PASSWORD", errs[0])
	}

	// Reject API keys in content
	if err := security.RejectAPIKeyInContent(name, "name"); err != nil {
		return nil, errors.New(400, "SENSITIVE_CONTENT", "name must not contain API keys")
	}
	if err := security.RejectAPIKeyInContent(password, "password"); err != nil {
		return nil, errors.New(400, "SENSITIVE_CONTENT", "password must not contain API keys")
	}

	// Check name existence (DB failure must not masquerade as "name free")
	exists, err := nameExistsProbe(ctx, pool, name)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "An error occurred during registration, please try again later")
	}
	if exists {
		return nil, errors.NewWithDetails(409, "NAME_TAKEN",
			"Bot name '"+name+"' is already taken",
			map[string]interface{}{
				"suggestion": "Try '" + name + "_v2' or other variations",
			})
	}

	// Generate key and hash password
	apiKey := auth.GenerateKey()
	hashedPassword, err := auth.HashPassword(password)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "An error occurred during registration, please try again later")
	}

	// Bot creation (balance=0) and the signup grant (+66) share one DB
	// transaction: either both the bot row and the grant_signup genesis
	// ledger entry exist, or neither does.
	tx, txErr := pool.TxBegin(ctx)
	if txErr != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "An error occurred during registration, please try again later")
	}
	defer func() { _ = pg.Rollback(tx) }()

	botID, err := repository.InsertRegisteredBot(ctx, tx, name, apiKey, hashedPassword, ip)
	if err != nil {
		// Check for unique constraint violation
		if isUniqueViolation(err) {
			return nil, errors.New(409, "NAME_TAKEN", "Registration failed: name already taken (concurrency conflict)")
		}
		return nil, errors.New(500, "INTERNAL_ERROR", "An error occurred during registration, please try again later")
	}

	if _, err := credits.Record(ctx, pool, tx, botID, "grant_signup", SignupGrant, nil, nil); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "An error occurred during registration, please try again later")
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "An error occurred during registration, please try again later")
	}

	// Operation log
	logOperation(ctx, pool, &botID, "register", nil, nil,
		map[string]interface{}{"bot_name": name}, true)

	return &RegistrationResult{
		BotName: name,
		Key:     apiKey,
		Balance: 0, // API returns 0; the 66-credit bonus is in the DB
		Message: "Registration successful. Give only the key to agents; keep the password for human key management.",
	}, nil
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if stderrors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}
