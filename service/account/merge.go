package account

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
)

// 别名合服（DESIGN-TODO D15，设计 docs/maintenance/FEATURE-2026-10-20-server-merge-alias.md）。
//
// 把服 A 并入服 B：角色身份（PlayerID、Role.ServerID、slot、名字）一律不变，account 在 A 的服务器记录上
// 写 MergedInto = B，此后 A 的角色由 B 的 game 进程承载。“承载服”只在本文件的 servingServer 一处解析，
// 所有返回 Role / Session 的出口都经它填 ServingServerID；Gate 与嵌入 TCP 的认证器按它路由与驻留，game
// 进程仍只认自己的一个 sid。
//
// 记录只有一跳（拍平，维护者 2026-10-20 决定）：A 并入 B 时，原来并入 A 的服一并改指 B；B 自己不能已并入
// 别处。解析因此只看一跳，遇到第二跳就是数据被手工改坏或两次合服并发，拒绝而不猜。
//
// 合服是停服窗口内的离线操作，由一个运维按 runbook（docs/framework/PLAYER-MIGRATION.md §6）执行；MergeServer
// 不上总线（没有 //roost:rpc），只在持有 account 存储凭据的进程里调用（game-demo 的 cmd/accountctl）。
// 每条记录的写是版本比较（versionstore.Update 的 CAS，mutate 对当前值重新校验），跨记录的写不是原子的：
// 写入顺序保证任何中途停下的状态都不会产生两跳，结束时再复核一次，并发合服造成的两跳报 ErrServerMergeCycle
// 并给出修复命令。

// MergeRequest 是一次别名合服：把 From 并入 Into。Note 必填（操作者备注，写进 From 的记录）。
// DryRun 只回答会改哪些记录，不写。
type MergeRequest struct {
	From, Into int32
	Note       string
	DryRun     bool
}

// MergeResult 是 MergeServer / UnmergeServer 的结论。
type MergeResult struct {
	From int32 `json:"from"`
	// Into 是合服的目标服；UnmergeServer 时是解除之前的承载服（没有合服时为 0）。
	Into int32 `json:"into"`
	// Repointed 是原来并入 From、这次改指 Into 的服（拍平），按 ID 排序。
	Repointed []int32 `json:"repointed,omitempty"`
	// Changed 为 false 表示记录本来就是目标状态（重复执行同一操作，幂等成功）。
	Changed bool `json:"changed"`
	DryRun  bool `json:"dry_run,omitempty"`
	// Servers 是操作之后（DryRun 时是操作之前）相关的服务器记录：From、Into 与 Repointed。
	Servers []GameServer `json:"servers"`
}

// MergeServer 把 request.From 并入 request.Into（拍平），见文件头注释。
//
// 拒绝（什么也不写）：From / Into 不是正数、相同或不存在（ErrServerInvalid）；Into 自己已并入别处、或
// 会形成环（ErrServerMergeCycle）；From 已并入另一个服（ErrConflict，先 UnmergeServer）；备注缺失或超长
// （ErrAdminNoteRequired）。同一对重复执行幂等成功，Changed 为 false；上次中途停下的会在这次补完。
//
// 写入顺序，以及为什么：
//
//  1. Into 的 MergedFrom 加上 From 与要改指的服。索引先于指针：中途停下只多出一项索引，解析以 MergedInto 为准，
//     拍平时也先核对 MergedInto 再改。
//  2. 原来并入 From 的服逐个改指 Into（只改 MergedInto 仍是 From 或已是 Into 的）。先于第 3 步：反过来中途停下会留下
//     X → From → Into 两跳。
//  3. From 写 MergedInto = Into、Status = closed、合服时间与备注，清空它的 MergedFrom。
//  4. 复核 Into 仍没有 MergedInto。
func (s *Service) MergeServer(ctx context.Context, request MergeRequest) (MergeResult, error) {
	from, into := request.From, request.Into
	result := MergeResult{From: from, Into: into, DryRun: request.DryRun}
	if from <= 0 || into <= 0 {
		return result, fmt.Errorf("%w: merge needs two positive server ids, got %d into %d", ErrServerInvalid, from, into)
	}
	if from == into {
		return result, fmt.Errorf("%w: server %d cannot be merged into itself", ErrServerInvalid, from)
	}
	note, err := validateAdminNote(request.Note)
	if err != nil {
		return result, err
	}
	source, err := s.mustServer(ctx, from)
	if err != nil {
		return result, err
	}
	target, err := s.mustServer(ctx, into)
	if err != nil {
		return result, err
	}
	if err := refuseMergeTarget(from, target); err != nil {
		return result, err
	}
	if source.MergedInto != 0 && source.MergedInto != into {
		return result, fmt.Errorf("%w: server %d is already merged into %d; unmerge-server -from %d first", ErrConflict, from, source.MergedInto, from)
	}
	// 要改指的服：From 的索引里、MergedInto 仍指向 From（或上次中途停下、已改指 Into）的。
	var repoint []GameServer
	for _, id := range source.MergedFrom {
		if id == into {
			continue
		}
		member, found, err := s.cfg.Servers.Get(ctx, id)
		if err != nil {
			return result, err
		}
		if found && (member.Value.MergedInto == from || member.Value.MergedInto == into) {
			repoint = append(repoint, member.Value.clone())
			result.Repointed = append(result.Repointed, id)
		}
	}
	slices.Sort(result.Repointed)
	wantMembers := append([]int32{from}, result.Repointed...)
	result.Changed = source.MergedInto != into || source.Status != ServerClosed || len(source.MergedFrom) != 0 ||
		!containsAll(target.MergedFrom, wantMembers) || slices.ContainsFunc(repoint, func(member GameServer) bool { return member.MergedInto != into })
	if request.DryRun || !result.Changed {
		result.Servers = append([]GameServer{source, target}, repoint...)
		return result, nil
	}

	nowUnix := s.cfg.SystemNow().Unix() // 运维记录，系统时间
	updatedTarget, _, err := s.cfg.Servers.Update(ctx, into, func(current GameServer, found bool) (GameServer, bool, error) {
		if !found {
			return current, false, fmt.Errorf("%w: server %d is unknown", ErrServerInvalid, into)
		}
		if err := refuseMergeTarget(from, current); err != nil {
			return current, false, err
		}
		if containsAll(current.MergedFrom, wantMembers) {
			return current, false, nil
		}
		next := current.clone()
		next.MergedFrom = unionSorted(next.MergedFrom, wantMembers)
		next.UpdatedAtUnix = nowUnix
		return next, true, nil
	})
	if err != nil {
		return result, err
	}
	servers := []GameServer{updatedTarget.Value.clone()}
	for _, member := range repoint {
		updated, _, err := s.cfg.Servers.Update(ctx, member.ID, func(current GameServer, found bool) (GameServer, bool, error) {
			// 读到之后被别人解除或改指别处的，不再归 From 管：跳过，索引里多出的那一项按文件头的约定无害。
			if !found || current.MergedInto != from {
				return current, false, nil
			}
			next := current.clone()
			next.MergedInto = into
			next.MergedAtUnix, next.MergeNote, next.UpdatedAtUnix = nowUnix, note, nowUnix
			return next, true, nil
		})
		if err != nil {
			return result, fmt.Errorf("account: repoint server %d from %d to %d (re-run the same merge to finish): %w", member.ID, from, into, err)
		}
		servers = append(servers, updated.Value.clone())
	}
	updatedSource, _, err := s.cfg.Servers.Update(ctx, from, func(current GameServer, found bool) (GameServer, bool, error) {
		if !found {
			return current, false, fmt.Errorf("%w: server %d is unknown", ErrServerInvalid, from)
		}
		if current.MergedInto != 0 && current.MergedInto != into {
			return current, false, fmt.Errorf("%w: server %d was merged into %d meanwhile", ErrConflict, from, current.MergedInto)
		}
		if current.MergedInto == into && current.Status == ServerClosed && len(current.MergedFrom) == 0 {
			return current, false, nil
		}
		next := current.clone()
		next.MergedInto, next.Status, next.MergedFrom = into, ServerClosed, nil
		next.MergedAtUnix, next.MergeNote, next.UpdatedAtUnix = nowUnix, note, nowUnix
		return next, true, nil
	})
	if err != nil {
		return result, fmt.Errorf("account: write the alias of server %d (re-run the same merge to finish): %w", from, err)
	}
	servers = append([]GameServer{updatedSource.Value.clone()}, servers...)
	result.Servers = servers

	// 第 4 步：跨记录不是原子的，承载服可能在途中被另一次合服并入别处。
	final, err := s.mustServer(ctx, into)
	if err != nil {
		return result, err
	}
	if final.MergedInto != 0 {
		slog.Error("account: server merge left a two-hop alias", "from", from, "into", into, "into_merged_into", final.MergedInto)
		return result, fmt.Errorf("%w: server %d was merged into %d while %d was being merged into it; repair with "+
			"accountctl unmerge-server -from %d, then merge-server -from %d -into %d", ErrServerMergeCycle, into, final.MergedInto, from, from, from, final.MergedInto)
	}
	slog.Info("account: server merged", "from", from, "into", into, "repointed", result.Repointed, "note", note)
	s.report.Accepted("admin.merge_server")
	return result, nil
}

// UnmergeServer 解除 from 的合服：清空它的 MergedInto，从原承载服的 MergedFrom 去掉它。Status 保持
// closed：原服要先重新部署（新的 WAL 目录、按 sid 独占的数据搬回，见 runbook 的回退），再由运维
// UpsertServer 改成 open。拍平时一并改指的其他服不受影响（它们直接指向承载服）。没有合服时幂等成功。
func (s *Service) UnmergeServer(ctx context.Context, from int32, note string) (MergeResult, error) {
	result := MergeResult{From: from}
	if from <= 0 {
		return result, fmt.Errorf("%w: server id %d is not positive", ErrServerInvalid, from)
	}
	note, err := validateAdminNote(note)
	if err != nil {
		return result, err
	}
	source, err := s.mustServer(ctx, from)
	if err != nil {
		return result, err
	}
	into := source.MergedInto
	result.Into = into
	if into == 0 {
		result.Servers = []GameServer{source}
		return result, nil
	}
	nowUnix := s.cfg.SystemNow().Unix()
	// 先清指针、再清索引：中途停下只多出一项索引，按约定无害。
	updated, _, err := s.cfg.Servers.Update(ctx, from, func(current GameServer, found bool) (GameServer, bool, error) {
		if !found {
			return current, false, fmt.Errorf("%w: server %d is unknown", ErrServerInvalid, from)
		}
		if current.MergedInto == 0 {
			return current, false, nil
		}
		next := current.clone()
		next.MergedInto = 0
		next.MergedAtUnix, next.MergeNote, next.UpdatedAtUnix = nowUnix, note, nowUnix
		return next, true, nil
	})
	if err != nil {
		return result, err
	}
	result.Changed = true
	result.Servers = []GameServer{updated.Value.clone()}
	host, _, err := s.cfg.Servers.Update(ctx, into, func(current GameServer, found bool) (GameServer, bool, error) {
		if !found || !slices.Contains(current.MergedFrom, from) {
			return current, false, nil
		}
		next := current.clone()
		next.MergedFrom = slices.DeleteFunc(next.MergedFrom, func(id int32) bool { return id == from })
		next.UpdatedAtUnix = nowUnix
		return next, true, nil
	})
	if err != nil {
		return result, fmt.Errorf("account: drop server %d from the merge index of %d (re-run unmerge to finish): %w", from, into, err)
	}
	result.Servers = append(result.Servers, host.Value.clone())
	slog.Info("account: server unmerged", "from", from, "was_into", into, "note", note)
	s.report.Accepted("admin.unmerge_server")
	return result, nil
}

// GetServer 返回一个服的记录（含合服字段）；不存在时 ErrServerInvalid。它在 Accounts 上：game 进程启动时
// 经总线用它确认本服没有被合并（wiring/account 的 ServingGuardMod），accountctl show-server 也用它。服务器
// 记录是登录前就公开的列表信息，读它不需要调用方身份。
func (s *Service) GetServer(ctx context.Context, serverID int32) (GameServer, error) {
	if serverID <= 0 {
		return GameServer{}, fmt.Errorf("%w: server id %d is not positive", ErrServerInvalid, serverID)
	}
	return s.mustServer(ctx, serverID)
}

// servingServer 是承载服的唯一解析点：原服没有合服（或没有记录）时就是原服；已并入时是 MergedInto，且
// 只看一跳——承载服记录缺失报 ErrServerInvalid，承载服自己也有 MergedInto（第二跳）报 ErrServerMergeCycle
// 并记 Error。两种都是“不知道该由谁承载”，角色登录失败，不猜一个服。
func (s *Service) servingServer(ctx context.Context, origin int32) (int32, error) {
	server, found, err := s.cfg.Servers.Get(ctx, origin)
	if err != nil {
		return 0, err
	}
	if !found || server.Value.MergedInto == 0 {
		return origin, nil
	}
	into := server.Value.MergedInto
	host, found, err := s.cfg.Servers.Get(ctx, into)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("%w: server %d is merged into %d, which is unknown", ErrServerInvalid, origin, into)
	}
	if host.Value.MergedInto != 0 {
		slog.Error("account: a merge alias has two hops; roles of the origin are refused until it is repaired",
			"origin", origin, "into", into, "into_merged_into", host.Value.MergedInto)
		return 0, fmt.Errorf("%w: server %d is merged into %d, which is itself merged into %d", ErrServerMergeCycle, origin, into, host.Value.MergedInto)
	}
	return into, nil
}

// withServing 返回带承载服的角色副本（存储里的记录不带它）。
func (s *Service) withServing(ctx context.Context, role Role) (Role, error) {
	serving, err := s.servingServer(ctx, role.ServerID)
	if err != nil {
		return Role{}, err
	}
	role = role.clone()
	role.ServingServerID = serving
	return role, nil
}

func (s *Service) mustServer(ctx context.Context, id int32) (GameServer, error) {
	server, found, err := s.cfg.Servers.Get(ctx, id)
	if err != nil {
		return GameServer{}, err
	}
	if !found {
		return GameServer{}, fmt.Errorf("%w: server %d is unknown", ErrServerInvalid, id)
	}
	return server.Value.clone(), nil
}

// refuseMergeTarget：目标服自己已并入别处（拍平之后记录只能有一跳），或已并入 from（会形成环）。
func refuseMergeTarget(from int32, target GameServer) error {
	switch {
	case target.MergedInto == from:
		return fmt.Errorf("%w: server %d is merged into %d; merging %d into it would form a cycle", ErrServerMergeCycle, target.ID, from, from)
	case target.MergedInto != 0:
		return fmt.Errorf("%w: server %d is itself merged into %d; merge %d into %d instead", ErrServerMergeCycle, target.ID, target.MergedInto, from, target.MergedInto)
	}
	return nil
}

func containsAll(set, want []int32) bool {
	for _, id := range want {
		if !slices.Contains(set, id) {
			return false
		}
	}
	return true
}

func unionSorted(set, add []int32) []int32 {
	out := append([]int32(nil), set...)
	for _, id := range add {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}
