package skillsync

import (
	"errors"
	"testing"
)

// RR-20261005-NC-216：SchemaRange 的 Min 为 0 表示空区间（Contains 对任何版本都返回
// false，NewApplier 也把 Min 0 当作“未配置”）。旧 NegotiateSchema 只在两边合并后的
// minimum 为 0 时失败，一边 Min 0、另一边 Min 非 0 时返回一个那一边 Contains 为
// false 的版本，协商结果不被双方同时支持。协商出的版本必须同时落在两边区间内。
func TestNegotiateSchemaRequiresBothRangesToContainTheResult(t *testing.T) {
	cases := []struct{ server, client SchemaRange }{
		{SchemaRange{Min: 0, Max: 5}, SchemaRange{Min: 2, Max: 3}},
		{SchemaRange{Min: 2, Max: 3}, SchemaRange{Min: 0, Max: 5}},
		{SchemaRange{Min: 4, Max: 2}, SchemaRange{Min: 1, Max: 5}},
	}
	for _, test := range cases {
		version, err := NegotiateSchema(test.server, test.client)
		if err == nil {
			t.Fatalf("NegotiateSchema(%v, %v) = %d; server contains=%v client contains=%v, want ErrSchemaNegotiationFailed", test.server, test.client, version, test.server.Contains(version), test.client.Contains(version))
		}
		if !errors.Is(err, ErrSchemaNegotiationFailed) {
			t.Fatalf("err = %v, want ErrSchemaNegotiationFailed", err)
		}
	}
	// 控制：两边都是有效区间时取交集的最大值。
	if version, err := NegotiateSchema(SchemaRange{Min: 1, Max: 3}, SchemaRange{Min: 2, Max: 4}); err != nil || version != 3 {
		t.Fatalf("NegotiateSchema(valid) = %d, %v; want 3", version, err)
	}
}
