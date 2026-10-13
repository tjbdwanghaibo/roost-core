package nestwal

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"

	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
)

const (
	checkpointMagic   = uint32(0x52534350) // RSCP
	checkpointVersion = uint16(1)
	checkpointSize    = 36
)

// checkpointState 是确认进度：fence 之前（含）的记录都已确认，可以被清理。
// 两个槽轮换写入（generation 奇偶决定槽位），加载时取合法槽中 generation 最大者。
type checkpointState struct {
	generation uint64
	fence      corenest.CommitFence
}

// loadCheckpoint 读取两个槽并取 generation 最大的合法槽；都不存在或都不合法时返回零值。
func loadCheckpoint(dir string) (checkpointState, error) {
	var best checkpointState
	found := false
	for slot := range 2 {
		state, err := readCheckpoint(filepath.Join(dir, checkpointName(slot)))
		if err != nil {
			// 允许一个槽写坏：另一个槽在替换内容持久之前一直保留。
			continue
		}
		if !found || state.generation > best.generation {
			best = state
			found = true
		}
	}
	return best, nil
}

func readCheckpoint(path string) (checkpointState, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return checkpointState{}, err
	}
	if len(raw) != checkpointSize {
		return checkpointState{}, io.ErrUnexpectedEOF
	}
	if binary.BigEndian.Uint32(raw[0:4]) != checkpointMagic || binary.BigEndian.Uint16(raw[4:6]) != checkpointVersion {
		return checkpointState{}, errors.New("nestwal: invalid checkpoint header")
	}
	want := binary.BigEndian.Uint32(raw[32:36])
	if crc32.ChecksumIEEE(raw[:32]) != want {
		return checkpointState{}, errors.New("nestwal: invalid checkpoint checksum")
	}
	return checkpointState{
		generation: binary.BigEndian.Uint64(raw[8:16]),
		fence: corenest.CommitFence{
			Segment: binary.BigEndian.Uint64(raw[16:24]),
			Offset:  int64(binary.BigEndian.Uint64(raw[24:32])),
		},
	}, nil
}

// storeCheckpoint 以“写临时文件 → fsync → 改名 → fsync 目录”持久化 state 到它的槽位。
func storeCheckpoint(dir string, state checkpointState, mode os.FileMode) error {
	raw := make([]byte, checkpointSize)
	binary.BigEndian.PutUint32(raw[0:4], checkpointMagic)
	binary.BigEndian.PutUint16(raw[4:6], checkpointVersion)
	binary.BigEndian.PutUint64(raw[8:16], state.generation)
	binary.BigEndian.PutUint64(raw[16:24], state.fence.Segment)
	binary.BigEndian.PutUint64(raw[24:32], uint64(state.fence.Offset))
	binary.BigEndian.PutUint32(raw[32:36], crc32.ChecksumIEEE(raw[:32]))

	slot := int(state.generation & 1)
	target := filepath.Join(dir, checkpointName(slot))
	tmp := target + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if err = writeFull(file, raw); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := replaceCheckpointFile(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDirectory(dir)
}

func checkpointName(slot int) string {
	if slot == 0 {
		return "ack-0.chk"
	}
	return "ack-1.chk"
}

// fenceAfter 报告 left 在日志中是否严格位于 right 之后。
func fenceAfter(left, right corenest.CommitFence) bool {
	return left.Segment > right.Segment || left.Segment == right.Segment && left.Offset > right.Offset
}
