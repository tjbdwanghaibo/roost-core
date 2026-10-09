//go:build windows

package syncstream

import (
	"syscall"
	"unsafe"
)

const (
	moveFileReplaceExisting = 0x1
	moveFileWriteThrough    = 0x8
)

var moveFileExW = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

// File.Sync persists newly created NTFS file metadata. Directory handles do
// not support FlushFileBuffers, so directory durability is supplied by the
// write-through rename used when publishing checkpoints.
func syncJournalDirectory(string) error { return nil }

func durableReplace(from, to, _ string) error {
	fromPath, err := syscall.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	toPath, err := syscall.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	result, _, callErr := moveFileExW.Call(
		uintptr(unsafe.Pointer(fromPath)),
		uintptr(unsafe.Pointer(toPath)),
		uintptr(moveFileReplaceExisting|moveFileWriteThrough),
	)
	if result != 0 {
		return nil
	}
	if callErr != syscall.Errno(0) {
		return callErr
	}
	return syscall.EINVAL
}
