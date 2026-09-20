package entity

import (
	"strings"
	"testing"
)

// M-04 · remote=capable 与 remote=true 不再被接受。
//
// 它们过去映射成 entity.RemotePolicyCapable,而那个值的全部作用就是"把这个 kind 放进
// 第一个锁档"。锁档现在是 kind 的 category,所以那个值说不出 category 说不了的事,已从框架删除。
// 必须报错而不是静默降级成 none:none 会改变一个已有实体的锁档。
func TestRemoteCapableMarkerIsRejectedWithTheCategoryReplacement(t *testing.T) {
	for _, value := range []string{"capable", "true", "yes", "1", "on"} {
		err := validateMarkerValues(map[string]string{"remote": value})
		if err == nil {
			t.Errorf("remote=%s was accepted; it must be refused", value)
			continue
		}
		for _, want := range []string{"no longer supported", "category"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("remote=%s error %q does not mention %q", value, err, want)
			}
		}
	}

	// The surviving spellings still parse, and an unrelated typo still gets the
	// plain "not one of" message rather than the migration hint.
	for _, value := range []string{"none", "managed", "mirror", "no", "false", "off", ""} {
		if err := validateMarkerValues(map[string]string{"remote": value}); err != nil {
			t.Errorf("remote=%q must still be accepted: %v", value, err)
		}
	}
	err := validateMarkerValues(map[string]string{"remote": "bogus"})
	if err == nil || strings.Contains(err.Error(), "no longer supported") {
		t.Fatalf("remote=bogus should get the plain spelling error, got %v", err)
	}
	if !strings.Contains(err.Error(), "none|managed|mirror") {
		t.Errorf("remote=bogus error %q does not list the accepted spellings", err)
	}
}
