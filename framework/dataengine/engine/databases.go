package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
)

// ErrDatabaseMappingChanged：WAL 里还有未投影的记录，而本次的逻辑库映射与写下它们的进程不同。
// 继续启动会把这些记录投到新的物理库，而文档的前序版本在旧库（版本条件不满足即 fatal，或新库里凭空
// 出现半份数据）。处理：用原来的映射启动、排空 WAL（正常停机即全部投影确认），迁移数据后再改映射。
var ErrDatabaseMappingChanged = errors.New("dataengine: database mapping changed while the WAL still holds unprojected records")

// reconcileDatabaseMapping 在打开 WAL 之后、开始回放之前核对并记下本次映射（nestwal.DatabaseMappingFile）。
//
// 上次映射里出现过的每个逻辑库，本次换算出的物理库都必须相同；本次新增的逻辑库不受限制（旧记录不会用到它）。
// 目录里还没有映射文件时，上次进程按“物理名就是逻辑名”投影（引入映射之前的版本都是如此），所以本次任何
// 非 identity 的映射都视为改变。WAL 已排空（没有未投影记录）时映射可以随意改，只记下新的。
func reconcileDatabaseMapping(ctx context.Context, wal *nestwal.WAL, databases *coredata.DatabaseMap) error {
	previous, found, err := nestwal.ReadDatabaseMapping(wal.Dir())
	if err != nil {
		return fmt.Errorf("dataengine: %w", err)
	}
	pending, err := wal.HasUnacknowledgedRecords(ctx)
	if err != nil {
		return fmt.Errorf("dataengine: check unprojected WAL records: %w", err)
	}
	if pending {
		var changed []string
		if found {
			for _, logical := range slices.Sorted(maps.Keys(previous)) {
				if now := databases.Physical(logical); now != previous[logical] {
					changed = append(changed, fmt.Sprintf("%s: %s -> %s", logical, previous[logical], now))
				}
			}
		} else {
			for _, entry := range databases.Entries() {
				if entry.Physical != entry.Logical {
					changed = append(changed, fmt.Sprintf("%s: %s (no %s, written before database mappings) -> %s", entry.Logical, entry.Logical, nestwal.DatabaseMappingFile, entry.Physical))
				}
			}
		}
		if len(changed) > 0 {
			return fmt.Errorf("%w (WAL %s): %s; start with the previous mapping and stop gracefully to drain the WAL, migrate the data, then change the mapping",
				ErrDatabaseMappingChanged, wal.Dir(), strings.Join(changed, "; "))
		}
	}
	if err := wal.WriteDatabaseMapping(databases.Physicals()); err != nil {
		return fmt.Errorf("dataengine: %w", err)
	}
	return nil
}
