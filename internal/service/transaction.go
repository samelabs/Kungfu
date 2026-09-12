package service

import "kungfu.md/internal/credits"

// The credits domain owns balance mutation and the ledger. These aliases keep
// existing service-internal call sites compiling with one-line diffs; new
// code should import internal/credits directly.
var (
	// Record is the single credit mutation primitive (credits.Record).
	Record = credits.Record
	// GetBalance reads the current balance (credits.Balance).
	GetBalance = credits.Balance
)
