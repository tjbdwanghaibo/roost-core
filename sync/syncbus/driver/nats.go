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

	fctx "github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/nats"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
)

// natsSyncBus implements fsyncbus.ISyncBus over NATS pub/sub.
type natsSyncBus struct {
	client   nats.IClient
	localSid int32
	prefix   string // NATS subject prefix, e.g. "roost.sync"
}

func NewNatsSyncBus(client nats.IClient, localSid int32, prefix string) fsyncbus.ISyncBus {
	if prefix == "" {
		prefix = "roost.sync"
	}
	return &natsSyncBus{client: client, localSid: localSid, prefix: prefix}
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
// 再发 UNSUB，然后在 ctx 内等在途回调（A3 ②）。
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
		// Skip messages from self
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
