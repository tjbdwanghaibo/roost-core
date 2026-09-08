package skill

import (
	"math"
	"math/big"
	"testing"
)

// referenceMulDivRounded is the original big.Int scaleRatioRounded, kept as
// the equivalence oracle: the allocation-free replacement must be bit-exact
// with it over the full domain or determinism (and replay) breaks.
func referenceMulDivRounded(value, numerator, denominator int64) int64 {
	if denominator == 0 {
		return 0
	}
	product := new(big.Int).Mul(big.NewInt(value), big.NewInt(numerator))
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(product, big.NewInt(denominator), remainder)
	twiceRemainder := new(big.Int).Lsh(new(big.Int).Abs(remainder), 1)
	if twiceRemainder.Cmp(big.NewInt(absoluteDifference(denominator, 0))) >= 0 {
		if product.Sign() < 0 {
			quotient.Sub(quotient, big.NewInt(1))
		} else {
			quotient.Add(quotient, big.NewInt(1))
		}
	}
	if quotient.IsInt64() {
		return quotient.Int64()
	}
	if quotient.Sign() < 0 {
		return math.MinInt64
	}
	return math.MaxInt64
}

// deterministic pseudo-random source: tests must not depend on math/rand
// global state ordering.
type xorshift struct{ state uint64 }

func (x *xorshift) next() int64 {
	x.state ^= x.state << 13
	x.state ^= x.state >> 7
	x.state ^= x.state << 17
	return int64(x.state)
}

func interestingInt64s() []int64 {
	return []int64{
		0, 1, -1, 2, -2, 3, 9999, 10000, 10001, -10000,
		math.MaxInt32, math.MinInt32, math.MaxInt64, math.MinInt64,
		math.MaxInt64 - 1, math.MinInt64 + 1, 3037000499, 3037000500, -3037000499,
	}
}

func TestMulDivRoundedMatchesReference(t *testing.T) {
	check := func(value, numerator, denominator int64) {
		t.Helper()
		got := mulDivRounded(value, numerator, denominator)
		want := referenceMulDivRounded(value, numerator, denominator)
		if got != want {
			t.Fatalf("mulDivRounded(%d, %d, %d) = %d, reference = %d", value, numerator, denominator, got, want)
		}
	}
	corners := interestingInt64s()
	for _, value := range corners {
		for _, numerator := range corners {
			for _, denominator := range corners {
				check(value, numerator, denominator)
			}
		}
	}
	rng := &xorshift{state: 0x9E3779B97F4A7C15}
	for i := 0; i < 200000; i++ {
		check(rng.next(), rng.next()%20001-10000, rng.next())
		check(rng.next()%3000000000, 10000, rng.next()%3000000000)
	}
}

func TestIntegerDistanceMatchesReference(t *testing.T) {
	check := func(left, right Position) {
		t.Helper()
		got := integerDistance(left, right)
		want := bigIntegerDistance(left, right)
		if got != want {
			t.Fatalf("integerDistance(%+v, %+v) = %d, reference = %d", left, right, got, want)
		}
	}
	corners := interestingInt64s()
	for _, x1 := range corners {
		for _, y1 := range corners {
			check(Position{X: x1, Y: y1}, Position{})
			check(Position{}, Position{X: x1, Y: y1})
			check(Position{X: x1, Y: y1}, Position{X: y1, Y: x1})
		}
	}
	rng := &xorshift{state: 0xC2B2AE3D27D4EB4F}
	for i := 0; i < 200000; i++ {
		// Realistic world-coordinate spans plus fast-path boundary crossers.
		check(Position{X: rng.next() % 3100000000, Y: rng.next() % 3100000000},
			Position{X: rng.next() % 3100000000, Y: rng.next() % 3100000000})
		check(Position{X: rng.next() % 1000000, Y: rng.next() % 1000000},
			Position{X: rng.next() % 1000000, Y: rng.next() % 1000000})
	}
}

func TestMotionMetricIsEuclidean(t *testing.T) {
	// Regression: motion used Chebyshev (max(|dx|,|dy|)) while input
	// validation used Euclidean distance, so a diagonal tracking projectile
	// moved up to 41% faster than an axis-aligned one. One world, one metric.
	origin, diagonal := Position{}, Position{X: 1000, Y: 1000}
	length := integerDistance(origin, diagonal)
	if length != 1414 {
		t.Fatalf("diagonal distance = %d, want 1414 (Euclidean); 1000 would be the Chebyshev bug", length)
	}
	if got := motionDistance(origin, diagonal); got != length {
		t.Fatalf("motionDistance = %d, must equal the Euclidean distance %d", got, length)
	}

	direction := normalizedDirection(origin, diagonal, length)
	if direction.X == normalizedDirectionScale && direction.Y == normalizedDirectionScale {
		t.Fatal("direction normalized by Chebyshev length: diagonal speed is 41% too fast")
	}
	norm := integerDistance(origin, Position{X: direction.X, Y: direction.Y})
	if norm < normalizedDirectionScale-1 || norm > normalizedDirectionScale+1 {
		t.Fatalf("normalized direction Euclidean norm = %d, want ~%d", norm, normalizedDirectionScale)
	}

	// Path advance consumes Euclidean budget: a diagonal segment of length
	// ~1414 must not be fully traversed with a speed budget of 1000.
	index := 0
	position := advanceMotionPath(origin, []Position{diagonal}, 1000, &index)
	if position == diagonal {
		t.Fatal("path advance still consumes Chebyshev distance")
	}
	if got := integerDistance(origin, position); got < 999 || got > 1001 {
		t.Fatalf("path advanced %d world units with a budget of 1000", got)
	}
}

func TestIsqrt64Floor(t *testing.T) {
	for _, x := range []uint64{0, 1, 2, 3, 4, 15, 16, 17, 1<<32 - 1, 1 << 32, 1<<32 + 1, math.MaxUint64} {
		root := isqrt64(x)
		if root*root > x {
			t.Fatalf("isqrt64(%d) = %d overshoots", x, root)
		}
		if next := root + 1; next*next > next && next*next <= x { // next*next may wrap at max
			t.Fatalf("isqrt64(%d) = %d undershoots", x, root)
		}
	}
}
