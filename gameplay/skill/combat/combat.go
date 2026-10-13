// Package combat 是 roost 技能 Host 可复用的战斗内容组件：属性集、buff 容器与十二阶段伤害管线，全部用确定性的
// 定点整数运算（数量用原始单位，比率用基点，10000 = 100%）。
//
// 包没有任何依赖，任何 Host 都能把它嵌进自己的实体模型。skill 的 MemoryHost 直接跑这份代码，combatcomponent
// 把它接进 roost-core 实体，测试 Host、参考实现与生产环境的计算不会分叉。
//
// 刻意不含随机性：概率由调用方自己掷（skill 运行时从 HMAC 密钥派生），结果作为事实传入（Dodge、ForceCritical 等）。
// 同样不含墙钟时间：时长是调用方给的 tick。
package combat

import "math"

// BasisPointScale 是所有比率的定点分母：10000 = 100%。
const BasisPointScale = 10000

// ScaleBasisPoints 按基点比率缩放数值，int64 溢出时饱和。比率 0 就是 0，不表示“未设置”——默认值由调用方补。
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
