//go:build !windows

package nestwal

import "os"

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func replaceCheckpointFile(source, target string) error { return os.Rename(source, target) }
