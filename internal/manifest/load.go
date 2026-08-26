package manifest

import (
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// Load decodes, validates, normalizes, and expands a manifest. Relative
// manifest and object paths are resolved from workspace, never from the
// process working directory.
func Load(path, workspace string) (*Manifest, error) {
	paths, err := newPathResolver(workspace)
	if err != nil {
		return nil, err
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

	config, plans, err := normalize(raw, paths)
	if err != nil {
		return nil, err
	}
	return &Manifest{
		Workspace: paths.root,
		Path:      manifestPath,
		Config:    config,
		Plans:     plans,
	}, nil
}
