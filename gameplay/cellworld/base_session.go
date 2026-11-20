package cellworld

import (
	"context"
	"errors"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// BaseSession 是本进程 TCP 接受连接时生成的身份。账号 SessionID 可跨重连复用；
// ConnectionID 是物理连接 nonce，Admission 按接受顺序严格递增（不是客户端的 Generation）。
type BaseSession struct {
	SessionID    string `msgpack:"s"`
	ConnectionID string `msgpack:"c"`
	Admission    uint64 `msgpack:"a"`
}

type SessionState struct {
	BaseSession
	OfflineAt int64 `msgpack:"off"` // 首次确认离线的 UnixNano；重复通知不延长宽限。
}

var ErrStaleSession = errors.New("cellworld: this connection does not own the player session")

// EnsureEntered 在一笔 strict 事务里复用同化身的 Entry，已离场时优先用有效快照位置。
// 无会话的旧调用方仍可用 Enter（重复进场仍报错）。
func (n *Node) EnsureEntered(ctx context.Context, base, actor int64, spawn spatial.Point, hp int32) (BaseState, error) {
	return n.enterState(ctx, base, baseCmd{kind: cmdEnsureEnter, actor: actor, pos: spawn, hp: hp})
}

// AdmitSession 同时提交入场与当前连接身份。等待超时非成功，调用方可用同一连接重试。
func (n *Node) AdmitSession(ctx context.Context, base, actor int64, spawn spatial.Point, hp int32, session BaseSession) (BaseState, error) {
	return n.enterState(ctx, base, baseCmd{kind: cmdAdmit, actor: actor, pos: spawn, hp: hp, session: session})
}

func (n *Node) enterState(ctx context.Context, base int64, cmd baseCmd) (BaseState, error) {
	out, err := n.cfg.Nest.Request(ctx, hBaseEnter, base, nest.Params{cmd})
	if err != nil {
		return BaseState{}, err
	}
	st, ok := out.(BaseState)
	if !ok {
		return st, errors.New("cellworld: missing committed entry result")
	}
	return st, nil
}

func (n *Node) MoveSession(ctx context.Context, base int64, session BaseSession, vx, vy int64) error {
	_, err := n.cfg.Nest.Request(ctx, hBaseCmd, base, nest.Params{baseCmd{kind: cmdSessionMove, session: session, vx: vx, vy: vy}})
	return err
}

// DisconnectSession 只把关闭事件投入 Nest，不等待。身份比较与置零都在同一 Player 事务内执行；
// 队列满或回滚由 BaseBeat 的本地连接检查补偿。
func (n *Node) DisconnectSession(ctx context.Context, base int64, sessionID, connectionID string) error {
	return n.cfg.Nest.Dispatch(ctx, hBaseCmd, base, nest.Params{baseCmd{kind: cmdDisconnect, session: BaseSession{SessionID: sessionID, ConnectionID: connectionID}, closedAt: time.Now()}})
}

func (n *Node) sessionLive(p BasePlayer, s BaseSession) bool {
	return s.Admission != 0 && s.SessionID != "" && s.ConnectionID != "" && n.cfg.SessionActive != nil && n.cfg.SessionActive(p.ID(), s.ConnectionID)
}

func (n *Node) admitSession(p BasePlayer, st *BaseState, cmd baseCmd, fx *txEffects, now time.Time) error {
	old, next := st.Session, cmd.session
	if !n.sessionLive(p, next) || next.Admission < old.Admission || (next.Admission == old.Admission && (next != old.BaseSession || old.OfflineAt != 0)) {
		return ErrStaleSession
	}
	if st.InWorld && st.Actor != cmd.actor {
		return errors.New("cellworld: session actor mismatch")
	}
	if n.graceElapsed(st, now) && st.InWorld {
		n.leaveBase(p, st, now, fx)
	}
	// 接替连接时旧的停步可能还在排队；在绑定新连接的同一事务内清除旧速度。
	if st.InWorld && next != old.BaseSession && (st.VX != 0 || st.VY != 0) {
		n.setVelocity(st, 0, 0, now, fx)
	}
	st.Session = SessionState{BaseSession: next}
	return nil
}

func (n *Node) graceElapsed(st *BaseState, now time.Time) bool {
	return st.Session.OfflineAt != 0 && now.Sub(time.Unix(0, st.Session.OfflineAt)) >= n.cfg.ReconnectGrace
}

func (n *Node) setVelocity(st *BaseState, vx, vy int64, now time.Time, fx *txEffects) {
	st.IntentSeq++
	st.VX, st.VY = vx, vy
	if st.Cell != 0 {
		fx.send(st.Cell, n.inputMsg(st, now))
	}
}
func (n *Node) disconnect(st *BaseState, closedAt, now time.Time, fx *txEffects) {
	st.Session.OfflineAt = closedAt.UnixNano()
	n.setVelocity(st, 0, 0, now, fx)
}
func (n *Node) leaveBase(p BasePlayer, st *BaseState, now time.Time, fx *txEffects) {
	if st.Cell != 0 {
		fx.send(st.Cell, &wireMsg{Kind: kindLeave, Leave: &leaveReq{ID: st.Actor, Entry: st.Entry}})
	}
	leaving := *st
	fx.hooks = append(fx.hooks, func() { n.unshow(&leaving, now) })
	st.Shown = false
	st.GroupViewID, st.GroupViewVer = 0, 0
	st.InWorld, st.Cell = false, 0
	// 保留最后的 admission 高水位：仍连着的旧账号会话不能在新会话到期后复活。
	if st.Session.Admission != 0 {
		if p, ok := p.(interface{ StopBase() }); ok {
			p.StopBase()
		}
	}
}
