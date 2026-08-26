package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type cleanupState struct {
	TempDirs []string `json:"temp_dirs,omitempty"`
	PIDs     []int    `json:"pids,omitempty"`
}

type stateTracker struct {
	mu   sync.Mutex
	path string
	data cleanupState
}

func newStateTracker(path string) *stateTracker { return &stateTracker{path: path} }

func (tracker *stateTracker) addTempDir(path string) error {
	if tracker.path == "" {
		return nil
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	previous := cloneCleanupState(tracker.data)
	tracker.data.TempDirs = appendUniqueString(tracker.data.TempDirs, path)
	if err := tracker.writeLocked(); err != nil {
		tracker.data = previous
		return err
	}
	return nil
}

func (tracker *stateTracker) removeTempDir(path string) error {
	if tracker.path == "" {
		return nil
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	previous := cloneCleanupState(tracker.data)
	updated := make([]string, 0, len(tracker.data.TempDirs))
	for _, current := range tracker.data.TempDirs {
		if current != path {
			updated = append(updated, current)
		}
	}
	tracker.data.TempDirs = updated
	if err := tracker.writeLocked(); err != nil {
		tracker.data = previous
		return err
	}
	return nil
}

func (tracker *stateTracker) addPID(pid int) error {
	if tracker.path == "" || pid <= 0 {
		return nil
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	for _, current := range tracker.data.PIDs {
		if current == pid {
			return nil
		}
	}
	previous := cloneCleanupState(tracker.data)
	tracker.data.PIDs = append(tracker.data.PIDs, pid)
	if err := tracker.writeLocked(); err != nil {
		tracker.data = previous
		return err
	}
	return nil
}

func (tracker *stateTracker) removePID(pid int) error {
	if tracker.path == "" {
		return nil
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	previous := cloneCleanupState(tracker.data)
	updated := make([]int, 0, len(tracker.data.PIDs))
	for _, current := range tracker.data.PIDs {
		if current != pid {
			updated = append(updated, current)
		}
	}
	tracker.data.PIDs = updated
	if err := tracker.writeLocked(); err != nil {
		tracker.data = previous
		return err
	}
	return nil
}

func (tracker *stateTracker) clear() error {
	if tracker.path == "" {
		return nil
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	previous := cloneCleanupState(tracker.data)
	tracker.data = cleanupState{}
	if err := tracker.writeLocked(); err != nil {
		tracker.data = previous
		return err
	}
	return nil
}

func (tracker *stateTracker) empty() bool {
	if tracker.path == "" {
		return true
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return len(tracker.data.PIDs) == 0 && len(tracker.data.TempDirs) == 0
}

func (tracker *stateTracker) writeLocked() error {
	if err := os.MkdirAll(filepath.Dir(tracker.path), 0o700); err != nil {
		return fmt.Errorf("create cleanup state directory: %w", err)
	}
	data, err := json.Marshal(tracker.data)
	if err != nil {
		return fmt.Errorf("encode cleanup state: %w", err)
	}
	temporary := tracker.path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write cleanup state: %w", err)
	}
	if err := replaceFile(temporary, tracker.path); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("replace cleanup state: %w", err)
	}
	return nil
}

func cloneCleanupState(state cleanupState) cleanupState {
	return cleanupState{
		TempDirs: append([]string(nil), state.TempDirs...),
		PIDs:     append([]int(nil), state.PIDs...),
	}
}

func appendUniqueString(values []string, value string) []string {
	for _, current := range values {
		if current == value {
			return values
		}
	}
	return append(values, value)
}
