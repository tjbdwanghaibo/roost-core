package combat

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
)

// 确定性的概率判定。伤害管线把闪避与暴击结果当作预先掷好的事实（Dodge、ForceCritical 等）；这些函数是 Host 产生
// 这些事实的标准方式，不依赖墙钟或 math/rand 状态：同样的密钥、用途与坐标总得到同样的结果，回放与副本逐位一致。
//
// 密钥是对局或施法范围内的秘密（skill Host 通常从对局种子派生）。坐标把一次判定钉在一个玩法时刻上——对技能效果
// 命令，推荐用事件的 RootEventID、EventID、EffectIndex 与目标实体 ID，每个伤害实例独立判定且可复现。

// RollValue 由密钥、用途与坐标派生 [0, BasisPointScale) 上的均匀值。不同用途（"crit"、"dodge"）在同样的坐标上
// 相互独立。
func RollValue(key []byte, purpose string, coordinates ...uint64) int64 {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(purpose))
	var buffer [8]byte
	for _, coordinate := range coordinates {
		binary.BigEndian.PutUint64(buffer[:], coordinate)
		_, _ = mac.Write(buffer[:])
	}
	digest := mac.Sum(nil)
	// 2^64 个样本对 10000 取模的偏差小于 1e-15，相对玩法概率的粒度可以忽略。
	return int64(binary.BigEndian.Uint64(digest[:8]) % BasisPointScale)
}

// ChanceRoll 报告给定坐标上以基点表示的概率是否命中：chanceBP <= 0 从不命中，chanceBP >= 10000 总是命中。
func ChanceRoll(key []byte, purpose string, chanceBP int64, coordinates ...uint64) bool {
	if chanceBP <= 0 {
		return false
	}
	if chanceBP >= BasisPointScale {
		return true
	}
	return RollValue(key, purpose, coordinates...) < chanceBP
}
