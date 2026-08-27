package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sliverarmory/sliver-test-bof/internal/safepath"
)

type pathResolver struct {
	root                 string
	rejectObjectSymlinks bool
}

type globMatch struct {
	path          string
	workspacePath string
	basename      string
}

func newPathResolver(workspace string) (pathResolver, error) {
	if strings.TrimSpace(workspace) == "" {
		return pathResolver{}, fmt.Errorf("workspace is required")
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return pathResolver{}, fmt.Errorf("resolve workspace: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return pathResolver{}, fmt.Errorf("resolve workspace symlinks: %w", err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return pathResolver{}, fmt.Errorf("stat workspace: %w", err)
	}
	if !info.IsDir() {
		return pathResolver{}, fmt.Errorf("workspace %q is not a directory", workspace)
	}
	return pathResolver{root: filepath.Clean(real)}, nil
}

func newObjectPathResolver(root string, rejectSymlinks bool) (pathResolver, error) {
	resolver, err := newPathResolver(root)
	if err != nil {
		return pathResolver{}, err
	}
	resolver.rejectObjectSymlinks = rejectSymlinks
	return resolver, nil
}

func (r pathResolver) file(value, field string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s path is required", field)
	}
	value = filepath.FromSlash(value)
	candidate := value
	relative := !filepath.IsAbs(candidate)
	if relative {
		candidate = filepath.Join(r.root, candidate)
	}
	candidate, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve %s path: %w", field, err)
	}
	if relative && !pathWithin(r.root, candidate) {
		return "", fmt.Errorf("%s path %q escapes workspace", field, value)
	}
	real, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve %s path %q: %w", field, value, err)
	}
	real, err = filepath.Abs(real)
	if err != nil {
		return "", fmt.Errorf("resolve canonical %s path %q: %w", field, value, err)
	}
	if !pathWithin(r.root, real) {
		return "", fmt.Errorf("%s path %q resolves outside workspace", field, value)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("stat %s path %q: %w", field, value, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s path %q is not a regular file", field, value)
	}
	return filepath.Clean(real), nil
}

// object resolves a BOF object and also returns its stable, slash-separated
// path relative to the selected object root. Planner and artifact roots reject
// symlinks; direct action loads preserve confined in-workspace symlink support.
func (r pathResolver) object(value, field string) (string, string, error) {
	if strings.TrimSpace(value) == "" {
		return "", "", fmt.Errorf("%s path is required", field)
	}
	candidate := filepath.FromSlash(value)
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(r.root, candidate)
	}
	candidate, err := filepath.Abs(candidate)
	if err != nil {
		return "", "", fmt.Errorf("resolve %s path: %w", field, err)
	}
	candidate = filepath.Clean(candidate)
	if !pathWithin(r.root, candidate) {
		return "", "", fmt.Errorf("%s path %q escapes workspace", field, value)
	}
	if r.rejectObjectSymlinks {
		if err := rejectSymlinkPath(r.root, candidate); err != nil {
			return "", "", fmt.Errorf("%s path %q: %w", field, value, err)
		}
	}
	real, err := r.file(candidate, field)
	if err != nil {
		return "", "", err
	}
	relative, err := filepath.Rel(r.root, candidate)
	if err != nil || relative == "." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("resolve workspace-relative %s path %q", field, value)
	}
	return real, filepath.ToSlash(relative), nil
}

func (r pathResolver) declaredObject(value, field string) (string, string, error) {
	if r.rejectObjectSymlinks {
		if err := safepath.ValidatePortableRelative(value); err != nil {
			return "", "", fmt.Errorf("%s path %q must be relative to the object root: %w", field, value, err)
		}
	}
	return r.object(value, field)
}

func (r pathResolver) glob(pattern, field string) ([]globMatch, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, fmt.Errorf("%s glob is required", field)
	}
	pattern = filepath.FromSlash(pattern)
	if filepath.IsAbs(pattern) {
		return nil, fmt.Errorf("%s glob %q must be relative to the workspace", field, pattern)
	}
	for _, part := range strings.Split(filepath.Clean(pattern), string(filepath.Separator)) {
		if part == ".." {
			return nil, fmt.Errorf("%s glob %q escapes workspace", field, pattern)
		}
	}
	fullPattern := filepath.Join(r.root, pattern)
	if !pathWithin(r.root, filepath.Clean(fullPattern)) {
		return nil, fmt.Errorf("%s glob %q escapes workspace", field, pattern)
	}
	matches, err := filepath.Glob(fullPattern)
	if err != nil {
		return nil, fmt.Errorf("invalid %s glob %q: %w", field, pattern, err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("%s glob %q matched no files", field, pattern)
	}

	resolved := make([]globMatch, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		real, workspacePath, err := r.object(match, field+" match")
		if err != nil {
			return nil, err
		}
		if _, ok := seen[real]; ok {
			continue
		}
		seen[real] = struct{}{}
		resolved = append(resolved, globMatch{path: real, workspacePath: workspacePath, basename: filepath.Base(match)})
	}
	return resolved, nil
}

func rejectSymlinkPath(root, candidate string) error {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || relative == "." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("cannot derive a confined workspace path")
	}
	current := root
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect path component %q: %w", component, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink component %q is not allowed for BOF objects", component)
		}
	}
	return nil
}

func pathWithin(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
