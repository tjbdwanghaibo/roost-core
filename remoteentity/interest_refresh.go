package remoteentity

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	"github.com/tjbdwanghaibo/roost-core/sync/syncbus/mirror"
)

// 兴趣续租请求（O-M6-1，docs/feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md）。
//
// 兴趣是广播软状态。owner 以相同 sid 重启时，兴趣主题的 durable 从游标续读，之前已确认的兴趣消息不重放，
// 兴趣表是空的；只读方的续租要等剩余不足一半才发（缺省最长约 15s），这段时间 owner 不推送，读取只能靠
// 陈旧上限回源。所以 owner 启动（兴趣订阅已确认）后广播一条“请重新续租”，收到的只读方把本机仍有效的
// 兴趣立即续租一次，推送在一次请求往返内恢复。
//
// 这是新主题、新消息：旧版本节点不订阅它，旧只读方按原来的续租周期收敛，旧 owner 不发请求、新只读方
// 行为不变。快照与兴趣主题的格式都没有变。

// SyncTopicInterestRefresh 是兴趣续租请求的主题。
const SyncTopicInterestRefresh = "remote_entity_interest_refresh"

// interestRefreshMinGap 是两次续租遍历开始时刻的最小间隔：请求再密，一个只读方每秒也至多遍历一次。
// 包级变量只为测试可调。
var interestRefreshMinGap = time.Second

// startInterestRefresh 启动一次续租遍历（缺省新 goroutine：遍历要广播 N 条续租，不能占着复制 handler，
// JetStream 要在 AckWait 内拿到回执）。包级变量只为测试改成同步执行。
var startInterestRefresh = func(run func()) { go run() }

// remoteInterestRefreshWire 是请求的消息体。信封的 Key 由 RequesterSID 派生、Version 等于 RequestedAt，
// 两者与消息体逐项核对（同快照 / 兴趣消息的身份绑定）。
type remoteInterestRefreshWire struct {
	RequesterSID int32 `json:"requester_sid"`
	RequestedAt  int64 `json:"requested_at"`
}

// remoteInterestRefreshReplicaKey 是请求的复制 key：每个 requester 一个，非零。
func remoteInterestRefreshReplicaKey(sid int32) int64 { return int64(uint32(sid)) + 1 }

// requestInterestRefresh 由 owner 在 Start 订阅成功之后调用（Assembly.Start）：广播一条续租请求。
// 推送关着（普通 NATS）时不发：没有推送，续租也不必赶。发送失败返回错误，调用方只记日志——退化到原来的
// 续租周期，不让启动失败。可以重复调用（请求幂等）。
func (c *SnapshotClient) requestInterestRefresh(ctx context.Context) error {
	if c == nil || !c.push.Load() {
		return nil
	}
	c.mu.Lock()
	rep := c.refreshRep
	c.mu.Unlock()
	if rep == nil {
		return nil
	}
	if !c.work.Begin() {
		return ErrSnapshotClientStopped
	}
	defer c.work.End()
	now := time.Now().UnixNano()
	raw, err := json.Marshal(remoteInterestRefreshWire{RequesterSID: c.consumerSID, RequestedAt: now})
	if err != nil {
		return err
	}
	err = rep.Publish(ctx, mirror.Envelope{Key: remoteInterestRefreshReplicaKey(c.consumerSID), Version: now, Op: mirror.OpUpsert, Payload: raw})
	result := "sent"
	if err != nil {
		result = "error"
	}
	metrics.IncCounter("remote_entity.remote.interest_refresh_sent_total", metrics.Labels{"result": result}, 1)
	return err
}

// InterestRefreshStore 把兴趣续租请求落到 SnapshotClient：核对身份、丢掉自己的与过期的，然后触发一次
// 续租遍历（合并进行中的）。
type InterestRefreshStore struct{ client *SnapshotClient }

func (s InterestRefreshStore) ApplyReplica(_ context.Context, env mirror.Envelope) error {
	c := s.client
	if c == nil || len(env.Payload) == 0 {
		return nil
	}
	var wire remoteInterestRefreshWire
	if err := json.Unmarshal(env.Payload, &wire); err != nil {
		noteInterestRefreshRequest("invalid")
		return err
	}
	if env.Op != mirror.OpUpsert || wire.RequesterSID == 0 || wire.RequestedAt <= 0 ||
		env.Key != remoteInterestRefreshReplicaKey(wire.RequesterSID) || env.Version != wire.RequestedAt {
		noteInterestRefreshRequest("invalid")
		return fmt.Errorf("remote_entity: interest refresh request identity does not match its payload")
	}
	switch {
	case wire.RequesterSID == c.consumerSID:
		// 自己的请求（JetStream 本来就不投递给自己；进程内总线会）：本机的兴趣表直接收到本机续租，不需要。
		noteInterestRefreshRequest("own")
	case time.Now().UnixNano()-wire.RequestedAt > c.cfg.SnapshotInterestTTL.Nanoseconds():
		// 发出已超过一个兴趣 TTL：那之后每个租约都至少经过了一个续租周期，请求已无意义（同 sid 重启的
		// 只读方从 durable 游标续读到的旧请求在这里被过滤）。
		noteInterestRefreshRequest("historic")
	default:
		noteInterestRefreshRequest(c.acceptInterestRefresh())
	}
	return nil
}

func noteInterestRefreshRequest(result string) {
	metrics.IncCounter("remote_entity.remote.interest_refresh_requests_total", metrics.Labels{"result": result}, 1)
}

// acceptInterestRefresh 安排一次续租遍历：已有遍历在跑时只置待办（它结束后再做一次，多少个请求都合并成
// 这一次）；否则在 work 准入下启动一个遍历。返回计数用的结果。
func (c *SnapshotClient) acceptInterestRefresh() string {
	c.refreshMu.Lock()
	if c.refreshRunning {
		c.refreshPending = true
		c.refreshMu.Unlock()
		return "coalesced"
	}
	if !c.work.Begin() {
		c.refreshMu.Unlock()
		return "stopped"
	}
	c.refreshRunning = true
	c.refreshMu.Unlock()
	startInterestRefresh(c.runInterestRefresh)
	return "accepted"
}

// runInterestRefresh 做续租遍历，直到没有待办为止；两次遍历的开始间隔不小于 interestRefreshMinGap。
// 它持有一次 work 准入：Stop 取消 stopCtx 后它在下一个 key 或间隔等待处退出，Stop 等它返回。
func (c *SnapshotClient) runInterestRefresh() {
	defer c.work.End()
	for {
		c.refreshMu.Lock()
		wait := interestRefreshMinGap - time.Since(c.refreshLastStart)
		c.refreshMu.Unlock()
		if wait > 0 && !c.waitInterestRefreshGap(wait) {
			c.finishInterestRefresh()
			return
		}
		c.refreshMu.Lock()
		c.refreshLastStart = time.Now()
		c.refreshPending = false // 这次遍历覆盖此前到达的全部请求
		c.refreshMu.Unlock()

		c.refreshInterestsOnce()

		c.refreshMu.Lock()
		if !c.refreshPending || c.stopCtx.Err() != nil {
			c.refreshRunning = false
			c.refreshPending = false
			c.refreshMu.Unlock()
			return
		}
		c.refreshMu.Unlock()
	}
}

func (c *SnapshotClient) finishInterestRefresh() {
	c.refreshMu.Lock()
	c.refreshRunning = false
	c.refreshPending = false
	c.refreshMu.Unlock()
}

// waitInterestRefreshGap 等间隔；Stop 时返回 false。
func (c *SnapshotClient) waitInterestRefreshGap(wait time.Duration) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-c.stopCtx.Done():
		return false
	}
}

// refreshInterestsOnce 把本机兴趣表里仍有效的每个 key 经 renewInterest(refresh) 续租一次：与读时续租同一
// 入口（条带锁内分配代际、本机兴趣表按 O4 配额判定、广播），只是不看半衰期门槛。消息数不超过本机兴趣表
// 的 key 数（上限 snapshot_interest_keys），与一个正常续租周期同级。
func (c *SnapshotClient) refreshInterestsOnce() {
	now := time.Now().UnixNano()
	c.localInterestMu.Lock()
	keys := make([]entity.RemoteSnapshotKey, 0, len(c.localInterests))
	for key, expiresAt := range c.localInterests {
		if expiresAt > now {
			keys = append(keys, key)
		}
	}
	c.localInterestMu.Unlock()
	renewed, failed := 0, 0
	for _, key := range keys {
		if c.stopCtx.Err() != nil {
			break
		}
		ctx, cancel := context.WithTimeout(c.stopCtx, c.cfg.SnapshotLoadTimeout)
		published, err := c.renewInterest(ctx, key, true)
		cancel()
		switch {
		case err != nil:
			failed++
		case published:
			renewed++
		}
	}
	metrics.IncCounter("remote_entity.remote.interest_refresh_renewed_total", nil, int64(renewed))
	if failed > 0 {
		slog.Warn("remote_entity: interest refresh could not renew every interest; those keys converge on the regular renewal",
			"consumer_sid", c.consumerSID, "renewed", renewed, "failed", failed)
	}
}
