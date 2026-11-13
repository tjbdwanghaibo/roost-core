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

// SingletonLease 在本地检查本次进程的有效写入窗口，不访问 Redis。
// 租约到期或已失锁会永久触发 fail-stop；业务准入、提交和出站都可以检查，
// 不能依赖续期 goroutine 比长暂停后的业务 goroutine 更早获得调度。
type SingletonLease interface{ Check() error }

const ModSingletonLease ModName = "singleton_lease"

func (l *singletonLock) Check() error {
	l.mu.Lock()
	if l.status.state == singletonLost {
		cause := l.status.err
		l.mu.Unlock()
		return fmt.Errorf("%w: %v", ErrSingletonLost, cause)
	}
	if l.status.validUntil.IsZero() || !l.clock.Now().Before(l.status.validUntil) {
		cause := errors.New("local singleton validity window expired")
		l.status.state, l.status.err = singletonLost, cause
		l.mu.Unlock()
		l.reportLost(cause, nil)
		return ErrSingletonLost
	}
	l.mu.Unlock()
	return nil
}
