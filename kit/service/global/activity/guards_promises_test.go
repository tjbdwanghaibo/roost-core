package activity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// U-0150 · C2 · gap map kit `service/global/activity` 4/20：运维重开派发拒绝非正 game sid；Redis 存储缺前缀 /
// 预留 ttl 不能构造；空组 id 不能列待办活动。
func TestActivityEntryPointsRefuseBlankAndNonPositiveArguments(t *testing.T) {
	ctx := context.Background()
	service, _ := newActivityService(t)
	key := Key{GroupID: "group-a", ActivityID: "a-1", Phase: PhaseClose}
	if _, err := service.ReopenDispatch(ctx, key, 0, "ops reviewed"); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "game sid must be positive") {
		t.Fatalf("ReopenDispatch with game sid 0 = %v", err)
	}
	if _, err := service.PendingActivities(ctx, "", 10); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "group id is empty") {
		t.Fatalf("PendingActivities with a blank group = %v", err)
	}
	if _, err := NewRedisStores(nil, "  ", time.Minute); err == nil || !strings.Contains(err.Error(), "key prefix is required") {
		t.Fatalf("NewRedisStores with a blank prefix = %v", err)
	}
	if _, err := NewRedisStores(nil, "activity", 0); err == nil || !strings.Contains(err.Error(), "reservation ttl must be positive") {
		t.Fatalf("NewRedisStores with ttl 0 = %v", err)
	}
}
