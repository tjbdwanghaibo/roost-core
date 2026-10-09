package global

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

// RedisStores are the stores this package needs, over Redis.
//
// There is no storage logic here, and for this package that is the entire
// point of the design. The implementation this replaces had FOUR hand-written
// stores per concern — a Redis variant whose Update used compare-and-set and a
// DAO variant whose Update read and then wrote unconditionally — satisfying
// one interface, so the type system could not tell them apart and whichever
// was configured decided whether the documented invariant held. Here there is
// one implementation, in kit, whose contract has no unconditional write.
type RedisStores struct {
	Routes versionstore.Store[int32, RouteBinding]
}

// NewRedisStores builds them.
//
// The route store gets no TTL: a TTL on versioned state takes the version with
// the value, so a binding that expired and was written again would restart at
// version 1 — and a binding that vanished would let a second Bind succeed.
//
// Keys live under <prefix>:route:. The <prefix>:lease: keyspace belonged to
// the removed lease API; nothing reads or writes it any more, and leftover
// keys from an older deployment can be deleted.
func NewRedisStores(client versionstore.RedisClient, prefix string) (RedisStores, error) {
	if strings.TrimSpace(prefix) == "" {
		return RedisStores{}, fmt.Errorf("global: redis key prefix is required")
	}
	int32Key := func(id int32) string { return strconv.FormatInt(int64(id), 10) }

	var (
		stores RedisStores
		err    error
	)
	if stores.Routes, err = versionstore.NewRedisStore(client, versionstore.RedisConfig[int32, RouteBinding]{
		Prefix: prefix + ":route:", KeyOf: int32Key, Codec: routeCodec{},
	}); err != nil {
		return RedisStores{}, fmt.Errorf("global: route store: %w", err)
	}
	return stores, nil
}

// 路由 v2 保存完成回执；没有回执的旧记录不能靠 epoch 猜测完成还是撤销。
type routeRecord struct {
	Version int `json:"version"`
	RouteBinding
}
type routeCodec struct{}

func (routeCodec) Encode(binding RouteBinding) ([]byte, error) {
	return json.Marshal(routeRecord{Version: 2, RouteBinding: binding})
}
func (routeCodec) Decode(data []byte) (RouteBinding, error) {
	var record routeRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return RouteBinding{}, err
	}
	if record.Version != 2 {
		return RouteBinding{}, fmt.Errorf("global: unsupported route version %d; clear old routes before upgrade", record.Version)
	}
	return record.RouteBinding, nil
}
