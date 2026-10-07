package skill

import "sort"

// 衍生物记录按分区存放（维护者第十三轮“skill 衍生物两张表”，docs/feature/REFACTOR-2026-10-07-skill-spawn-partition.md）。
//
// 之前 Runtime 有两张 map：spawns 存全部记录，ownedSpawns 是其中“已移交、仍在运行”那部分的重复索引，指向同一批
// *SpawnInstance；两张表要在移交、停止、待停止、回收时一起维护，checkpoint 也存两份、恢复时逐条比对。现在“移交给谁”
// 只由记录字段（Owner、handedOff）表达，记录按字段分进五个分区，每条记录恰好在一个分区里：
//
//	分区              字段                               谁按它查
//	spawnCasting      Status == running，未移交          施法收尾 / 打断 / goto 停衍生物，移交，施法期间 lifecycle 失效回收；逐 tick 推进
//	spawnHandedOff    Status == running，已移交          OwnedSpawns；逐 tick 推进、到期 / 失效回收与施放中分区相同（spawnSteppedPartitions）
//	spawnStopPending  Status == stop_pending             退避重试，待停止条目上限，RetentionStats
//	spawnStopped      Status == ended/cancelled/failed   只是历史：随 cast 回收、checkpoint、状态快照
//	spawnAbandoned    Status == abandoned                待停止超过 MaxStopPendingSpawns 时放弃的；Advance 末尾按 MaxAbandonedSpawns 清理
//
// 只有 setState 改分区字段（Status、handedOff）并把记录挪到对应分区；新记录经 add 入表、按字段落分区，删除经 drop。
// spawn_partition_promises_test.go 的源码守卫禁止别处写分区 map、给这两个字段赋值。
// drop 只有三个调用点（同一守卫登记）：Advance 末尾清理已放弃分区（pruneAbandonedSpawnsLocked）、随 cast 回收已停止的
// 记录（forgetCastSpawnsLocked）、衍生物启动失败且已停掉之后（startEntitySpawn）。任何“取 ID 列表 → 逐个处理”的循环
// 中途都不会删记录（维护者第十三轮“待停止上限”选 B，docs/feature/REFACTOR-2026-10-07-skill-spawn-partition.md §11）。
type spawnPartition uint8

const (
	spawnCasting spawnPartition = iota
	spawnHandedOff
	spawnStopPending
	spawnStopped
	spawnAbandoned
	spawnPartitionCount
)

// spawnLivePartitions 是衍生物仍在宿主侧运行、且由 Runtime 负责的分区（liveOnHost）：钉住所属 cast、占用 owned
// 衍生物容量，Shutdown / RemoveProgram 要停它们。已放弃分区不在其中：宿主侧可能仍在运行，但 Runtime 不再负责。
var spawnLivePartitions = []spawnPartition{spawnCasting, spawnHandedOff, spawnStopPending}

// spawnSteppedPartitions 是 Runtime 逐 tick 推进的分区：运行中的衍生物不论是否已移交，都由 advanceOwnedSpawns 这一个
// 入口按 NextTick 推进、按 EndTick / 运动完成回收（RR-20261006-51）。之前只推进已移交分区，施放中的衍生物在移交之前
// 一步都不走，移交前的 tick 永久丢失。是否移交只决定停止、表现与 revision 记在谁名下（spawnOwnerCast）。
var spawnSteppedPartitions = []spawnPartition{spawnCasting, spawnHandedOff}

// partition 由记录字段决定记录所在的分区。checkpoint 恢复只接受已知的 Status（validSpawnStatus）。
func (spawn *SpawnInstance) partition() spawnPartition {
	switch spawn.Status {
	case SpawnRunning:
		if spawn.handedOff {
			return spawnHandedOff
		}
		return spawnCasting
	case SpawnStopPending:
		return spawnStopPending
	case SpawnAbandoned:
		return spawnAbandoned
	default:
		return spawnStopped
	}
}

func validSpawnStatus(status SpawnStatus) bool {
	switch status {
	case SpawnRunning, SpawnStopPending, SpawnEnded, SpawnCancelled, SpawnFailed, SpawnAbandoned:
		return true
	}
	return false
}

type spawnTable struct {
	partitions [spawnPartitionCount]map[SpawnID]*SpawnInstance
}

func newSpawnTable() spawnTable {
	var table spawnTable
	for index := range table.partitions {
		table.partitions[index] = make(map[SpawnID]*SpawnInstance)
	}
	return table
}

// setState 是唯一改衍生物分区字段（Status、handedOff）的地方，改完把记录挪到字段对应的分区。记录不在表里时
// （已被 drop，或测试直接构造、没有入表的记录）只改字段、不入表：被删掉的记录不能因为一次迟到的状态变化回到表里。
func (table *spawnTable) setState(spawn *SpawnInstance, status SpawnStatus, handedOff bool) {
	current := table.partitions[spawn.partition()]
	filed := current[spawn.ID] == spawn
	if filed {
		delete(current, spawn.ID)
	}
	spawn.Status, spawn.handedOff = status, handedOff
	if filed {
		table.partitions[spawn.partition()][spawn.ID] = spawn
	}
}

// add 把新记录（启动的衍生物、checkpoint 恢复出的记录）按字段放进分区。ID 已在表里时返回 false、不入表。
func (table *spawnTable) add(spawn *SpawnInstance) bool {
	if table.get(spawn.ID) != nil {
		return false
	}
	table.partitions[spawn.partition()][spawn.ID] = spawn
	return true
}

// drop 把记录从表里删掉（Advance 末尾清理已放弃分区、cast 回收已停止的记录、启动失败且已停的记录）。
func (table *spawnTable) drop(id SpawnID) {
	if spawn := table.get(id); spawn != nil {
		delete(table.partitions[spawn.partition()], id)
	}
}

// get 按 ID 查记录：只在给出的分区里找，不给分区时依次查全部分区。
func (table *spawnTable) get(id SpawnID, partitions ...spawnPartition) *SpawnInstance {
	for _, partition := range orAllSpawnPartitions(partitions) {
		if spawn := table.partitions[partition][id]; spawn != nil {
			return spawn
		}
	}
	return nil
}

// count 返回给出分区的记录数，不给分区时是全部记录数。
func (table *spawnTable) count(partitions ...spawnPartition) int {
	total := 0
	for _, partition := range orAllSpawnPartitions(partitions) {
		total += len(table.partitions[partition])
	}
	return total
}

// each 对给出分区（不给时全部分区）的每条记录调用 visit，顺序不定；visit 不能增删记录、不能改分区字段。
// 要按顺序处理、或处理中会停止 / 删除记录的调用方用 sortedIDs 先取 ID，再逐个 get。
func (table *spawnTable) each(visit func(*SpawnInstance), partitions ...spawnPartition) {
	for _, partition := range orAllSpawnPartitions(partitions) {
		for _, spawn := range table.partitions[partition] {
			visit(spawn)
		}
	}
}

// sortedIDs 按 ID 升序返回给出分区（不给时全部分区）里满足 match 的记录 ID；match 为 nil 时全部返回。
func (table *spawnTable) sortedIDs(match func(*SpawnInstance) bool, partitions ...spawnPartition) []SpawnID {
	ids := make([]SpawnID, 0)
	table.each(func(spawn *SpawnInstance) {
		if match == nil || match(spawn) {
			ids = append(ids, spawn.ID)
		}
	}, partitions...)
	sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
	return ids
}

var allSpawnPartitions = []spawnPartition{spawnCasting, spawnHandedOff, spawnStopPending, spawnStopped, spawnAbandoned}

func orAllSpawnPartitions(partitions []spawnPartition) []spawnPartition {
	if len(partitions) == 0 {
		return allSpawnPartitions
	}
	return partitions
}
