package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sliverarmory/sliver-test-bof/internal/manifest"
)

func TestStagePlanObjectsCopiesOnlyValidatedRelativeObjects(t *testing.T) {
	workspace := t.TempDir()
	runnerTemp := t.TempDir()
	t.Setenv("RUNNER_TEMP", runnerTemp)
	first := writeStageObject(t, workspace, "build/windows/amd64/one.o", []byte("one"))
	second := writeStageObject(t, workspace, ".objects/linux/386/two.o", []byte("two"))
	destination, err := os.MkdirTemp(runnerTemp, "objects-")
	if err != nil {
		t.Fatal(err)
	}
	plans := []manifest.Plan{
		{ID: "second", Object: second, ObjectPath: ".objects/linux/386/two.o"},
		{ID: "first", Object: first, ObjectPath: "build/windows/amd64/one.o"},
		{ID: "duplicate", Object: first, ObjectPath: "build/windows/amd64/one.o"},
	}
	inventory, err := stagePlanObjects(workspace, destination, plans)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory) != 2 || inventory[0].Path != ".objects/linux/386/two.o" || inventory[1].Path != "build/windows/amd64/one.o" {
		t.Fatalf("inventory = %#v", inventory)
	}
	for _, item := range inventory {
		data, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(item.Path)))
		if err != nil {
			t.Fatalf("read staged %s: %v", item.Path, err)
		}
		digest := sha256.Sum256(data)
		if got := hex.EncodeToString(digest[:]); got != item.SHA256 {
			t.Fatalf("staged %s digest = %s, want %s", item.Path, got, item.SHA256)
		}
		if runtime.GOOS != "windows" {
			info, err := os.Stat(filepath.Join(destination, filepath.FromSlash(item.Path)))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("staged %s mode = %o, want 600", item.Path, info.Mode().Perm())
			}
		}
	}
	entries, err := os.ReadDir(destination)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name() != ".objects" || entries[1].Name() != "build" {
		t.Fatalf("staging root entries = %#v", entries)
	}
}

func TestStagePlanObjectsCreatesMissingDestination(t *testing.T) {
	workspace := t.TempDir()
	runnerTemp := t.TempDir()
	t.Setenv("RUNNER_TEMP", runnerTemp)
	object := writeStageObject(t, workspace, "objects/test.o", []byte("object"))
	destination := filepath.Join(runnerTemp, "planned-objects")

	inventory, err := stagePlanObjects(workspace, destination, []manifest.Plan{{
		ID: "test", Object: object, ObjectPath: "objects/test.o",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory) != 1 || inventory[0].Path != "objects/test.o" {
		t.Fatalf("inventory = %#v", inventory)
	}
	if data, err := os.ReadFile(filepath.Join(destination, "objects", "test.o")); err != nil || string(data) != "object" {
		t.Fatalf("staged object = %q, %v", data, err)
	}
}

func TestStagePlanObjectsRejectsUnsafeSourcesAndDestinations(t *testing.T) {
	workspace := t.TempDir()
	runnerTemp := t.TempDir()
	t.Setenv("RUNNER_TEMP", runnerTemp)
	object := writeStageObject(t, workspace, "object.o", []byte("object"))

	tests := []struct {
		name        string
		plan        manifest.Plan
		prepareDest func(*testing.T) string
		want        string
	}{
		{
			name: "path escape", plan: manifest.Plan{ID: "escape", Object: object, ObjectPath: "../object.o"},
			prepareDest: emptyStageDestination(runnerTemp), want: "clean relative path",
		},
		{
			name: "Windows reserved path", plan: manifest.Plan{ID: "reserved", Object: object, ObjectPath: "build/CON.o"},
			prepareDest: emptyStageDestination(runnerTemp), want: "Windows-reserved",
		},
		{
			name: "destination in workspace", plan: manifest.Plan{ID: "object", Object: object, ObjectPath: "object.o"},
			prepareDest: func(t *testing.T) string {
				path := filepath.Join(workspace, "stage")
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				return path
			}, want: "outside the caller workspace",
		},
		{
			name: "nonempty destination", plan: manifest.Plan{ID: "object", Object: object, ObjectPath: "object.o"},
			prepareDest: func(t *testing.T) string {
				path := emptyStageDestination(runnerTemp)(t)
				if err := os.WriteFile(filepath.Join(path, "unexpected"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			}, want: "must be empty",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := stagePlanObjects(workspace, test.prepareDest(t), []manifest.Plan{test.plan})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("stagePlanObjects() error = %v, want %q", err, test.want)
			}
		})
	}

	if runtime.GOOS != "windows" {
		symlink := filepath.Join(workspace, "link.o")
		if err := os.Symlink(object, symlink); err != nil {
			t.Fatal(err)
		}
		_, err := stagePlanObjects(workspace, emptyStageDestination(runnerTemp)(t), []manifest.Plan{{ID: "link", Object: symlink, ObjectPath: "link.o"}})
		if err == nil || !strings.Contains(err.Error(), "non-symlink") {
			t.Fatalf("symlink error = %v", err)
		}

		destination := emptyStageDestination(runnerTemp)(t)
		symlinkDestination := destination + "-link"
		if err := os.Symlink(destination, symlinkDestination); err != nil {
			t.Fatal(err)
		}
		_, err = stagePlanObjects(workspace, symlinkDestination, []manifest.Plan{{ID: "object", Object: object, ObjectPath: "object.o"}})
		if err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
			t.Fatalf("staging destination symlink error = %v", err)
		}
	}
}

func emptyStageDestination(root string) func(*testing.T) string {
	return func(t *testing.T) string {
		t.Helper()
		path, err := os.MkdirTemp(root, "objects-")
		if err != nil {
			t.Fatal(err)
		}
		return path
	}
}

func writeStageObject(t *testing.T, workspace, relative string, data []byte) string {
	t.Helper()
	path := filepath.Join(workspace, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
