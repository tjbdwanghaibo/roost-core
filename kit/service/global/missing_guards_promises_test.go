package global

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// U-0150 · C2 · gap map kit `service/global` 4/20：Redis 存储缺前缀不能构造；完成 / 中止迁移与续租对未知
// 游戏服报 ErrRouteMissing / ErrLeaseMissing。
func TestMigrationAndLeaseOperationsRefuseUnknownGames(t *testing.T) {
	ctx := context.Background()
	if _, err := NewRedisStores(nil, "  "); err == nil || !strings.Contains(err.Error(), "key prefix is required") {
		t.Fatalf("NewRedisStores with a blank prefix = %v", err)
	}
	service, _ := newService(t)
	if _, err := service.CompleteMigration(ctx, 999, 1); !errors.Is(err, ErrRouteMissing) {
		t.Fatalf("CompleteMigration for an unknown game = %v", err)
	}
	if _, err := service.AbortMigration(ctx, 999, 1); !errors.Is(err, ErrRouteMissing) {
		t.Fatalf("AbortMigration for an unknown game = %v", err)
	}
	if _, err := service.RenewLease(ctx, 999, "inc-1", nil); !errors.Is(err, ErrLeaseMissing) {
		t.Fatalf("RenewLease for an unknown game = %v", err)
	}
}
