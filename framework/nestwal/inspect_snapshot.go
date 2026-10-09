package nestwal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type snapshotFile struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

func inspectRegularFiles(dir string) error {
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return fmt.Errorf("nestwal: refusing non-regular inspection file %s", path)
		}
		return nil
	})
}

// snapshotWAL 在同一writer锁下复制整个目录，包括坏帧后的后缀和辅助文件。
// INCOMPLETE仅在副本、哈希清单及诊断报告均落盘后移除；失败副本留给操作者检查。
func snapshotWAL(ctx context.Context, source, requested string) (string, error) {
	parent, err := filepath.EvalSymlinks(filepath.Dir(requested))
	if err != nil {
		return "", err
	}
	destination, err := filepath.Abs(filepath.Join(parent, filepath.Base(requested)))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(source, destination)
	if err != nil || rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("nestwal: snapshot must be outside the source directory")
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		return "", err
	}
	if err := writeInspectionFile(filepath.Join(destination, "INCOMPLETE"), []byte("Do not restore: snapshot or inspection did not finish.\n")); err != nil {
		return "", err
	}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		return "", err
	}
	var files []snapshotFile
	var directories []string
	buffer := make([]byte, 32<<10)
	err = filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, "wal", rel)
		if entry.IsDir() {
			if err := os.Mkdir(target, 0o700); err != nil {
				return err
			}
			directories = append(directories, target)
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("nestwal: refusing snapshot link or special file %s", path)
		}
		file, err := copyInspectionFile(ctx, path, target, buffer)
		if err != nil {
			return err
		}
		file.Path = filepath.ToSlash(rel)
		files = append(files, file)
		return nil
	})
	if err != nil {
		return "", err
	}
	for i := len(directories) - 1; i >= 0; i-- {
		if err := syncDirectory(directories[i]); err != nil {
			return "", err
		}
	}
	raw, err := json.MarshalIndent(files, "", "  ")
	if err != nil {
		return "", err
	}
	if err := writeInspectionFile(filepath.Join(destination, "manifest.json"), raw); err != nil {
		return "", err
	}
	return destination, nil
}

type inspectionReader struct {
	ctx context.Context
	io.Reader
}

func (r inspectionReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

func copyInspectionFile(ctx context.Context, source, target string, buffer []byte) (entry snapshotFile, err error) {
	in, err := os.Open(source)
	if err != nil {
		return entry, err
	}
	defer func() { err = errors.Join(err, in.Close()) }()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return entry, err
	}
	defer func() { err = errors.Join(err, out.Close()) }()
	hash := sha256.New()
	entry.Bytes, err = io.CopyBuffer(io.MultiWriter(out, hash), inspectionReader{ctx: ctx, Reader: in}, buffer)
	if err == nil {
		err = out.Sync()
	}
	entry.SHA256 = hex.EncodeToString(hash.Sum(nil))
	return entry, err
}

func writeInspectionFile(path string, raw []byte) (err error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	if err := writeFull(file, raw); err != nil {
		return err
	}
	return file.Sync()
}

func finishWALSnapshot(dir string, report Inspection) error {
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := writeInspectionFile(filepath.Join(dir, "inspection.json"), raw); err != nil {
		return err
	}
	if err := syncDirectory(dir); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, "INCOMPLETE")); err != nil {
		return err
	}
	return syncDirectory(dir)
}
