package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
)

// SingletonIdentityChecker 核对远端当前持锁的进程代次。Live 只能回答 SID 是否活，
// 不能据此信任发现缓存里的旧 incarnation；Matches 也不代表该进程 ingress 已 ready。
type SingletonIdentityChecker interface {
	Matches(context.Context, string, int32, string) (bool, error)
}

const ModSingletonIdentityChecker ModName = "singleton_identity_checker"

func (s singletonLiveness) Matches(ctx context.Context, serverType string, sid int32, incarnation string) (bool, error) {
	if ctx == nil || strings.TrimSpace(serverType) == "" || sid <= 0 || incarnation == "" || len(incarnation) > 128 {
		return false, errors.New("app: singleton identity: context, server type, positive sid and bounded incarnation are required")
	}
	values, err := s.store.Get(ctx, []string{singletonKey(s.prefix, serverType, sid)})
	if err != nil {
		return false, fmt.Errorf("app: singleton identity: %w", err)
	}
	if len(values) != 1 {
		return false, fmt.Errorf("app: singleton identity: store returned %d values for one key", len(values))
	}
	if len(values[0]) == 0 {
		return false, nil
	}
	token, _, ok := bytes.Cut(values[0], []byte("|"))
	if !ok || len(token) == 0 {
		return false, errors.New("app: singleton identity: malformed lock value")
	}
	return string(token) == incarnation, nil
}
