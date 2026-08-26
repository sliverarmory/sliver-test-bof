package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type pathResolver struct {
	root string
}

type globMatch struct {
	path     string
	basename string
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
		real, err := r.file(match, field+" match")
		if err != nil {
			return nil, err
		}
		if _, ok := seen[real]; ok {
			continue
		}
		seen[real] = struct{}{}
		resolved = append(resolved, globMatch{path: real, basename: filepath.Base(match)})
	}
	return resolved, nil
}

func pathWithin(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
