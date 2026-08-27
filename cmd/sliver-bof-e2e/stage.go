package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sliverarmory/sliver-test-bof/internal/manifest"
	"github.com/sliverarmory/sliver-test-bof/internal/safepath"
)

type stagedObject struct {
	plannedObject
	data []byte
}

// stagePlanObjects copies only manifest-expanded BOF objects into an empty,
// action-owned staging directory while preserving their workspace-relative
// paths. The resulting directory is safe to upload as the workflow's object
// artifact; no manifest, source, credential, or other workspace file enters it.
func stagePlanObjects(workspace, destination string, plans []manifest.Plan) ([]plannedObject, error) {
	workspace, err := canonicalDirectory(workspace)
	if err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	objects := make([]stagedObject, 0, len(plans))
	seenPaths := make(map[string]string, len(plans))
	for _, plan := range plans {
		relative := filepath.ToSlash(plan.ObjectPath)
		if err := validateStagedObjectPath(relative); err != nil {
			return nil, fmt.Errorf("%s object path %q: %w", plan.ID, relative, err)
		}
		folded := strings.ToUpper(relative)
		if previous, ok := seenPaths[folded]; ok {
			if previous != relative {
				return nil, fmt.Errorf("object paths %q and %q collide on case-insensitive filesystems", previous, relative)
			}
			continue
		}
		seenPaths[folded] = relative

		source, err := filepath.Abs(plan.Object)
		if err != nil {
			return nil, fmt.Errorf("resolve %s object source: %w", plan.ID, err)
		}
		info, err := os.Lstat(source)
		if err != nil {
			return nil, fmt.Errorf("inspect %s object: %w", plan.ID, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s object source must be a regular non-symlink file", plan.ID)
		}
		source, err = filepath.EvalSymlinks(source)
		if err != nil || !pathWithinDirectory(workspace, source) {
			return nil, fmt.Errorf("%s object source is outside the workspace", plan.ID)
		}
		data, err := os.ReadFile(source)
		if err != nil {
			return nil, fmt.Errorf("read %s object: %w", plan.ID, err)
		}
		digest := sha256.Sum256(data)
		objects = append(objects, stagedObject{
			plannedObject: plannedObject{Path: relative, SHA256: hex.EncodeToString(digest[:])},
			data:          data,
		})
	}
	if len(objects) == 0 {
		return nil, fmt.Errorf("manifest contains no BOF objects")
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Path < objects[j].Path })

	if strings.TrimSpace(destination) != "" {
		root, err := validateStagingDirectory(workspace, destination)
		if err != nil {
			return nil, err
		}
		for _, object := range objects {
			target := filepath.Join(root, filepath.FromSlash(object.Path))
			if !pathWithinDirectory(root, target) {
				return nil, fmt.Errorf("staged object path %q escapes destination", object.Path)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return nil, fmt.Errorf("create staged object parent: %w", err)
			}
			if err := os.WriteFile(target, object.data, 0o600); err != nil {
				return nil, fmt.Errorf("write staged object %q: %w", object.Path, err)
			}
			if err := os.Chmod(target, 0o600); err != nil {
				return nil, fmt.Errorf("set staged object permissions %q: %w", object.Path, err)
			}
		}
	}

	inventory := make([]plannedObject, len(objects))
	for index := range objects {
		inventory[index] = objects[index].plannedObject
	}
	return inventory, nil
}

func validateStagedObjectPath(value string) error {
	if err := safepath.ValidatePortableRelative(value); err != nil {
		return err
	}
	if path.Ext(value) != ".o" {
		return fmt.Errorf("must have a .o extension")
	}
	return nil
}

func validateStagingDirectory(workspace, destination string) (string, error) {
	absolute, err := filepath.Abs(destination)
	if err != nil {
		return "", fmt.Errorf("resolve staging directory: %w", err)
	}
	info, err := os.Lstat(absolute)
	if errors.Is(err, os.ErrNotExist) {
		parent, parentErr := canonicalDirectory(filepath.Dir(absolute))
		if parentErr != nil {
			return "", fmt.Errorf("staging directory parent: %w", parentErr)
		}
		root := filepath.Join(parent, filepath.Base(absolute))
		if err := validateStagingLocation(workspace, root); err != nil {
			return "", err
		}
		if err := os.Mkdir(root, 0o700); err != nil {
			return "", fmt.Errorf("create staging directory: %w", err)
		}
		absolute = root
		info, err = os.Lstat(absolute)
	}
	if err != nil {
		return "", fmt.Errorf("inspect staging directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("staging directory must not be a symlink")
	}
	root, err := canonicalDirectory(destination)
	if err != nil {
		return "", fmt.Errorf("staging directory: %w", err)
	}
	if err := validateStagingLocation(workspace, root); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("read staging directory: %w", err)
	}
	if len(entries) != 0 {
		return "", fmt.Errorf("staging directory must be empty")
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return "", fmt.Errorf("set staging directory permissions: %w", err)
	}
	return root, nil
}

func validateStagingLocation(workspace, root string) error {
	if pathWithinDirectory(workspace, root) {
		return fmt.Errorf("staging directory must be outside the caller workspace")
	}
	if runnerTemp := strings.TrimSpace(os.Getenv("RUNNER_TEMP")); runnerTemp != "" {
		tempRoot, err := canonicalDirectory(runnerTemp)
		if err != nil {
			return fmt.Errorf("runner temp: %w", err)
		}
		if !pathWithinDirectory(tempRoot, root) {
			return fmt.Errorf("staging directory must be inside RUNNER_TEMP")
		}
	}
	return nil
}

func canonicalDirectory(value string) (string, error) {
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", value)
	}
	return filepath.Clean(real), nil
}

func pathWithinDirectory(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))))
}
