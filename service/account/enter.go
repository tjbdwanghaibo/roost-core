package account

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

// 进服：登录入口（manifest）在玩家选定一个服之后向 account 要一张**进服票据**，客户端拿它去连那个服的 Gate；
// Gate（或嵌入 TCP 的 game 进程）的握手认证器再拿票据来问 account——这个账号在这个服上的角色是哪一个玩家号，
// 没有就新建这个映射。
//
//	登录入口：IssueEnterTicket(accountID, serverID)   -> 票据（只绑定账号与服）
//	握手认证器：EnterServer(ticket)                    -> 角色（玩家号、归属），以及是不是这一次新建的映射
//
// account 在这条路上管的只是**账号 id ↔ 角色 id**：分配玩家号、占住这个账号在这个服的名额。它不接触角色名——
// 名字怎么起、合不合规、要不要全服唯一，是游戏服自己的数据与规则；这样建出来的角色 Role.Name 是空的。
// 所以这里的建角计划是“无名计划”：slot 的 insert-only 就是唯一性，名字目录的两阶段占用这一步根本不走
// （建角判定表里的 slotPendingUnnamed / entryUnnamed）。
//
// 已经自己做选角的工程照旧用 CreateRole（带名字）/ SelectRole / ValidateSession，两条路并存；
// 同一个名额上两条路不会各建一个：谁先占住 slot，另一条路要么续完它，要么按名额已占回答。
//
// 进服票据的格式与域见 tickets.go。有效期用会话的有效期（Config.SessionTTL，缺省 30 分钟），不另加配置键：
// 它与角色会话令牌是同一种东西（“凭它握手”的短期凭证），寿命该由同一个旋钮管。有效期内可以反复握手
// （断线重连）。

const (
	enterTicketVersion = "et1"
	enterTicketDomain  = "roost.account.enter-ticket\n"
)

// EnterTicket 是 IssueEnterTicket 的结果。
type EnterTicket struct {
	// Ticket 是票据本身，原样交给客户端。
	Ticket string `json:"ticket"`
	// ServerID 是票据绑定的服（玩家选中的服）。
	ServerID int32 `json:"server_id"`
	// ServingServerID 是承载服：ServerID 没有并入别的服时就是它自己，并入了就是那个服。
	// 客户端要连的是承载服的入口；握手时账号在 ServerID 上的角色由承载服的进程服务。
	ServingServerID int32 `json:"serving_server_id"`
	// ExpiresAtUnix 是票据的过期时刻（系统时间）。
	ExpiresAtUnix int64 `json:"expires_at_unix"`
}

// ServerEntry 是 EnterServer 的结果。
type ServerEntry struct {
	// Role 是账号在票据那个服上的角色，照 SelectRole 一样带着承载服、绑定代数与在途的换服。
	// 经这条路新建的角色没有名字（Role.Name 为空）。
	Role Role `json:"role"`
	// Created 为 true 表示“账号 ↔ 玩家号”的映射是这一次调用建出来（发布）的。它只是给握手方的提示：
	// 并发的两次握手里只有一个为 true，重放与重连为 false；建映射的应答丢失后重试时，映射可能在上一次调用里
	// 就发布了，这一次看到的是 false。游戏服要不要为这名玩家建数据，以它自己的档为准，不以这个标记为准。
	Created bool `json:"created,omitempty"`
}

// IssueEnterTicket 给 accountID 签一张进 serverID 的票据。信任模型与 SelectRole 相同：调用方（登录入口）
// 已经用 ValidateAccountTicket 认出这个账号，accountID 是它给的。只读——不建映射、不改任何记录。
//
// 拒绝：账号不存在（ErrAccountMissing）、账号被封（ErrIdentityDenied）、服号不是正数或没登记
// （ErrServerInvalid）、承载服解析不了（ErrServerInvalid / ErrServerMergeCycle）。
//
// 不看区服的状态，也不看账号在那个服有没有角色：维护中、已满、关闭的服上已有的角色照常能进，没有角色的
// 由握手时的 EnterServer 按建角的规则拒绝（ErrServerClosed）。已并入别的服的服照实签给它——那里的老角色
// 要用原服号才问得到，ServingServerID 告诉入口该给哪个服的地址。
func (s *Service) IssueEnterTicket(ctx context.Context, accountID string, serverID int32) (EnterTicket, error) {
	if strings.TrimSpace(accountID) == "" {
		return EnterTicket{}, fmt.Errorf("%w: account id is empty", ErrAccountMissing)
	}
	if serverID <= 0 {
		return EnterTicket{}, fmt.Errorf("%w: server id %d is not positive", ErrServerInvalid, serverID)
	}
	if _, err := s.accountInGoodStanding(ctx, accountID); err != nil {
		return EnterTicket{}, err
	}
	if _, err := s.mustServer(ctx, serverID); err != nil {
		return EnterTicket{}, err
	}
	serving, err := s.servingServer(ctx, serverID)
	if err != nil {
		return EnterTicket{}, err
	}
	expiresAt := s.cfg.SystemNow().Add(s.cfg.SessionTTL).Unix() // 凭证的寿命是系统时间
	body := enterTicketVersion + "." + strconv.FormatInt(expiresAt, 10) + "." + strconv.FormatInt(int64(serverID), 10) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(accountID))
	s.report.Accepted("issue_enter_ticket")
	return EnterTicket{Ticket: s.signTicket(enterTicketDomain, body), ServerID: serverID, ServingServerID: serving, ExpiresAtUnix: expiresAt}, nil
}

// EnterServer 凭进服票据解析“这个账号在这个服上的角色”，没有就新建这个映射，并记一次登录。调用方是握手的
// 认证器（Gate，或嵌入 TCP 的 game 进程）；它与本接口其余方法一样是可信的内部调用方，票据才是客户端出示的凭证。
// **它会写**：新建映射、最近登录时间、换服的收尾。
//
//   - 票据无效——过期、被改、别的密钥签的、格式不对、拿账号票据或角色会话令牌冒充：ErrSessionInvalid，什么也不写。
//   - 账号不存在 / 被封：ErrAccountMissing / ErrIdentityDenied。封号在这里立即生效：已经签出去的票据也进不来。
//   - 账号在那个服上有角色（RoleOnServer 的判定；CreateRole 建的带名字的角色也算）：返回它，Created 为 false。
//   - 没有：新建映射——分配玩家号、占住名额、写下一条没有名字的角色记录。区服的准入与 CreateRole 相同：
//     服不是 open 或已并入别的服答 ErrServerClosed；名额被换服的占位占着答 ErrRoleLimit。
//   - slot 与角色对不上、建角正在对账：ErrConflict（与 RoleOnServer / SelectRole 相同），稍后重试。
//
// 幂等：同一张票据重放、两次握手并发，只会有一个映射——靠的是 slot 的唯一性与建角计划，不是这里的判断。
// 建到一半停下（应答丢失、进程退出）的，下一次 EnterServer 接上同一个计划把它做完，拿到的是同一个玩家号。
// 名额上停着一个 CreateRole 留下的带名字的待定计划时，这里按“同名重试”把它续完（名字在计划里）；
// 那个计划的名字已经被别人占走时得到 ErrNameTaken，计划随之释放，再进一次就建无名的映射。
func (s *Service) EnterServer(ctx context.Context, ticket string) (ServerEntry, error) {
	accountID, serverID, ok := s.verifyEnterTicket(ticket)
	if !ok {
		s.report.Refused("enter_server", "bad_ticket")
		return ServerEntry{}, errTicketInvalid()
	}
	if _, err := s.accountInGoodStanding(ctx, accountID); err != nil {
		s.report.Refused("enter_server", "account")
		return ServerEntry{}, err
	}
	role, err := s.RoleOnServer(ctx, accountID, serverID)
	created := false
	switch {
	case err == nil:
	case errors.Is(err, ErrRoleMissing):
		if err := s.serverAcceptsNewRoles(ctx, serverID); err != nil {
			return ServerEntry{}, err
		}
		if role, created, err = s.createUnnamedRole(ctx, accountID, serverID); err != nil {
			return ServerEntry{}, err
		}
	default:
		return ServerEntry{}, err
	}
	// 与 SelectRole 相同：登录是换服之后一定会走到的写入口，在这里补完上一次换服的收尾；补不完不挡进服。
	s.settleTransferBestEffort(ctx, role)
	now := s.cfg.Now() // 最近登录时间是业务时间
	stored, _, err := s.cfg.Roles.Update(ctx, role.PlayerID, func(current Role, found bool) (Role, bool, error) {
		if !found {
			return current, false, fmt.Errorf("%w: player %d", ErrRoleMissing, role.PlayerID)
		}
		if current.AccountID != accountID {
			return current, false, fmt.Errorf("%w: player %d", ErrNotPermitted, role.PlayerID)
		}
		current.LastLoginAtUnix = now.Unix()
		return current, true, nil
	})
	if err != nil {
		return ServerEntry{}, err
	}
	entered, err := s.withServing(ctx, stored.Value)
	if err != nil {
		return ServerEntry{}, err
	}
	s.report.Accepted("enter_server")
	return ServerEntry{Role: entered, Created: created}, nil
}

// createUnnamedRole 为 accountID 在 serverID 上建（或续完）一个没有名字的角色：只有“账号 ↔ 玩家号”的映射。
// 走的是建角判定表的 entryUnnamed 这一列，与 createRole 共用 slot 的分类、建计划与发布；不碰名字目录。
// published 的含义同 createRole。调用方已经核对过账号与区服的准入。
func (s *Service) createUnnamedRole(ctx context.Context, accountID string, serverID int32) (role Role, published bool, err error) {
	key := slotKeyFor(accountID, serverID)
	holdChecked := false
	for range 3 {
		slot, found, err := s.cfg.Slots.Get(ctx, key)
		if err != nil {
			return Role{}, false, err
		}
		state := classifySlot(slot, found, accountID, serverID)
		if state == slotTransferHold && !holdChecked {
			// 与 createRole 相同：一次已经了结的换服迟到写下的占位不该挡住建角。
			holdChecked = true
			if err := s.releaseStrayHold(ctx, key); err != nil {
				return Role{}, false, err
			}
			continue
		}
		action := decideCreation(state, entryUnnamed, nameNotRead)
		if action == actMakePlan {
			// 名字为空的计划就是无名计划。存下来的是这次的计划，或者并发的另一次请求先插进去的那个。
			if slot, err = s.makeCreationPlan(ctx, key, accountID, serverID, ""); err != nil {
				return Role{}, false, err
			}
			state = classifySlot(slot, true, accountID, serverID)
			action = decideCreation(state, entryUnnamed, nameNotRead)
		}
		switch action {
		case actReturnRole:
			stored, found, err := s.cfg.Roles.Get(ctx, slot.Value.PlayerID)
			if err != nil {
				return Role{}, false, err
			}
			if !found || stored.Value.CreationID != slot.Value.Creation.ID {
				return Role{}, false, fmt.Errorf("%w: completed creation has no matching role", ErrConflict)
			}
			if stored.Value.ServerID != serverID {
				// 角色已经换服走了，这个 slot 是提交之后还没删掉的旧服 slot：收尾补完之前名额仍按已占用回答。
				s.report.Refused("enter_server", "role_limit")
				return Role{}, false, fmt.Errorf("%w: the slot of %s on server %d still belongs to role %d, which moved to server %d; "+
					"its transfer is not settled yet", ErrRoleLimit, accountID, serverID, stored.Value.PlayerID, stored.Value.ServerID)
			}
			return stored.Value.clone(), false, nil
		case actResume:
			if slot.Value.Creation.Name != "" {
				// CreateRole 留下的带名字的待定计划：按同名重试续完（Reserve → admit → Create → Commit → 发布）。
				return s.resumeRoleCreation(ctx, key, slot)
			}
			return s.resumeUnnamedCreation(ctx, key, slot)
		case actRefuseRoleLimit:
			s.report.Refused("enter_server", "role_limit")
			return Role{}, false, fmt.Errorf("%w: role transfer %s holds the slot of %s on server %d", ErrRoleLimit, slot.Value.TransferID, accountID, serverID)
		case actRefuseConflict:
			if state == slotForeign {
				return Role{}, false, fmt.Errorf("%w: slot identity mismatch", ErrConflict)
			}
			return Role{}, false, fmt.Errorf("%w: slot has no creation plan; reconcile it offline", ErrConflict)
		default:
			return Role{}, false, fmt.Errorf("account: creation table answered %d for slot %d on the unnamed entry", action, state)
		}
	}
	return Role{}, false, fmt.Errorf("%w: the slot of %s on server %d keeps changing", ErrConflict, accountID, serverID)
}

// resumeUnnamedCreation 把一个无名计划往前推到发布：写角色记录（insert-only）→ 发布 slot。两步都按计划的身份认记录，
// 所以任何一步之后停下，下一次调用从头再走一遍得到同一个结果。没有名字可占，所以没有“计划再也做不完”的情况；
// 唯一的死路是分配器给了一个别人已经在用的玩家号——那是配置错误，释放 slot 并报 ErrConflict（与带名字的计划相同）。
func (s *Service) resumeUnnamedCreation(ctx context.Context, key string, slot versionstore.Versioned[Slot]) (Role, bool, error) {
	plan := slot.Value.Creation
	role := Role{CreationID: plan.ID, PlayerID: plan.PlayerID, AccountID: slot.Value.AccountID, ServerID: slot.Value.ServerID, CreatedAtUnix: plan.CreatedAtUnix}
	stored, created, err := s.cfg.Roles.Create(ctx, role.PlayerID, role)
	if err != nil {
		return Role{}, false, err
	}
	if !created {
		var found bool
		if stored, found, err = s.cfg.Roles.Get(ctx, role.PlayerID); err != nil {
			return Role{}, false, err
		}
		if !found {
			return Role{}, false, fmt.Errorf("%w: role disappeared during creation recovery", ErrConflict)
		}
	}
	if stored.Value.CreationID != plan.ID || stored.Value.AccountID != role.AccountID || stored.Value.ServerID != role.ServerID {
		_, releaseErr := s.releaseCreationSlot(ctx, key, slot, true)
		return Role{}, false, errors.Join(fmt.Errorf("%w: player id %d is already in use", ErrConflict, role.PlayerID), s.compensated(releaseErr))
	}
	role = stored.Value.clone()
	published, err := s.publishRole(ctx, key, plan, role)
	if err != nil {
		return role, false, err
	}
	s.report.Accepted("enter_server.create")
	return role.clone(), published, nil
}

// PeekEnterTicket 读出进服票据里写的服号，**不验证签名与过期**。握手方可以用它在调 EnterServer 之前
// 做不涉及身份的预判（例如独立 Gate 按服号挑后端）。读到的值不能当作身份——票据是不是真的只有 EnterServer
// 说了算。形状不对时 ok 为 false。
func PeekEnterTicket(ticket string) (serverID int32, ok bool) {
	parts, ok := splitTicket(enterTicketVersion, 5, ticket)
	if !ok {
		return 0, false
	}
	_, serverID, ok = decodeEnterTicketBody(parts)
	return serverID, ok
}

// decodeEnterTicketBody 解出账号与服号；服号必须是正数的规范十进制写法，账号不能为空。
func decodeEnterTicketBody(parts []string) (accountID string, serverID int32, ok bool) {
	sid, err := strconv.ParseInt(parts[2], 10, 32)
	if err != nil || sid <= 0 || strconv.FormatInt(sid, 10) != parts[2] {
		return "", 0, false
	}
	account, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil || len(account) == 0 {
		return "", 0, false
	}
	return string(account), int32(sid), true
}

// verifyEnterTicket 校验一张进服票据，返回它绑定的账号与服。
func (s *Service) verifyEnterTicket(ticket string) (accountID string, serverID int32, ok bool) {
	parts, ok := s.verifyTicket(enterTicketDomain, enterTicketVersion, 5, ticket)
	if !ok {
		return "", 0, false
	}
	return decodeEnterTicketBody(parts)
}
