package chat

import (
	"context"
	domain "github.com/tjbdwanghaibo/roost-core/service/chat"
	"testing"
)

// 配置验收使用显式测试协作者；生产没有默认允许所有请求的权限策略。
type allowAllPolicy struct{}

func (allowAllPolicy) CanPublish(context.Context, domain.Sender, domain.Channel) error { return nil }
func (allowAllPolicy) CanRead(context.Context, domain.Sender, domain.Channel) error    { return nil }
func testRegistry(t *testing.T) *domain.BodyRegistry                                   { t.Helper(); return domain.NewBodyRegistry() }
func grantingAuth() domain.SystemAuthenticator {
	return domain.SystemAuthenticatorFunc(func(context.Context) (domain.SystemToken, error) { return domain.GrantSystem(), nil })
}
