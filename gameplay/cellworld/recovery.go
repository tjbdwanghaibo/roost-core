package cellworld

import (
	"errors"
	"slices"
	"time"
)

// TimedEffect 是可跨 Cell、跨重启恢复的限时状态。Key 对应业务配置，Value 由业务解释；
// 框架负责保留和过期，不代替业务的 Buff 叠加/属性计算。ExpiresAt=0 表示永久。
// 时间统一使用 UTC UnixMilli；停机期间时间照常流逝，不能把剩余时长重新加一遍。
type TimedEffect struct {
	Key       string `msgpack:"k"`
	Stacks    int32  `msgpack:"s"`
	Value     int64  `msgpack:"v"`
	ExpiresAt int64  `msgpack:"e"`
}
type Cooldown struct {
	Key     string `msgpack:"k"`
	ReadyAt int64  `msgpack:"r"`
}

// RecoveryState 是玩家快照的一部分，必须映射为私有持久字段；Actor 的同名字段是私有临时状态。
// 写入后不可修改底层切片。SetRecovery 复制并规范化，DAO 接入同样应读写值副本。
type RecoveryState struct {
	Revision  uint64        `msgpack:"v"`
	Effects   []TimedEffect `msgpack:"e,omitempty"`
	Cooldowns []Cooldown    `msgpack:"c,omitempty"`
}

const MaxRecoveryEntries = 128

func (s RecoveryState) Clone() RecoveryState {
	s.Effects = slices.Clone(s.Effects)
	s.Cooldowns = slices.Clone(s.Cooldowns)
	return s
}
func (s RecoveryState) Validate() error {
	if len(s.Effects)+len(s.Cooldowns) > MaxRecoveryEntries {
		return errors.New("cellworld: too many recovery entries")
	}
	effects, cooldowns := map[string]bool{}, map[string]bool{}
	for _, e := range s.Effects {
		if e.Key == "" || len(e.Key) > 64 || e.Stacks <= 0 || e.ExpiresAt < 0 || effects[e.Key] {
			return errors.New("cellworld: invalid or repeated effect")
		}
		effects[e.Key] = true
	}
	for _, c := range s.Cooldowns {
		if c.Key == "" || len(c.Key) > 64 || c.ReadyAt < 0 || cooldowns[c.Key] {
			return errors.New("cellworld: invalid or repeated cooldown")
		}
		cooldowns[c.Key] = true
	}
	return nil
}

// At 返回指定时间仍生效的副本；只在确有过期条目时分配，旧快照不会被原地改写。
func (s RecoveryState) At(now time.Time) RecoveryState {
	at := now.UnixMilli()
	expiredEffect := func(e TimedEffect) bool { return e.ExpiresAt != 0 && e.ExpiresAt <= at }
	expiredCooldown := func(c Cooldown) bool { return c.ReadyAt <= at }
	if slices.ContainsFunc(s.Effects, expiredEffect) {
		s.Effects = slices.DeleteFunc(slices.Clone(s.Effects), expiredEffect)
	}
	if slices.ContainsFunc(s.Cooldowns, expiredCooldown) {
		s.Cooldowns = slices.DeleteFunc(slices.Clone(s.Cooldowns), expiredCooldown)
	}
	return s
}

// SetRecovery 在 Actor 的 Nest handler 中设置 Buff / 冷却快照；setter 只改本实体 DAO，
// 随正常回写持久化、随 Offer 交接，不在 handler 中等待数据库。
func (n *Node) SetRecovery(a Actor, value RecoveryState) error {
	if err := n.guardTransaction(); err != nil {
		return err
	}
	if err := value.Validate(); err != nil {
		return err
	}
	st := a.CellState()
	if st.Role != RoleReal {
		return ErrOwnerConflict
	}
	if st.Recovery.Revision == ^uint64(0) {
		return errors.New("cellworld: recovery revision exhausted")
	}
	value = value.Clone().At(time.Now())
	value.Revision = st.Recovery.Revision + 1
	st.Recovery = value
	st.Ver.Seq++
	a.SetCellState(st)
	return nil
}
