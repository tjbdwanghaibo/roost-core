package gateway

import (
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"strings"
	"testing"
)

type payloadClient struct {
	fnats.RawClient
	limit int64
}

func (c payloadClient) MaxPayload() (int64, error) { return c.limit, nil }
func TestCompleteChannelEnvelopeMustFitActualLimits(t *testing.T) {
	config := DefaultConfig()
	if err := checkChannelSize(config, payloadClient{limit: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	// 裸正文可装下并不代表完整信封可准入，不能启动后随机截断业务包。
	if err := checkChannelSize(config, payloadClient{limit: int64(config.MaxPayloadBytes)}); err == nil || !strings.Contains(err.Error(), "complete") {
		t.Fatalf("NATS envelope overflow=%v", err)
	}
	config.Outbound.MaxPacketBytes = config.MaxPayloadBytes
	if err := checkChannelSize(config, payloadClient{limit: 1 << 20}); err == nil {
		t.Fatal("outbound limit checked only naked payload")
	}
	config = DefaultConfig()
	config.SubscriptionBytes = 8192
	if err := checkChannelSize(config, payloadClient{limit: 1 << 20}); err == nil {
		t.Fatal("subscription bytes cannot hold one complete control/data packet")
	}
	config = DefaultConfig()
	config.ClockSkew = 2 * config.Lease
	if err := config.Validate(); err == nil {
		t.Fatal("unbounded clock skew accepted")
	}
}
