package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestStateTrackerPersistsAndRemovesEntries(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "cleanup-state.json")
	tracker := newStateTracker(statePath)

	if err := tracker.addTempDir("/tmp/sliver-bof-e2e-test"); err != nil {
		t.Fatal(err)
	}
	if err := tracker.addPID(4242); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state cleanupState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.TempDirs) != 1 || state.TempDirs[0] != "/tmp/sliver-bof-e2e-test" || len(state.PIDs) != 1 || state.PIDs[0] != 4242 {
		t.Fatalf("persisted cleanup state = %#v", state)
	}

	if err := tracker.removePID(4242); err != nil {
		t.Fatal(err)
	}
	if err := tracker.removeTempDir("/tmp/sliver-bof-e2e-test"); err != nil {
		t.Fatal(err)
	}
	if !tracker.empty() {
		t.Fatal("cleanup tracker retained removed entries")
	}
}

func TestStateTrackerRollsBackFailedPersistence(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	tracker := newStateTracker(filepath.Join(blocker, "cleanup-state.json"))

	if err := tracker.addPID(4242); err == nil {
		t.Fatal("addPID unexpectedly ignored persistence failure")
	}
	if err := tracker.addTempDir("/tmp/sliver-bof-e2e-test"); err == nil {
		t.Fatal("addTempDir unexpectedly ignored persistence failure")
	}
	if !tracker.empty() {
		t.Fatalf("failed updates were retained in memory: %#v", tracker.data)
	}
}
