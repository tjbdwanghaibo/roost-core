package skill

import (
	"math/big"
	"math/bits"
)

// Allocation-free fixed-point arithmetic for the per-tick hot paths. Every
// function here is deterministic pure integer math and must stay bit-exact
// with the reference big.Int implementations (see fixed_math_test.go, which
// fuzzes the equivalence); magnitudes outside the fast paths fall back to the
// reference implementation so exactness holds over the full int64 domain.

const maxInt64Value = int64(^uint64(0) >> 1)
const minInt64Value = -maxInt64Value - 1

// mulDivRounded computes value*numerator/denominator with a 128-bit
// intermediate product — the allocation-free, bit-exact equivalent of the
// historical big.Int scaleRatioRounded, saturating on int64 overflow.
//
// Rounding faithfully reproduces the reference, including its quirk: the
// half-bump is applied in the direction of the PRODUCT's sign (not the
// quotient's), so results for negative denominators mirror the historical
// behavior (e.g. 1/-2 = 1). Every production call site passes a positive
// denominator (lengths and distances), where this equals ordinary
// round-half-away-from-zero; the quirk domain is preserved only because
// bit-exact determinism outranks arithmetic aesthetics here.
func mulDivRounded(value, numerator, denominator int64) int64 {
	if denominator == 0 {
		return 0
	}
	productNeg := value != 0 && numerator != 0 && (value < 0) != (numerator < 0)
	quotientNeg := productNeg != (denominator < 0)
	uValue, uNumerator, uDenominator := absUint64(value), absUint64(numerator), absUint64(denominator)
	hi, lo := bits.Mul64(uValue, uNumerator)
	if hi >= uDenominator {
		// The truncated quotient exceeds 64 bits; a ±1 bump cannot bring it
		// back into range, so saturate like the reference.
		if quotientNeg {
			return minInt64Value
		}
		return maxInt64Value
	}
	quotient, remainder := bits.Div64(hi, lo, uDenominator)
	// remainder < uDenominator <= 2^63, so remainder<<1 cannot overflow.
	bump := int64(0)
	if remainder<<1 >= uDenominator {
		if productNeg {
			bump = -1
		} else {
			bump = 1
		}
	}
	if quotientNeg {
		// magnitude <= 2^63 is representable as MinInt64.
		if quotient > uint64(maxInt64Value)+1 {
			return minInt64Value
		}
		base := -int64(quotient>>1) - int64(quotient-(quotient>>1)) // -quotient, MinInt64-safe
		if base == minInt64Value && bump < 0 {
			return minInt64Value // exact value below MinInt64: saturate
		}
		return base + bump
	}
	if quotient > uint64(maxInt64Value) {
		return maxInt64Value
	}
	if int64(quotient) == maxInt64Value && bump > 0 {
		return maxInt64Value // exact value above MaxInt64: saturate
	}
	return int64(quotient) + bump
}

// integerDistance is the rounded Euclidean distance between two positions:
// round-to-nearest of sqrt(dx²+dy²), saturating at MaxInt64. Deltas whose
// squares fit the 64-bit fast path (|delta| <= ~3.03e9, far beyond any world
// coordinate span) run allocation-free; larger magnitudes take the big.Int
// reference path.
func integerDistance(left, right Position) int64 {
	distance, _ := integerDistanceSaturated(left, right)
	return distance
}

// integerDistanceSaturated additionally reports whether the true distance
// exceeded int64 and was clamped — comparisons must treat a saturated value
// as strictly greater than any int64 bound.
func integerDistanceSaturated(left, right Position) (int64, bool) {
	dx := absDeltaUint64(right.X, left.X)
	dy := absDeltaUint64(right.Y, left.Y)
	const fastLimit = 3037000499 // floor(sqrt(MaxInt64)): 2·fastLimit² < 2⁶⁴, so dx²+dy² fits uint64
	if dx <= fastLimit && dy <= fastLimit {
		squared := dx*dx + dy*dy
		root := isqrt64(squared)
		// Round to nearest: remainder > root means closer to root+1 (matches
		// the reference's remainder >= root+1 rule).
		if squared-root*root >= root+1 {
			root++
		}
		return int64(root), false
	}
	return bigIntegerDistanceSaturated(left, right)
}

func distanceExceeds(left, right Position, maximum int64) bool {
	distance, saturated := integerDistanceSaturated(left, right)
	if saturated {
		return true
	}
	return distance > maximum
}

// isqrt64 returns floor(sqrt(x)) using integer Newton iteration seeded above
// the root, which converges monotonically downward — deterministic and
// float-free.
func isqrt64(x uint64) uint64 {
	if x == 0 {
		return 0
	}
	root := uint64(1) << ((bits.Len64(x) + 1) / 2)
	for {
		next := (root + x/root) / 2
		if next >= root {
			return root
		}
		root = next
	}
}

// absDeltaUint64 is the exact |a-b| over the full int64 domain: two's
// complement subtraction of the larger operand yields the exact magnitude,
// which always fits uint64 (max 2^64-1).
func absDeltaUint64(a, b int64) uint64 {
	if a >= b {
		return uint64(a) - uint64(b)
	}
	return uint64(b) - uint64(a)
}

func absUint64(value int64) uint64 {
	if value < 0 {
		return uint64(-(value + 1)) + 1 // MinInt64-safe
	}
	return uint64(value)
}

// bigIntegerDistance is the arbitrary-precision reference used for magnitudes
// beyond the fast path and by the equivalence tests.
func bigIntegerDistance(left, right Position) int64 {
	distance, _ := bigIntegerDistanceSaturated(left, right)
	return distance
}

func bigIntegerDistanceSaturated(left, right Position) (int64, bool) {
	dx := new(big.Int).Sub(big.NewInt(right.X), big.NewInt(left.X))
	dy := new(big.Int).Sub(big.NewInt(right.Y), big.NewInt(left.Y))
	squared := new(big.Int).Add(new(big.Int).Mul(dx, dx), new(big.Int).Mul(dy, dy))
	root := new(big.Int).Sqrt(squared)
	remainder := new(big.Int).Sub(squared, new(big.Int).Mul(new(big.Int).Set(root), root))
	if remainder.Cmp(new(big.Int).Add(root, big.NewInt(1))) >= 0 {
		root.Add(root, big.NewInt(1))
	}
	if !root.IsInt64() {
		return maxInt64Value, true
	}
	return root.Int64(), false
}
