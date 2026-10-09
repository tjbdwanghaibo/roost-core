package dataengine

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// U-0148 · C2 · gap map core `dataengine` 7/20：租约围栏回执缺 id / 空载荷 / 非 JSON 报
// ErrInvalidLeaseFence（且识别为围栏命名空间）；Loader 无存储报 ErrStoreRequired；模板空资源名 /
// 重名 / 依赖未知资源报 ErrLoadDependency；WaitProjection 无票报错；版本 0 不能推进。
func TestLeaseFenceLoaderAndTrackerGuards(t *testing.T) {
	ctx := context.Background()
	for name, receipt := range map[string]Receipt{
		"no id":      {Namespace: LeaseFenceReceiptNamespace, Payload: []byte(`{}`)},
		"empty body": {Namespace: LeaseFenceReceiptNamespace, ID: "db/res/1"},
		"not json":   {Namespace: LeaseFenceReceiptNamespace, ID: "db/res/1", Payload: []byte("{")},
	} {
		if _, isFence, err := DecodeLeaseFenceReceipt(receipt); !isFence || !errors.Is(err, ErrInvalidLeaseFence) {
			t.Fatalf("%s: DecodeLeaseFenceReceipt = (fence=%v, %v)", name, isFence, err)
		}
	}
	if _, isFence, err := DecodeLeaseFenceReceipt(Receipt{Namespace: "other"}); isFence || err != nil {
		t.Fatalf("a receipt from another namespace = (fence=%v, %v)", isFence, err)
	}

	var none *Loader
	if err := none.LoadAll(ctx, nil); !errors.Is(err, ErrStoreRequired) {
		t.Fatalf("LoadAll on a nil loader = %v", err)
	}
	if err := NewLoader(nil, loaderExister{}).LoadAll(ctx, nil); !errors.Is(err, ErrStoreRequired) {
		t.Fatalf("LoadAll without a store = %v", err)
	}
	loader := NewLoader(&loaderStore{}, loaderExister{})
	onLoad := func(RawDocument) error { return nil }
	cases := []struct {
		name      string
		templates []LoadTemplate
		text      string
	}{
		{"empty resource", []LoadTemplate{{Database: "game", OnLoad: onLoad}}, "template 0 has empty resource"},
		{"duplicate resource", []LoadTemplate{{Database: "game", Resource: "heroes", OnLoad: onLoad}, {Database: "game", Resource: "heroes", OnLoad: onLoad}}, `duplicate resource "heroes"`},
		{"unknown dependency", []LoadTemplate{{Database: "game", Resource: "heroes", DependsOn: []string{"players"}, OnLoad: onLoad}}, `depends on unknown resource "players"`},
	}
	for _, tc := range cases {
		if err := loader.LoadAll(ctx, tc.templates); !errors.Is(err, ErrLoadDependency) || !strings.Contains(err.Error(), tc.text) {
			t.Fatalf("%s: LoadAll = %v", tc.name, err)
		}
	}
	if err := loader.LoadAll(ctx, []LoadTemplate{{Database: "game", Resource: "heroes", OnLoad: onLoad}}); err != nil {
		t.Fatalf("valid template refused: %v", err)
	}

	if err := WaitProjection(ctx, nil); err == nil || !strings.Contains(err.Error(), "projection ticket is required") {
		t.Fatalf("WaitProjection(nil) = %v", err)
	}
	tracker := &Tracker{}
	if err := tracker.AdvanceVersion(0); !errors.Is(err, ErrInvalidVersion) {
		t.Fatalf("AdvanceVersion(0) = %v", err)
	}
	if err := tracker.AdvanceVersion(3); err != nil || tracker.Version() != 3 {
		t.Fatalf("AdvanceVersion(3) = %v, version=%d", err, tracker.Version())
	}
}
