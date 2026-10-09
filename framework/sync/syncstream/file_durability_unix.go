//go:build !windows

package syncstream

import (
	"errors"
	"os"
)

func syncJournalDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	syncErr := handle.Sync()
	closeErr := handle.Close()
	return errors.Join(syncErr, closeErr)
}

func durableReplace(from, to, directory string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	return syncJournalDirectory(directory)
}
