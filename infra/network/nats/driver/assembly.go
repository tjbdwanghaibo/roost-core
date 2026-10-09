package driver

import (
	"context"
	"errors"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/internal/operation"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

// ErrClosedUndrained reports that Assembly.Close closed the connection hard
// because draining it failed: the budget ran out, or the connection was
// already closed or reconnecting. It wraps the drain error, so a budget that
// ran out still matches context.DeadlineExceeded / context.Canceled. Unlike the
// ctx error of RPC callback waiting it is terminal: the connection is gone and
// a later Close cannot drain it again (RR-20261004-08).
var ErrClosedUndrained = errors.New("nats: connection closed before drain finished")

// rpcCallbackWorkers is the size of the RPC callback pool every process
// assembles; it was a literal in the kit Mod before P3b.
const rpcCallbackWorkers = 4

// Assembly is the complete NATS capability set a process publishes: the core
// connection, JetStream on that connection and the request/reply RPC client.
// The bus is assembled by the kit Mod from registry configuration on top of
// Client and RPC; everything that needs driver internals lives here.
type Assembly struct {
	Client    *Client
	JetStream *JetStreamClient
	RPC       *RPCClient

	// closeSerial 串行化 Close。Assembly 没有自己的“已关闭”标记：连接是否已关闭只看 Client 的判据
	// （见 Client 的类型注释），RPC 的停止是幂等的。
	closeSerial operation.Serial
}

// Assemble connects and builds JetStream and RPC on the connection. When
// JetStream cannot be initialised the connection is closed again rather than
// left for the caller to find.
func Assemble(cfg *fnats.Config, extra ClientOptions) (*Assembly, error) {
	client, err := NewClient(cfg, extra)
	if err != nil {
		return nil, err
	}
	jetStream, err := NewJetStreamClient(client)
	if err != nil {
		client.Close()
		return nil, err
	}
	return &Assembly{
		Client:    client,
		JetStream: jetStream,
		RPC:       NewRPCClient(client, fnats.DefaultRetryPolicy(), rpcCallbackWorkers),
	}, nil
}

// Connected reports whether the underlying connection is up (health check).
func (a *Assembly) Connected() bool {
	return a != nil && a.Client.Connected()
}

// Close stops the RPC client (failing every pending call with ErrCancelled)
// and waits for callbacks within ctx before draining the connection. When
// callback waiting expires, the assembly retains ownership for a later Close.
// When the connection drain fails or does not finish in time, the connection
// is closed hard and the drain error is returned wrapped in
// ErrClosedUndrained; that result is terminal, the assembly then owns nothing
// a later Close could still release. The bus must already be stopped by the
// caller — it owns subscriptions on Client.
//
// Close 幂等：连接一旦关闭（排空完成，或排空失败被硬关），之后的 Close 返回 nil，ErrClosedUndrained
// 只报给关掉连接的那一次调用（RR-20261006-10）。判断“已关闭”用 Client 自己的已关闭状态，与 Client
// 的其他方法同一个判据；直接调过 Client.Close 的，Assembly.Close 同样按重复 Close 返回 nil。
// 并发调用串行执行，后到者在自己的 ctx 内等第一个做完，ctx 先结束返回 ctx 错误、保留所有权（与等
// RPC 回调超时同样处理，可以再调用）。
func (a *Assembly) Close(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := a.closeSerial.Lock(ctx); err != nil {
		return err
	}
	defer a.closeSerial.Unlock()
	if a.RPC != nil {
		if err := a.RPC.StopWithContext(ctx); err != nil {
			// RPC 回调仍在运行、连接仍在使用：再次 Close 继续等同一次排空。
			return err
		}
	}
	if a.Client == nil || a.Client.closed() {
		return nil
	}
	if err := a.Client.DrainWithContext(ctx); err != nil {
		// The connection is closed from here on. Returning the bare ctx
		// error made callers keep the assembly for a retry whose drain can
		// only fail with nats.go's ErrConnectionClosed (RR-20261004-08).
		a.Client.Close()
		return fmt.Errorf("%w: %w", ErrClosedUndrained, err)
	}
	return nil
}
