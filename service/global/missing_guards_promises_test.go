package global

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// U-0150 · C2 · gap map kit `service/global` 4/20：Redis 存储缺前缀不能构造；完成 / 中止迁移对未知
// 游戏服报 ErrRouteMissing。（原来还钉住续租报 ErrLeaseMissing，租约 API 已删除。）
func TestMigrationOperationsRefuseUnknownGames(t *testing.T) {
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
}
