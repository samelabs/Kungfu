package payment

import (
	"math"
	"math/big"
	"testing"
)

// Audit-repair regression proofs for reversalTarget: true 128-bit
// floor(C*R/P) — no float64, no lossy uint64→int64 cast, no
// "hi != 0 ⇒ reject" shortcut.
func TestReversalTarget128BitArithmetic(t *testing.T) {
	cases := []struct {
		name    string
		c       int64
		r, p    int64
		want    int64
		wantErr bool
	}{
		{"audit example floor", 1000, 300, 1080, 277, false},
		{"cumulative 605/1210", 1000, 605, 1210, 500, false},
		{"full refund exact", 1000, 1080, 1080, 1000, false},
		{"full refund MaxInt64 credits", math.MaxInt64, 500, 500, math.MaxInt64, false},
		{"zero refund", 1000, 0, 1080, 0, false},
		// lo > MaxInt64 but quotient valid: C=2^32, R=2^32+2 (even),
		// P=2^33 → product = 2^64 + 2^33 > MaxInt64, quotient = floor((2^64+2^33)/2^33) = 2^31+1.
		{"lo exceeds MaxInt64, quotient valid", 1 << 32, (1 << 32) + 2, 1 << 33, (1 << 31) + 1, false},
		// hi != 0 but quotient valid: C=MaxInt64, R=2, P=MaxInt64 →
		// product = 2*MaxInt64 (128-bit hi=1), quotient = 2.
		{"hi nonzero, quotient valid", math.MaxInt64, 2, math.MaxInt64, 2, false},
		// MaxInt64-scale legal: C=MaxInt64, R=MaxInt64-1, P=MaxInt64 → MaxInt64-1.
		{"MaxInt64 scale cumulative", math.MaxInt64, math.MaxInt64 - 1, math.MaxInt64, math.MaxInt64 - 1, false},
		// invalid denominator / refund bounds
		{"denominator zero", 1000, 1, 0, 0, true},
		{"denominator negative", 1000, 1, -5, 0, true},
		{"refund negative", 1000, -1, 1080, 0, true},
		{"refund exceeds paid", 1000, 1081, 1080, 0, true},
		{"credits non-positive", 0, 1, 1080, 0, true},
	}
	for _, tc := range cases {
		got, err := reversalTarget(tc.c, tc.r, tc.p)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: expected error, got %d", tc.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: floor(%d*%d/%d) = %d, want %d", tc.name, tc.c, tc.r, tc.p, got, tc.want)
		}
	}
}

// Cross-check against big.Int on a deterministic grid: every result
// must equal the mathematically exact floor and stay ≤ C.
func TestReversalTargetMatchesBigInt(t *testing.T) {
	credits := []int64{1, 7, 1000, 1 << 40, math.MaxInt64 / 3, math.MaxInt64}
	rs := []int64{0, 1, 3, 300, 1079, 1080, 1<<62 - 1, 1 << 62, math.MaxInt64}
	ps := []int64{1, 2, 1080, 1<<62 + 1, math.MaxInt64}
	for _, c := range credits {
		for _, r := range rs {
			for _, p := range ps {
				if r > p {
					continue
				}
				got, err := reversalTarget(c, r, p)
				if err != nil {
					t.Fatalf("c=%d r=%d p=%d: %v", c, r, p, err)
				}
				want := bigFloor(c, r, p)
				if got != want {
					t.Fatalf("c=%d r=%d p=%d: got %d want %d", c, r, p, got, want)
				}
				if got > c {
					t.Fatalf("c=%d r=%d p=%d: %d exceeds credits", c, r, p, got)
				}
			}
		}
	}
}

// bigFloor is an INDEPENDENT oracle: math/big exact arithmetic, a
// completely different code path from reversalTarget.
func bigFloor(c, r, p int64) int64 {
	cb := new(big.Int).SetInt64(c)
	rb := new(big.Int).SetInt64(r)
	pb := new(big.Int).SetInt64(p)
	prod := new(big.Int).Mul(cb, rb)
	q := new(big.Int).Quo(prod, pb) // Quo truncates toward zero; operands ≥0 ⇒ floor
	if !q.IsInt64() {
		panic("oracle overflow")
	}
	return q.Int64()
}
