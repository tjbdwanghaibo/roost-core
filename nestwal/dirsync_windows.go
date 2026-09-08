//go:build windows

package nestwal

import "golang.org/x/sys/windows"

// Windows does not expose POSIX directory fsync. File.Sync and the
// MOVEFILE_WRITE_THROUGH replacement below provide the corresponding durable
// file/rename boundary.
func syncDirectory(string) error { return nil }

func replaceCheckpointFile(source, target string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
