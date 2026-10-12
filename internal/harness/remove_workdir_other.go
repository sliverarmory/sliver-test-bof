//go:build !windows

package harness

import "os"

func removeWorkDir(path string) error {
	return os.RemoveAll(path)
}
