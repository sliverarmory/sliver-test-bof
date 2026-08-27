package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/sliverarmory/sliver-test-bof/internal/safepath"
)

var runtimeFixturePlaceholder = regexp.MustCompile(`^\$\{([A-Z][A-Z0-9_]{0,63})\}$`)
var runtimeFixtureVariable = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

func prepareFixtures(root string, fixtures Fixtures) (variables map[string]string, err error) {
	variables = make(map[string]string, 2)
	if fixtures.File == nil && fixtures.Directory == nil {
		return variables, nil
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve fixture root: %w", err)
	}
	for where, variable := range map[string]string{
		"file": fixtureVariable(fixtures.File), "directory": directoryFixtureVariable(fixtures.Directory),
	} {
		if variable != "" && !runtimeFixtureVariable.MatchString(variable) {
			return nil, fmt.Errorf("%s fixture variable %q is invalid", where, variable)
		}
	}
	if fixtures.File != nil && fixtures.Directory != nil && fixtures.File.Variable == fixtures.Directory.Variable {
		return nil, fmt.Errorf("fixture variable %q is duplicated", fixtures.File.Variable)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create fixture root: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("set fixture root permissions: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			removeErr := os.RemoveAll(root)
			if removeErr != nil {
				err = joinFixtureError(err, fmt.Errorf("remove incomplete fixtures: %w", removeErr))
			}
		}
	}()

	if fixtures.File != nil {
		path := filepath.Join(root, "file", fixtures.File.Variable+".txt")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create file fixture parent: %w", err)
		}
		if err := os.WriteFile(path, []byte(fixtures.File.ContentsUTF8), 0o600); err != nil {
			return nil, fmt.Errorf("write file fixture: %w", err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("set file fixture permissions: %w", err)
		}
		absolute, err := filepath.Abs(path)
		if err != nil || !pathWithin(root, absolute) {
			return nil, fmt.Errorf("file fixture escaped fixture root")
		}
		variables[fixtures.File.Variable] = absolute
	}

	if fixtures.Directory != nil {
		directory := filepath.Join(root, "directory", fixtures.Directory.Variable)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create directory fixture: %w", err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return nil, fmt.Errorf("set directory fixture permissions: %w", err)
		}
		names := make([]string, 0, len(fixtures.Directory.Files))
		for name := range fixtures.Directory.Files {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if err := validateRuntimeFixturePath(name); err != nil {
				return nil, fmt.Errorf("directory fixture path %q: %w", name, err)
			}
			path := filepath.Join(directory, filepath.FromSlash(name))
			if !pathWithin(directory, path) {
				return nil, fmt.Errorf("directory fixture path %q escapes fixture root", name)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return nil, fmt.Errorf("create directory fixture parent %q: %w", name, err)
			}
			if err := os.WriteFile(path, []byte(fixtures.Directory.Files[name]), 0o600); err != nil {
				return nil, fmt.Errorf("write directory fixture %q: %w", name, err)
			}
			if err := os.Chmod(path, 0o600); err != nil {
				return nil, fmt.Errorf("set directory fixture permissions %q: %w", name, err)
			}
		}
		absolute, err := filepath.Abs(directory)
		if err != nil || !pathWithin(root, absolute) {
			return nil, fmt.Errorf("directory fixture escaped fixture root")
		}
		variables[fixtures.Directory.Variable] = absolute
	}
	ok = true
	return variables, nil
}

func fixtureVariable(fixture *FileFixture) string {
	if fixture == nil {
		return ""
	}
	return fixture.Variable
}

func directoryFixtureVariable(fixture *DirectoryFixture) string {
	if fixture == nil {
		return ""
	}
	return fixture.Variable
}

func substituteFixtureArguments(arguments []Argument, variables map[string]string) ([]Argument, error) {
	resolved := make([]Argument, len(arguments))
	for index, argument := range arguments {
		resolved[index] = argument
		resolved[index].Data = append([]byte(nil), argument.Data...)
		if argument.Type != "string" && argument.Type != "wstring" {
			continue
		}
		if !strings.Contains(argument.String, "${") {
			continue
		}
		match := runtimeFixturePlaceholder.FindStringSubmatch(argument.String)
		if match == nil {
			return nil, fmt.Errorf("argument %d fixture substitution must be the entire value in the form ${NAME}", index+1)
		}
		value, ok := variables[match[1]]
		if !ok {
			return nil, fmt.Errorf("argument %d references unknown fixture variable %q", index+1, match[1])
		}
		resolved[index].String = value
	}
	return resolved, nil
}

func validateRuntimeFixturePath(value string) error {
	return safepath.ValidatePortableRelative(value)
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))))
}

func joinFixtureError(primary, secondary error) error {
	if primary == nil {
		return secondary
	}
	if secondary == nil {
		return primary
	}
	return fmt.Errorf("%v; %w", primary, secondary)
}
