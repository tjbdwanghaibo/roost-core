package nestwal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"

	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// DatabaseMappingFile 记录写下本目录记录的进程把哪些逻辑库映射到哪些物理库（DESIGN-TODO D9 修订版 A）。
//
// WAL 记录里的库名是 DAO 的逻辑库名，投影时才按映射换成物理库。映射在 WAL 还有未投影记录时改变，
// 这些记录就会投到新的物理库，而它们的前序版本在旧库——所以 DataEngine 启动时比较这里记下的映射与本次的映射，
// 有未投影记录且映射变了就拒绝启动（改映射要先停写、排空 WAL、迁移数据，见 docs/framework/PLAYER-MIGRATION.md）。
const DatabaseMappingFile = "databases.json"

const databaseMappingVersion = 1

type databaseMappingDocument struct {
	Version   int               `json:"version"`
	Databases map[string]string `json:"databases"`
}

// ReadDatabaseMapping 读 dir 里上次启动记下的映射（逻辑名 → 物理名）。文件不存在时 found 为 false；
// 内容不可解析或版本不认识时返回错误，不猜测。
func ReadDatabaseMapping(dir string) (databases map[string]string, found bool, err error) {
	raw, err := os.ReadFile(filepath.Join(dir, DatabaseMappingFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("nestwal: read %s: %w", DatabaseMappingFile, err)
	}
	var document databaseMappingDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, false, errors.Join(ErrCorrupt, fmt.Errorf("nestwal: decode %s: %w", DatabaseMappingFile, err))
	}
	if document.Version != databaseMappingVersion {
		return nil, false, errors.Join(ErrCorrupt, fmt.Errorf("nestwal: %s version %d is not supported (want %d)", DatabaseMappingFile, document.Version, databaseMappingVersion))
	}
	if document.Databases == nil {
		document.Databases = map[string]string{}
	}
	return document.Databases, true, nil
}

// Dir 是 WAL 目录的绝对路径。
func (w *WAL) Dir() string { return w.opts.Dir }

// WriteDatabaseMapping 以“写临时文件 → fsync → 改名 → fsync 目录”记下本进程的映射。只能由持有
// writer.lock 的 WAL 调用（方法挂在打开的 WAL 上即为此），与要比较它的下一个进程不会并发。
// 内容与已有文件相同时不写。
func (w *WAL) WriteDatabaseMapping(databases map[string]string) error {
	if !w.operations.Begin() {
		return ErrClosed
	}
	defer w.operations.End()
	if databases == nil {
		databases = map[string]string{}
	}
	if previous, found, err := ReadDatabaseMapping(w.opts.Dir); err == nil && found && maps.Equal(previous, databases) {
		return nil
	}
	raw, err := json.MarshalIndent(databaseMappingDocument{Version: databaseMappingVersion, Databases: databases}, "", "  ")
	if err != nil {
		return err
	}
	target := filepath.Join(w.opts.Dir, DatabaseMappingFile)
	tmp := target + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, w.opts.FileMode)
	if err != nil {
		return err
	}
	if err = writeFull(file, append(raw, '\n')); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("nestwal: write %s: %w", DatabaseMappingFile, err)
	}
	if err := replaceCheckpointFile(tmp, target); err != nil {
		return fmt.Errorf("nestwal: replace %s: %w", DatabaseMappingFile, err)
	}
	return syncDirectory(w.opts.Dir)
}

var errPendingRecordFound = errors.New("nestwal: pending record found")

// HasUnacknowledgedRecords 报告 checkpoint 之后是否还有记录（尚未确认投影）。只读到第一条为止。
func (w *WAL) HasUnacknowledgedRecords(ctx context.Context) (bool, error) {
	err := w.Replay(ctx, func(corenest.CommitFence, corenest.CommitRecord) error { return errPendingRecordFound })
	if errors.Is(err, errPendingRecordFound) {
		return true, nil
	}
	return false, err
}
