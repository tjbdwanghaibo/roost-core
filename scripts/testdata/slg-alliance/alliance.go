// Package alliance 是 SLG 联盟活动教学消费者；规则不进入公共框架。
package alliance

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
	"github.com/tjbdwanghaibo/roost-core/service/activity"
)

var errRefused = errors.New("alliance: command refused")

// Member 的 Home 来自已鉴权的静态角色归属；客户端不能提交 sid 或贡献数值。
type member struct {
	Home   int32
	Active bool
}
type intent struct {
	Player    string
	Home      int32
	Sent      bool
	CreatedAt int64
}
type state struct {
	Key     activity.Key
	Members map[string]member
	Intents map[string]intent
	Closed  bool
}

// 联盟权威用一个 CAS 同时裁决成员权限和贡献意图。每个已验战果固定 10 分，
// 教学账本最多 128 条，满后拒新写。业务实例不自动归档或删除旧身份。
type alliance struct {
	store versionstore.Store[string, state]
	key   activity.Key
}

func newAlliance(r versionstore.RedisClient, prefix string, key activity.Key) (*alliance, error) {
	s, e := versionstore.NewRedisStore(r, versionstore.RedisConfig[string, state]{Prefix: prefix + ":alliance:", KeyOf: func(k string) string { return k }, Codec: versionstore.JSONCodec[state]{}})
	return &alliance{store: s, key: key}, e
}
func (a *alliance) open(ctx context.Context) error {
	_, _, e := a.store.Create(ctx, a.key.String(), state{Key: a.key, Members: map[string]member{"alice": {1000, true}, "bob": {1001, true}}, Intents: map[string]intent{}})
	return e
}
func (a *alliance) read(ctx context.Context) (state, error) {
	v, ok, e := a.store.Get(ctx, a.key.String())
	if e == nil && !ok {
		e = errRefused
	}
	return v.Value, e
}
func clone(s state) state {
	s.Members = maps.Clone(s.Members)
	s.Intents = maps.Clone(s.Intents)
	return s
}

// battleID 必须是可信战斗权威的不可变结果身份；不能把客户端自报成功接到这里。
// 已成功的同身份重试允许查回执，即使后来退盟；新身份在撤权后立即拒绝。
func (a *alliance) contribute(ctx context.Context, home int32, player, battleID string, now time.Time) error {
	if battleID == "" || len(battleID) > 64 {
		return errRefused
	}
	_, _, e := a.store.Update(ctx, a.key.String(), func(s state, found bool) (state, bool, error) {
		m, ok := s.Members[player]
		if !found || !ok || m.Home != home {
			return s, false, errRefused
		}
		if old, ok := s.Intents[battleID]; ok {
			if old.Player != player || old.Home != home {
				return s, false, errRefused
			}
			return s, false, nil
		}
		if !m.Active || s.Closed || len(s.Intents) >= 128 {
			return s, false, errRefused
		}
		s = clone(s)
		s.Intents[battleID] = intent{Player: player, Home: home, CreatedAt: now.Unix()}
		return s, true, nil
	})
	return e
}
func (a *alliance) revoke(ctx context.Context, player string) error {
	_, _, e := a.store.Update(ctx, a.key.String(), func(s state, found bool) (state, bool, error) {
		m, ok := s.Members[player]
		if !found || !ok {
			return s, false, errRefused
		}
		s = clone(s)
		m.Active = false
		s.Members[player] = m
		return s, true, nil
	})
	return e
}
func (a *alliance) freeze(ctx context.Context) error {
	_, _, e := a.store.Update(ctx, a.key.String(), func(s state, found bool) (state, bool, error) {
		if !found {
			return s, false, errRefused
		}
		s.Closed = true
		return s, true, nil
	})
	return e
}

// drain 在 CAS 外调用 RPC。回复丢失时保留原窗口、玩家和战果身份重试；只有
// RPC 成功才确认意图。超过协调器保留期则停止自动补报，交给业务对账，不能再加分。
func (a *alliance) drain(ctx context.Context, c activity.Coordinator, now time.Time) error {
	s, e := a.read(ctx)
	if e != nil {
		return e
	}
	for id, v := range s.Intents {
		if v.Sent {
			continue
		}
		if now.Unix()-v.CreatedAt >= int64(time.Hour/time.Second) {
			return fmt.Errorf("contribution %s requires reconciliation", id)
		}
		if _, e = c.ApplyProgress(ctx, s.Key, v.Player, id, activity.ProgressDelta{Score: 10, Progress: 1}); e != nil {
			return e
		}
		_, _, e = a.store.Update(ctx, a.key.String(), func(cur state, found bool) (state, bool, error) {
			if !found {
				return cur, false, errRefused
			}
			cur = clone(cur)
			v := cur.Intents[id]
			v.Sent = true
			cur.Intents[id] = v
			return cur, true, nil
		})
		if e != nil {
			return e
		}
	}
	return nil
}

// phase 只能在停止准入且全部贡献确认后推进；否则结算会截断尚未上报的战果。
func (a *alliance) phase(ctx context.Context, c activity.Coordinator, home int32) error {
	s, e := a.read(ctx)
	if e != nil {
		return e
	}
	if !s.Closed {
		return errRefused
	}
	for _, v := range s.Intents {
		if !v.Sent {
			return errRefused
		}
	}
	_, e = c.NotifyPhase(ctx, s.Key, home)
	return e
}

type reward struct {
	Key    activity.Key
	Points map[string]int64
}
type homeState struct {
	SID      int32
	Receipts map[string]reward
}
type home struct {
	sid   int32
	store versionstore.Store[string, homeState]
}

func newHome(r versionstore.RedisClient, prefix string, sid int32) (*home, error) {
	s, e := versionstore.NewRedisStore(r, versionstore.RedisConfig[string, homeState]{Prefix: fmt.Sprintf("%s:home:%d:", prefix, sid), KeyOf: func(k string) string { return k }, Codec: versionstore.JSONCodec[homeState]{}})
	return &home{sid: sid, store: s}, e
}

// receive 只把不可变奖励义务持久交到原服，并不直接修改 Player 金币。
// Player DAO / 邮件消费该义务时仍需自己的永久幂等到账回执；ACK 不表示玩家已领取。
func (h *home) receive(ctx context.Context, d activity.Dispatch, s state) error {
	if d.GameSID != h.sid || d.Key != s.Key || d.Result.Key != s.Key || d.Result.Reason != activity.CompletedCollected || !s.Closed {
		return errRefused
	}
	points := map[string]int64{}
	for _, v := range s.Intents {
		if !v.Sent {
			return errRefused
		}
		if v.Home == h.sid {
			points[v.Player] += 10
		}
	}
	_, _, e := h.store.Update(ctx, "rewards", func(cur homeState, found bool) (homeState, bool, error) {
		if found && cur.SID != h.sid {
			return cur, false, errRefused
		}
		if old, ok := cur.Receipts[s.Key.String()]; ok {
			if old.Key != s.Key || !maps.Equal(old.Points, points) {
				return cur, false, errRefused
			}
			return cur, false, nil
		}
		if len(cur.Receipts) >= 128 {
			return cur, false, errRefused
		}
		cur.SID = h.sid
		cur.Receipts = maps.Clone(cur.Receipts)
		if cur.Receipts == nil {
			cur.Receipts = map[string]reward{}
		}
		cur.Receipts[s.Key.String()] = reward{s.Key, points}
		return cur, true, nil
	})
	return e
}
