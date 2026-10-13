package skill

import (
	"math/big"
	"math/bits"
)

// 逐 tick 热路径用的无分配定点运算。这里每个函数都是确定性的纯整数运算，必须与 big.Int 参考实现逐位一致
// （fixed_math_test.go 对等价性做模糊测试）；超出快路径的量级回退到参考实现，在整个 int64 范围上保持精确。

const maxInt64Value = int64(^uint64(0) >> 1)
const minInt64Value = -maxInt64Value - 1

// mulDivRounded 用 128 位中间积计算 value*numerator/denominator，是 big.Int 参考实现的无分配、逐位一致版本，
// int64 溢出时饱和。
//
// 舍入忠实复现参考实现，包括它的特例：半值进位的方向取乘积的符号（而不是商的符号），所以分母为负时的结果与
// 参考实现一致（例如 1/-2 = 1）。生产调用点传的分母都是正数（长度与距离），这时就是普通的四舍五入（远离零）；
// 保留这个特例只是因为逐位确定性比算术上的好看更重要。
func mulDivRounded(value, numerator, denominator int64) int64 {
	if denominator == 0 {
		return 0
	}
	productNeg := value != 0 && numerator != 0 && (value < 0) != (numerator < 0)
	quotientNeg := productNeg != (denominator < 0)
	uValue, uNumerator, uDenominator := absUint64(value), absUint64(numerator), absUint64(denominator)
	hi, lo := bits.Mul64(uValue, uNumerator)
	if hi >= uDenominator {
		// 截断后的商超过 64 位，±1 的进位也拉不回范围内，与参考实现一样饱和。
		if quotientNeg {
			return minInt64Value
		}
		return maxInt64Value
	}
	quotient, remainder := bits.Div64(hi, lo, uDenominator)
	// remainder < uDenominator <= 2^63，remainder<<1 不会溢出。
	bump := int64(0)
	if remainder<<1 >= uDenominator {
		if productNeg {
			bump = -1
		} else {
			bump = 1
		}
	}
	if quotientNeg {
		// 量级 <= 2^63 时可以表示为 MinInt64。
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

// integerDistance 是两点之间取整后的欧氏距离：sqrt(dx²+dy²) 四舍五入到最近整数，在 MaxInt64 处饱和。平方能放进
// 64 位快路径的差值（|delta| <= 约 3.03e9，远超任何世界坐标范围）不分配内存；更大的量级走 big.Int 参考路径。
func integerDistance(left, right Position) int64 {
	distance, _ := integerDistanceSaturated(left, right)
	return distance
}

// integerDistanceSaturated 另外报告真实距离是否超过 int64 而被夹取——比较时必须把饱和值当作严格大于任何 int64 上界。
func integerDistanceSaturated(left, right Position) (int64, bool) {
	dx := absDeltaUint64(right.X, left.X)
	dy := absDeltaUint64(right.Y, left.Y)
	const fastLimit = 3037000499 // floor(sqrt(MaxInt64)): 2·fastLimit² < 2⁶⁴, so dx²+dy² fits uint64
	if dx <= fastLimit && dy <= fastLimit {
		squared := dx*dx + dy*dy
		root := isqrt64(squared)
		// 舍入到最近：余数 >= root+1 说明更接近 root+1（与参考实现的规则一致）。
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

// isqrt64 返回 floor(sqrt(x))：从根的上方开始做整数牛顿迭代，单调向下收敛，确定且不用浮点。
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

// absDeltaUint64 在整个 int64 范围上精确计算 |a-b|：用较大的操作数做补码减法得到精确量级，总能放进 uint64
// （最大 2^64-1）。
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

// bigIntegerDistance 是任意精度的参考实现，只供等价测试使用；生产路径超出快路径时直接调用
// bigIntegerDistanceSaturated。
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
