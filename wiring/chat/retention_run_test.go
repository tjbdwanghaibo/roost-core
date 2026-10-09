package chat

import (
	"context"
	"errors"
	"fmt"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	domain "github.com/tjbdwanghaibo/roost-core/service/chat"
	"testing"
)

// 仅解析频道，不允许发起 Redis 请求；嵌入的 nil 方法在误用时直接失败。
type unusedRedis struct{ fredis.IRedis }

// Configuration names shared channels as kind:target. A malformed entry, or
// a pair-scoped kind that needs a participant, stops the process at startup
// rather than being skipped on every tick.
func TestPruneChannelsConfigurationFailsClosed(t *testing.T) {
	store, err := domain.NewRedisStore(&unusedRedis{}, "test:chat", domain.Config{Policy: allowAllPolicy{}, Bodies: domain.NewBodyRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"world", "world:", ":1", "world:abc", "world:-1"} {
		if _, err := parsePruneChannels([]string{bad}); err == nil {
			t.Errorf("entry %q was accepted", bad)
		}
	}
	channels, err := parsePruneChannels([]string{"world:1", " group:42 "})
	if err != nil || len(channels) != 2 || channels[1] != (domain.Channel{Kind: domain.ChannelGroup, Target: 42}) {
		t.Fatalf("parsed %+v err=%v", channels, err)
	}
	if _, err := resolvePruneChannels(store, []domain.Channel{{Kind: domain.ChannelPrivate, Target: 7}}); !errors.Is(err, domain.ErrChannelInvalid) {
		t.Fatalf("a pair-scoped kind in chat.prune_channels was resolved: %v", err)
	}
	refs, err := resolvePruneChannels(store, channels)
	if err != nil || len(refs) != 2 || refs[0].Key() != "world:1" {
		t.Fatalf("refs=%v err=%v", refs, err)
	}

	cfg := modConfig()
	cfg.Set("chat.prune_channels", []string{"world:oops"})
	mod := NewMod(allowAllPolicy{}, domain.NewBodyRegistry(), domain.SystemAuthenticatorFunc(func(context.Context) (domain.SystemToken, error) {
		return domain.SystemToken{}, fmt.Errorf("no system path")
	}), nil, nil)
	if err := mod.Init(cfg); err == nil {
		t.Fatal("Init accepted a malformed chat.prune_channels entry")
	}
}
