package manifest

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sliverarmory/sliver-test-bof/internal/safepath"
	"gopkg.in/yaml.v3"
)

// Load decodes, validates, normalizes, and expands a manifest for direct action
// use. Manifest, object, and file-argument paths resolve from workspace.
func Load(path, workspace string) (*Manifest, error) {
	return load(path, workspace, workspace, false, false)
}

// LoadForPlanning rejects object symlinks so planner-staged artifacts have a
// stable workspace-relative destination on every supported filesystem.
func LoadForPlanning(path, workspace string) (*Manifest, error) {
	return load(path, workspace, workspace, true, false)
}

// LoadWithObjectRoot resolves BOF objects and collection globs from a private,
// symlink-free artifact tree while keeping the manifest and typed file
// arguments rooted in the caller workspace.
func LoadWithObjectRoot(path, workspace, objectRoot string) (*Manifest, error) {
	return load(path, workspace, objectRoot, true, true)
}

func load(path, workspace, objectRoot string, rejectObjectSymlinks, isolatedObjectRoot bool) (*Manifest, error) {
	paths, err := newPathResolver(workspace)
	if err != nil {
		return nil, err
	}
	objects, err := newObjectPathResolver(objectRoot, rejectObjectSymlinks)
	if err != nil {
		return nil, fmt.Errorf("object root: %w", err)
	}
	if isolatedObjectRoot && (pathWithin(paths.root, objects.root) || pathWithin(objects.root, paths.root)) {
		return nil, fmt.Errorf("isolated object root must not overlap the caller workspace")
	}

	manifestPath, err := paths.file(path, "manifest")
	if err != nil {
		return nil, err
	}
	f, err := os.Open(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("open manifest: %w", err)
	}
	defer f.Close()

	var raw rawConfig
	decoder := yaml.NewDecoder(f)
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("manifest is empty")
		}
		return nil, fmt.Errorf("decode manifest: %w", err)
	}

	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("manifest must contain exactly one YAML document")
		}
		return nil, fmt.Errorf("decode trailing YAML document: %w", err)
	}

	config, plans, err := normalize(raw, paths, objects)
	if err != nil {
		return nil, err
	}
	if isolatedObjectRoot {
		if err := validateIsolatedObjectRoot(objects.root, plans); err != nil {
			return nil, err
		}
	}
	return &Manifest{
		Workspace: paths.root,
		Path:      manifestPath,
		Config:    config,
		Plans:     plans,
	}, nil
}

func validateIsolatedObjectRoot(root string, plans []Plan) error {
	expected := make(map[string]string, len(plans))
	for _, plan := range plans {
		folded := strings.ToUpper(plan.ObjectPath)
		if previous, ok := expected[folded]; ok && previous != plan.ObjectPath {
			return fmt.Errorf("isolated object paths %q and %q collide on case-insensitive filesystems", previous, plan.ObjectPath)
		}
		expected[folded] = plan.ObjectPath
	}
	seen := make(map[string]struct{}, len(expected))
	err := filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == root {
			return nil
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("isolated object root contains symlink %q", relative)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || filepath.Ext(relative) != ".o" {
			return fmt.Errorf("isolated object root contains non-object file %q", relative)
		}
		if err := safepath.ValidatePortableRelative(relative); err != nil {
			return fmt.Errorf("isolated object path %q: %w", relative, err)
		}
		folded := strings.ToUpper(relative)
		planned, ok := expected[folded]
		if !ok || planned != relative {
			return fmt.Errorf("isolated object root contains unplanned object %q", relative)
		}
		seen[folded] = struct{}{}
		return nil
	})
	if err != nil {
		return fmt.Errorf("validate isolated object root: %w", err)
	}
	if len(seen) != len(expected) {
		return fmt.Errorf("isolated object root contains %d planned object(s); expected %d", len(seen), len(expected))
	}
	return nil
}
