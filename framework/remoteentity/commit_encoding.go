package remoteentity

import (
	"fmt"
	"math"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// BSON 没有无符号 64 位整数，本包持久化的每个 uint64 都要回答“最高位为 1 怎么办”，答案取决于字段的用途：
//
//	checksum            完整的 64 位哈希，没有任何地方对它比较或区间运算，所以 entity.RemoteChecksum 以 8 字节存储，
//	                    保留整个取值域。
//	version/epoch/fence Mongo 要比较的单调计数（快照读的 $gte、版本 CAS），必须保持数值形式才能保序，所以取值域止于
//	                    MaxInt64，超出的提交在这里拒绝。
//
// 在这里拒绝最关键：无法编码的记录绝不能变成持久的，否则启动恢复每次都会重放它、以同样确定的方式失败——进程再也
// 起不来，唯一出路是删除 WAL。
func validateRemoteCommitEncodable(commit entity.RemoteCommit) error {
	for _, counter := range []struct {
		name  string
		value uint64
	}{
		{"base version", commit.BaseVersion},
		{"next version", commit.NextVersion},
		{"marker epoch", commit.MarkerEpoch},
		{"route epoch", commit.RouteEpoch},
		{"lock fence", commit.LockFence},
	} {
		if counter.value > math.MaxInt64 {
			return fmt.Errorf("%w: %s %d exceeds what BSON can hold (max %d)", entity.ErrRemoteRejected, counter.name, counter.value, uint64(math.MaxInt64))
		}
	}
	for _, mutation := range commit.Mutations {
		if mutation.Version > math.MaxInt64 {
			return fmt.Errorf("%w: mutation %d version %d exceeds what BSON can hold", entity.ErrRemoteRejected, mutation.ID, mutation.Version)
		}
	}
	for _, snapshot := range commit.Snapshots {
		// 有意不检查 checksum：它以字节存储，每个值都能写入，拒绝它会拒掉一半合法的哈希空间。
		for _, counter := range []struct {
			name  string
			value uint64
		}{
			{"state version", snapshot.StateVersion},
			{"base version", snapshot.BaseVersion},
			{"marker epoch", snapshot.MarkerEpoch},
			{"route epoch", snapshot.RouteEpoch},
		} {
			if counter.value > math.MaxInt64 {
				return fmt.Errorf("%w: snapshot %s %d exceeds what BSON can hold", entity.ErrRemoteRejected, counter.name, counter.value)
			}
		}
	}
	return nil
}
