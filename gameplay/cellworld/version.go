package cellworld

// Version 是一个实体空间状态的版本：Epoch 是迁移代际（每次交接 +1，只有当前 epoch 的 Real 能产生该 epoch
// 的新版本），Seq 是同一实体的单调序号（Real 每拍 +1，交接时随状态带走，所以跨 epoch 也单调）。
// 接收端按 (Epoch, Seq) 字典序只接受更新的版本，旧消息、重复消息直接丢弃，不依赖传输的投递顺序。
type Version struct {
	Epoch uint64 `msgpack:"e"`
	Seq   uint64 `msgpack:"s"`
}

// Newer 报告 v 是否严格新于 old。
func (v Version) Newer(old Version) bool {
	if v.Epoch != old.Epoch {
		return v.Epoch > old.Epoch
	}
	return v.Seq > old.Seq
}

// AtLeast 报告 v 是否不旧于 old（相等或更新）。
func (v Version) AtLeast(old Version) bool { return v == old || v.Newer(old) }
