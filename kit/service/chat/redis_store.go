package chat

import (
	"fmt"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// NewRedisStore builds a chat Store over Redis.
//
// No storage logic: a channel's ring and its sequence live in ONE versioned
// entry — which is what makes a sequence number that disagrees with the
// messages it numbers impossible — and kit's versionstore.RedisStore already
// implements that contract.
//
// It returns a ready Store rather than the state store because the state's
// value type is unexported.
//
// One key per channel means contention on a busy channel is contention on one
// key, reported as chat's "append" conflict counter. That is a deliberate
// trade against the alternative the implementation this replaces made: a
// single global sequence document, where EVERY channel's writes serialized on
// one key and each message cost two round trips.
func NewRedisStore(client versionstore.RedisClient, prefix string, cfg Config) (Store, error) {
	if strings.TrimSpace(prefix) == "" {
		return nil, fmt.Errorf("chat: redis key prefix is required")
	}
	state, err := versionstore.NewRedisStore(client, versionstore.RedisConfig[string, channelState]{
		Prefix: prefix + ":ch:",
		KeyOf:  func(key string) string { return key },
		Codec:  versionstore.JSONCodec[channelState]{},
	})
	if err != nil {
		return nil, fmt.Errorf("chat: channel state: %w", err)
	}
	return NewStore(state, cfg)
}
