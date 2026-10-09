// Package combat is the reusable combat content battery for roost-skill
// hosts: attribute sets, buff containers, and the twelve-stage damage
// pipeline, all in deterministic fixed-point integer math (amounts in raw
// units, rates in basis points where 10000 = 100%).
//
// The package has zero dependencies so any host can embed it into its own
// entity model. The skill MemoryHost runs on this exact code, and
// combatcomponent wires it into roost-core entities, so test-host math,
// reference math, and production math cannot diverge.
//
// Randomness is intentionally absent: callers roll chances themselves (the
// skill runtime derives rolls from HMAC keys) and pass the outcomes in as
// facts (Dodge, ForceCritical, ...). Wall-clock time is likewise absent;
// durations are ticks supplied by the caller.
package combat

import "math"

// BasisPointScale is the fixed-point denominator for all rates: 10000 = 100%.
const BasisPointScale = 10000

// ScaleBasisPoints applies a basis-point rate to a value, saturating on
// int64 overflow. A rate of 0 means 0, not "unset" — callers apply defaults.
func ScaleBasisPoints(value, basisPoints int64) int64 {
	return saturatingInt64Mul(value, basisPoints) / BasisPointScale
}

func saturatingInt64Add(left, right int64) int64 {
	if right > 0 && left > math.MaxInt64-right {
		return math.MaxInt64
	}
	if right < 0 && left < math.MinInt64-right {
		return math.MinInt64
	}
	return left + right
}

func saturatingInt64Sub(left, right int64) int64 {
	if right > 0 && left < math.MinInt64+right {
		return math.MinInt64
	}
	if right < 0 && left > math.MaxInt64+right {
		return math.MaxInt64
	}
	return left - right
}

func saturatingInt64Mul(left, right int64) int64 {
	product, ok := checkedInt64Mul(left, right)
	if ok {
		return product
	}
	if (left < 0) != (right < 0) {
		return math.MinInt64
	}
	return math.MaxInt64
}

func checkedInt64Mul(left, right int64) (int64, bool) {
	if left == 0 || right == 0 {
		return 0, true
	}
	if (left == math.MinInt64 && right == -1) || (right == math.MinInt64 && left == -1) {
		return 0, false
	}
	product := left * right
	return product, product/right == left
}

func minInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
