package account

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// TransferSweepInterval 是 account 进程清扫过期换服的间隔（transfer_index.go）。一次换服的期限缺省 10 分钟
// （DefaultTransferTTL），清扫让“过期”最多晚这么久变成“已中止”；在这之间迟到的提交照样被拒绝（那一条在提交里判）。
const TransferSweepInterval = 30 * time.Second

// TransferSweepBatch 是一次清扫至多中止的换服数；积压时按批次处理，不在一次里无界地跑。
const TransferSweepBatch = 100

// transferSweepEvery 是 run 实际用的间隔（测试改短）。
var transferSweepEvery = TransferSweepInterval

// run is the periodic work the account process does.
//
// Sessions need none: a session carries ExpiresAtUnix and ValidateSession
// enforces it on every read, so an expired session is refused the next time it
// is used and nothing a sweep would make true is not already true.
//
// 在途的换服不一样（D8 A，维护者 2026-11-14）：它是存着的记录，占着目标服的名额，期限到了也没有谁的读会
// 让它了结——源服可能再也不来提交。所以这里定期按索引中止过期的换服（SweepExpiredTransfers）。多个 account
// 副本同时清扫无害（中止按角色记录的版本比较落地）。角色存储没有索引（不是 Redis 的那一种）时不清扫，启动时
// 记一条 Warn：期限仍由提交那一侧执行，只是没有人提交的那些不会自己了结。
func (s *Server) run(ctx context.Context) error {
	service, ok := s.Service().(*Service)
	if !ok {
		// Server 只在本地实现上启动；真遇到了，悄悄不清扫就是期限不再生效而没有任何东西失败。
		return fmt.Errorf("account server: the local capability is not a *Service, so no transfer deadline is being enforced")
	}
	if service.transferIndex() == nil {
		slog.Warn("account server: the role store keeps no index of transfers in flight; expired transfers are refused on commit but not swept")
		<-ctx.Done()
		return nil
	}
	ticker := time.NewTicker(transferSweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			swept, err := service.SweepExpiredTransfers(ctx, TransferSweepBatch)
			if err != nil && !errors.Is(err, context.Canceled) {
				// 记下、不返回：下一次清扫重试，为它停掉进程只会把可恢复的积压变成事故。
				slog.Error("account server: sweeping expired transfers failed", "err", err, "swept", swept)
				continue
			}
			if swept > 0 {
				slog.Info("account server: aborted transfers past their deadline", "count", swept)
			}
		}
	}
}
