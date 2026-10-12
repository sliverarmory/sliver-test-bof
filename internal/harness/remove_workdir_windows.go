//go:build windows

package harness

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

const (
	workDirRemovalTimeout  = 10 * time.Second
	workDirRemovalInterval = 100 * time.Millisecond
)

// A terminated implant's executable can remain locked briefly while Windows
// releases its image section or another process finishes reading the file.
func removeWorkDir(path string) error {
	deadline := time.Now().Add(workDirRemovalTimeout)
	for {
		err := os.RemoveAll(path)
		if err == nil || !retryableWorkDirRemovalError(err) || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(workDirRemovalInterval)
	}
}

func retryableWorkDirRemovalError(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
