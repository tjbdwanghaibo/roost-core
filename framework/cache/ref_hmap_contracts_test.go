package cache

// RR-20261004-NC-16～19：支持的类型必须往返，Patch成功必须可见；坏布局在写前拒绝。

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
)

type refContractPointerText int

func (v *refContractPointerText) MarshalText() ([]byte, error) {
	return []byte(fmt.Sprintf("code=%d", *v)), nil
}
func (v *refContractPointerText) UnmarshalText(raw []byte) error {
	var n int
	_, err := fmt.Sscanf(string(raw), "code=%d", &n)
	*v = refContractPointerText(n)
	return err
}

type refContractValueText int

func (v refContractValueText) MarshalText() ([]byte, error) {
	return []byte(fmt.Sprintf("code=%d", v)), nil
}
func (v *refContractValueText) UnmarshalText(raw []byte) error {
	var n int
	_, err := fmt.Sscanf(string(raw), "code=%d", &n)
	*v = refContractValueText(n)
	return err
}

type refContractPointerTextRecord struct {
	ID     int64
	Amount refContractPointerText
}
type refContractValueTextRecord struct {
	ID     int64
	Amount refContractValueText
}
type refContractRootAlias struct {
	ID   int64 `json:"id"`
	Root struct {
		ID int64 `json:"id"`
	} `json:"root"`
}
type refContractReservedField struct {
	ID   int64
	Data string `redisdao:"__keys"`
}

func TestRefHMapContractsRealRedis(t *testing.T) {
	addr := os.Getenv("ROOST_REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("requires an isolated Redis")
	}
	client, err := redisdriver.NewClient(fredis.DefaultConfig(addr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"value_control", "pointer_root", "pointer_text", "value_text_control", "patch_nil_parent", "patch_existing_control", "root_alias", "reserved_field"} {
		t.Run(scenario, func(t *testing.T) {
			prefix := "roost:refContract:" + rand.Text()
			defer func() {
				if caught := recover(); caught != nil {
					t.Errorf("supported generic store panicked: %v", caught)
				}
			}()
			switch scenario {
			case "pointer_root":
				store := NewRedisRefHMapStore[int64, *refHMapSession](client, RefHMapConfig[int64, *refHMapSession]{Prefix: prefix, Name: "record", StoreConfig: StoreConfig[int64, *refHMapSession]{KeyOf: func(v *refHMapSession) int64 { return v.ID }}})
				t.Cleanup(func() { _ = store.Delete(context.Background(), 1) })
				want := &refHMapSession{ID: 1, Version: 2}
				if err := store.Set(ctx, want); err != nil {
					t.Fatal(err)
				}
				got, held, err := store.Get(ctx, 1)
				if err != nil || !held || !reflect.DeepEqual(got, want) {
					t.Fatalf("pointer roundtrip: %+v %v %v", got, held, err)
				}
			case "pointer_text":
				store := NewRedisRefHMapStore(client, RefHMapConfig[int64, refContractPointerTextRecord]{Prefix: prefix, Name: "record", StoreConfig: StoreConfig[int64, refContractPointerTextRecord]{KeyOf: func(v refContractPointerTextRecord) int64 { return v.ID }}})
				t.Cleanup(func() { _ = store.Delete(context.Background(), 1) })
				want := refContractPointerTextRecord{ID: 1, Amount: 7}
				if err := store.Set(ctx, want); err != nil {
					t.Fatal(err)
				}
				got, held, err := store.Get(ctx, 1)
				t.Logf("pointer TextMarshaler roundtrip: %+v held=%v err=%v", got, held, err)
				if err != nil || !held || got != want {
					t.Fatal("recognized text scalar did not roundtrip")
				}
			case "value_text_control":
				store := NewRedisRefHMapStore(client, RefHMapConfig[int64, refContractValueTextRecord]{Prefix: prefix, Name: "record", StoreConfig: StoreConfig[int64, refContractValueTextRecord]{KeyOf: func(v refContractValueTextRecord) int64 { return v.ID }}})
				t.Cleanup(func() { _ = store.Delete(context.Background(), 1) })
				want := refContractValueTextRecord{ID: 1, Amount: 7}
				if err := store.Set(ctx, want); err != nil {
					t.Fatal(err)
				}
				got, held, err := store.Get(ctx, 1)
				if err != nil || !held || got != want {
					t.Fatalf("value TextMarshaler: %+v %v %v", got, held, err)
				}
			case "root_alias":
				store := NewRedisRefHMapStore(client, RefHMapConfig[int64, refContractRootAlias]{Prefix: prefix, Name: "record", StoreConfig: StoreConfig[int64, refContractRootAlias]{KeyOf: func(v refContractRootAlias) int64 { return v.ID }}})
				t.Cleanup(func() { _ = store.Delete(context.Background(), 1) })
				want := refContractRootAlias{ID: 1}
				want.Root.ID = 99
				if err := store.Set(ctx, want); !errors.Is(err, ErrRefHMapUnsupported) {
					t.Fatalf("RR-NC-19: root collision err=%v, want unsupported before write", err)
				}
				if exists, err := client.Exists(ctx, prefix+":{record:1}:root"); err != nil || exists != 0 {
					t.Fatalf("rejected layout wrote data: exists=%d err=%v", exists, err)
				}

			case "reserved_field":
				store := NewRedisRefHMapStore(client, RefHMapConfig[int64, refContractReservedField]{Prefix: prefix, Name: "record", StoreConfig: StoreConfig[int64, refContractReservedField]{KeyOf: func(v refContractReservedField) int64 { return v.ID }}})
				// Registered keys are precisely this test's root; do not delete arbitrary keys from the payload.
				rootKey := prefix + ":{record:1}:root"
				t.Cleanup(func() { _, _ = client.Del(context.Background(), rootKey) })
				want := refContractReservedField{ID: 1, Data: "payload"}
				if err := store.Set(ctx, want); !errors.Is(err, ErrRefHMapUnsupported) {
					t.Fatalf("RR-NC-19: reserved field err=%v, want unsupported before write", err)
				}
				if exists, err := client.Exists(ctx, rootKey); err != nil || exists != 0 {
					t.Fatalf("rejected layout wrote data: exists=%d err=%v", exists, err)
				}

			default:
				store := NewRedisRefHMapStore(client, RefHMapConfig[int64, refHMapSession]{Prefix: prefix, Name: "record", StoreConfig: refHMapSessionConfig()})
				t.Cleanup(func() { _ = store.Delete(context.Background(), 1) })
				want := refHMapSession{ID: 1, Version: 2}
				if scenario != "patch_nil_parent" {
					want.Meta = &refHMapMeta{Label: "before"}
				}
				if err := store.Set(ctx, want); err != nil {
					t.Fatal(err)
				}
				if scenario != "value_control" {
					if err := store.Patch(ctx, 1, "Meta.Label", "after"); err != nil {
						t.Fatal(err)
					}
					want.Meta = &refHMapMeta{Label: "after"}
				}
				got, held, err := store.Get(ctx, 1)
				t.Logf("nested patch: got=%+v Meta=%+v held=%v err=%v", got, got.Meta, held, err)
				if err != nil || !held || !reflect.DeepEqual(got, want) {
					t.Fatal("successful nested patch was not observable")
				}
			}
		})
	}
}
