// Package alliance 是 SLG 联盟活动教学消费者；规则不进入公共框架。
package alliance

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
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
type allianceStore interface {
	Get(context.Context, string) (versionstore.Versioned[state], bool, error)
	Create(context.Context, string, state) (versionstore.Versioned[state], bool, error)
	Update(context.Context, string, versionstore.Mutate[state]) (versionstore.Versioned[state], bool, error)
}

type alliance struct {
	store allianceStore
	key   activity.Key
	// reservationTTL 是协调器配置的 activity.Config.ReservationTTL，也是自动补报的截止：协调器只在这段时间内
	// 按请求身份去重，过了还补报，同一战果会被计两次。两处必须是同一个值；零值与 activity.New 一样按
	// activity.DefaultReservationTTL。
	reservationTTL time.Duration
}

// newAlliance 的 reservationTTL 传协调器配置里的同一个 ReservationTTL。
func newAlliance(r versionstore.RedisClient, prefix string, key activity.Key, reservationTTL time.Duration) (*alliance, error) {
	s, e := versionstore.NewRedisStore(r, versionstore.RedisConfig[string, state]{Prefix: prefix + ":alliance:", KeyOf: func(k string) string { return k }, Codec: versionstore.JSONCodec[state]{}})
	return newAllianceWithStore(s, key, reservationTTL), e
}

// newAllianceWithStore 让 Remote 托管实体等别的存储承载同一套联盟规则。
func newAllianceWithStore(store allianceStore, key activity.Key, reservationTTL time.Duration) *alliance {
	return &alliance{store: store, key: key, reservationTTL: reservationTTL}
}

// reportDeadline 是待报意图停止自动补报的时长，按 activity.New 的同一规则补缺省。
func (a *alliance) reportDeadline() time.Duration {
	if a.reservationTTL == 0 {
		return activity.DefaultReservationTTL
	}
	return a.reservationTTL
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
// RPC 成功才确认意图。超过协调器保留期（reportDeadline）则停止自动补报，交给业务对账，不能再加分。
func (a *alliance) drain(ctx context.Context, c activity.Coordinator, now time.Time) error {
	s, e := a.read(ctx)
	if e != nil {
		return e
	}
	for id, v := range s.Intents {
		if v.Sent {
			continue
		}
		if now.Sub(time.Unix(v.CreatedAt, 0)) >= a.reportDeadline() {
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
	SID       int32
	Receipts  map[string]reward
	Delivered map[string]bool
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

// deliverPending 是启动／定时任务可调用的一次有界泵：从原服义务账本发现工作，
// 不依赖已 ACK 的协调器索引，不猜当前窗口。只有全部 Player 永久到账成功才标记完成。
// grant 在 CAS 外调用；进程在到账与标记之间退出时，恢复会重投同身份，由 Player 去重。
// 回执与 Delivered 一起保留，不用这个标记替代 Player 永久回执；本示例不自动归档。
func (h *home) deliverPending(ctx context.Context, limit int, grant func(context.Context, activity.Key, string, int64) error) (int, error) {
	if limit < 1 || limit > 128 || grant == nil {
		return 0, errRefused
	}
	stored, found, e := h.store.Get(ctx, "rewards")
	if e != nil || !found {
		return 0, e
	}
	if stored.Value.SID != h.sid {
		return 0, errRefused
	}
	keys := make([]string, 0, len(stored.Value.Receipts))
	for k := range stored.Value.Receipts {
		if !stored.Value.Delivered[k] {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	done := 0
	for _, k := range keys {
		if done == limit {
			break
		}
		r := stored.Value.Receipts[k]
		if k != r.Key.String() {
			return done, errRefused
		}
		players := make([]string, 0, len(r.Points))
		for p := range r.Points {
			players = append(players, p)
		}
		slices.Sort(players)
		for _, p := range players {
			if e := grant(ctx, r.Key, p, r.Points[p]); e != nil {
				return done, e
			}
		}
		_, _, e = h.store.Update(ctx, "rewards", func(cur homeState, found bool) (homeState, bool, error) {
			if !found || cur.SID != h.sid {
				return cur, false, errRefused
			}
			actual, ok := cur.Receipts[k]
			if !ok || actual.Key != r.Key || !maps.Equal(actual.Points, r.Points) {
				return cur, false, errRefused
			}
			if cur.Delivered[k] {
				return cur, false, nil
			}
			cur.Delivered = maps.Clone(cur.Delivered)
			if cur.Delivered == nil {
				cur.Delivered = map[string]bool{}
			}
			cur.Delivered[k] = true
			return cur, true, nil
		})
		if e != nil {
			return done, e
		}
		done++
	}
	return done, nil
}
