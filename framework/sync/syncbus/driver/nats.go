// Package driver holds the ISyncBus implementations: plain NATS (at most once,
// no confirmation) and JetStream (durable, confirmed). The contract is in
// syncbus; this is the service-to-service sync bus, not entity replication.
package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	fsyncbus "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus"
	fctx "github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

// Bus 是装配层持有的同步总线：ISyncBus 加停机契约 StopWithContext。两种驱动都实现它，
// wiring 的 SyncBusMod 字段就是这个类型，停机在编译期就要求有 StopWithContext，不做运行期断言、
// 也没有不限时的 Stop() 旁路。StopWithContext 返回 nil 之后调用方才能释放下面的 NATS 连接。
type Bus interface {
	fsyncbus.ISyncBus
	StopWithContext(ctx context.Context) error
}

var (
	_ Bus = (*natsSyncBus)(nil)
	_ Bus = (*jetStreamSyncBus)(nil)
)

// natsSyncBus implements fsyncbus.ISyncBus over NATS pub/sub.
type natsSyncBus struct {
	client   nats.IClient
	localSid int32
	prefix   string // NATS subject prefix, e.g. "roost.sync"
}

// NewNatsSyncBus 创建至多一次投递的普通 NATS 同步总线；prefix 为空时取 "roost.sync"。
func NewNatsSyncBus(client nats.IClient, localSid int32, prefix string) Bus {
	if prefix == "" {
		prefix = "roost.sync"
	}
	return &natsSyncBus{client: client, localSid: localSid, prefix: prefix}
}

// StopWithContext 满足 Bus 的停机契约。普通 NATS 总线是至多一次的 pub/sub，不持有订阅之外的资源：
// 每个订阅由 Subscription.Unsubscribe 自己关准入、发 UNSUB 并在 ctx 内排空在途回调，
// 连接归 NatsMod。所以这里没有要等的东西，直接返回 nil，调用方随后即可释放连接。
func (b *natsSyncBus) StopWithContext(context.Context) error {
	return nil
}

func (b *natsSyncBus) Publish(msg *fsyncbus.SyncMsg) error {
	return b.PublishContext(context.Background(), msg)
}

func (b *natsSyncBus) PublishContext(ctx context.Context, msg *fsyncbus.SyncMsg) error {
	if b == nil || b.client == nil {
		return fmt.Errorf("nats sync: bus is not initialized")
	}
	if msg == nil {
		return fmt.Errorf("nats sync: message is nil")
	}
	if strings.TrimSpace(msg.Topic) == "" {
		return fmt.Errorf("nats sync: topic is empty")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	owned := *msg
	owned.Data = append([]byte(nil), msg.Data...)
	if owned.FromSid == 0 {
		owned.FromSid = b.localSid
	}
	data, err := json.Marshal(&owned)
	if err != nil {
		return err
	}
	subject := fmt.Sprintf("%s.%s", b.prefix, owned.Topic)
	return b.client.Publish(subject, data)
}

// Subscribe 在 nats.go 的回调里解码、跳过本服消息，再经 fsyncbus.Subscription.Deliver 调 handler。
// nats.go 的 Unsubscribe 只发 UNSUB、不等回调 goroutine；返回的 Subscription.Unsubscribe(ctx) 先关准入
// 再发 UNSUB，然后在 ctx 内等在途回调。
func (b *natsSyncBus) Subscribe(topic string, handler fsyncbus.Handler) (*fsyncbus.Subscription, error) {
	if b == nil || b.client == nil {
		return nil, fmt.Errorf("nats sync: bus is not initialized")
	}
	if strings.TrimSpace(topic) == "" {
		return nil, fmt.Errorf("nats sync: topic is empty")
	}
	if handler == nil {
		return nil, fmt.Errorf("nats sync: handler is nil")
	}
	subject := fmt.Sprintf("%s.%s", b.prefix, topic)
	// sub 在 client.Subscribe 返回后赋值；release 只会在调用方拿到 local 之后被调用。
	var sub nats.ISubscription
	local := fsyncbus.NewSubscription(topic, handler, func() {
		if sub == nil {
			return
		}
		// 失败（如连接已关）不影响退订：准入已关，之后到达的消息不会再调 handler。
		if err := sub.Unsubscribe(); err != nil {
			slog.Warn("nats sync: unsubscribe failed", "topic", topic, "err", err)
		}
	})
	sub, err := b.client.Subscribe(subject, func(m *nats.Msg) {
		var msg fsyncbus.SyncMsg
		if err := json.Unmarshal(m.Data, &msg); err != nil {
			slog.Warn("nats sync: unmarshal failed", "topic", topic, "err", err)
			return
		}
		// 跳过本服自己发出的消息。
		if msg.FromSid == b.localSid {
			return
		}
		if err := local.Deliver(fctx.BaseContext(), &msg); err != nil && !errors.Is(err, fsyncbus.ErrUnsubscribed) {
			slog.Warn("nats sync: handler error", "topic", topic, "key", msg.Key, "err", err)
		}
	})
	if err != nil {
		return nil, err
	}
	return local, nil
}
