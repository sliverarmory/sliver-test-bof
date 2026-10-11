//go:build windows

package harness

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestRemoveWorkDirWaitsForWindowsFileLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "generated")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "beacon.exe")
	if err := os.WriteFile(file, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(windows.StringToUTF16Ptr(file), windows.GENERIC_READ,
		windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); !retryableWorkDirRemovalError(err) {
		_ = windows.CloseHandle(handle)
		t.Fatalf("expected a transient Windows lock error, got %v", err)
	}
	released := make(chan error, 1)
	go func() {
		time.Sleep(250 * time.Millisecond)
		released <- windows.CloseHandle(handle)
	}()
	if err := removeWorkDir(dir); err != nil {
		t.Errorf("remove work directory after lock release: %v", err)
	}
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("work directory still exists: %v", err)
	}
}
