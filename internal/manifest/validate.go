package manifest

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sliverarmory/sliver-test-bof/internal/safepath"
	"gopkg.in/yaml.v3"
)

const (
	maxRepeat            = 1000
	maxTimeout           = 24 * time.Hour
	maxArguments         = 1024
	maxInlineBytes       = 16 << 20
	maxFixtureFiles      = 256
	maxFixtureFileBytes  = 1 << 20
	maxFixtureTotalBytes = 16 << 20
)

var versionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)
var commitPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
var fixtureVariablePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
var fixturePlaceholderPattern = regexp.MustCompile(`^\$\{([A-Z][A-Z0-9_]{0,63})\}$`)

func normalize(raw rawConfig, paths, objects pathResolver) (Config, []Plan, error) {
	if raw.Schema != SchemaV1 {
		return Config{}, nil, fmt.Errorf("schema must be %q", SchemaV1)
	}
	if err := validateVersion(raw.Sliver.Version); err != nil {
		return Config{}, nil, fmt.Errorf("sliver.version: %w", err)
	}
	if len(raw.Suites) == 0 {
		return Config{}, nil, fmt.Errorf("suites must contain at least one suite")
	}

	fixtures, fixtureVariables, err := normalizeFixtures(raw.Fixtures)
	if err != nil {
		return Config{}, nil, err
	}
	defaults, err := normalizeDefaults(raw.Defaults)
	if err != nil {
		return Config{}, nil, err
	}
	version := raw.Sliver.Version
	if commitPattern.MatchString(version) {
		version = strings.ToLower(version)
	}
	config := Config{
		Schema:   raw.Schema,
		Sliver:   SliverConfig{Version: version},
		Fixtures: fixtures,
		Defaults: defaults,
		Suites:   make([]Suite, 0, len(raw.Suites)),
	}
	plans := make([]Plan, 0)
	suiteNames := make(map[string]struct{}, len(raw.Suites))
	planIDs := make(map[string]struct{})

	for suiteIndex, input := range raw.Suites {
		where := fmt.Sprintf("suites[%d]", suiteIndex)
		if err := validateName(input.Name); err != nil {
			return Config{}, nil, fmt.Errorf("%s.name: %w", where, err)
		}
		if _, exists := suiteNames[input.Name]; exists {
			return Config{}, nil, fmt.Errorf("%s.name %q is duplicated", where, input.Name)
		}
		suiteNames[input.Name] = struct{}{}

		target := Target{OS: input.Target.OS, Arch: input.Target.Arch}
		if _, err := RunnerForTarget(target); err != nil {
			return Config{}, nil, fmt.Errorf("%s.target: %w", where, err)
		}
		if len(input.Tests) == 0 && len(input.Collections) == 0 {
			return Config{}, nil, fmt.Errorf("%s must contain at least one test or collection", where)
		}

		suite := Suite{
			Name:        input.Name,
			Target:      target,
			Tests:       make([]Test, 0, len(input.Tests)),
			Collections: make([]Collection, 0, len(input.Collections)),
		}
		itemNames := make(map[string]struct{}, len(input.Tests)+len(input.Collections))

		for testIndex, inputTest := range input.Tests {
			testWhere := fmt.Sprintf("%s.tests[%d]", where, testIndex)
			if err := validateName(inputTest.Name); err != nil {
				return Config{}, nil, fmt.Errorf("%s.name: %w", testWhere, err)
			}
			if _, exists := itemNames[inputTest.Name]; exists {
				return Config{}, nil, fmt.Errorf("%s.name %q is duplicated in suite %q", testWhere, inputTest.Name, input.Name)
			}
			itemNames[inputTest.Name] = struct{}{}
			if filepath.Ext(inputTest.Object) != ".o" {
				return Config{}, nil, fmt.Errorf("%s.object %q must have a .o extension", testWhere, inputTest.Object)
			}
			object, objectPath, err := objects.declaredObject(inputTest.Object, testWhere+".object")
			if err != nil {
				return Config{}, nil, err
			}
			if err := validateObject(object, target); err != nil {
				return Config{}, nil, fmt.Errorf("%s.object: %w", testWhere, err)
			}
			settings, err := normalizeSettings(defaults, inputTest.rawSettings, paths, fixtureVariables, testWhere)
			if err != nil {
				return Config{}, nil, err
			}
			test := Test{Name: inputTest.Name, Object: object, Settings: settings}
			suite.Tests = append(suite.Tests, test)
			plan := Plan{
				ID:         input.Name + "/" + inputTest.Name,
				Suite:      input.Name,
				Name:       inputTest.Name,
				Object:     object,
				ObjectPath: objectPath,
				Target:     target,
				Settings:   cloneSettings(settings),
			}
			if err := appendUniquePlan(&plans, planIDs, plan); err != nil {
				return Config{}, nil, err
			}
		}

		for collectionIndex, inputCollection := range input.Collections {
			collectionWhere := fmt.Sprintf("%s.collections[%d]", where, collectionIndex)
			if err := validateName(inputCollection.Name); err != nil {
				return Config{}, nil, fmt.Errorf("%s.name: %w", collectionWhere, err)
			}
			if _, exists := itemNames[inputCollection.Name]; exists {
				return Config{}, nil, fmt.Errorf("%s.name %q is duplicated in suite %q", collectionWhere, inputCollection.Name, input.Name)
			}
			itemNames[inputCollection.Name] = struct{}{}
			collectionSettings, err := normalizeSettings(defaults, inputCollection.rawSettings, paths, fixtureVariables, collectionWhere)
			if err != nil {
				return Config{}, nil, err
			}
			matches, err := objects.glob(inputCollection.Glob, collectionWhere+".glob")
			if err != nil {
				return Config{}, nil, err
			}
			sort.Slice(matches, func(i, j int) bool { return matches[i].basename < matches[j].basename })
			objects := make([]string, 0, len(matches))
			basenames := make(map[string]struct{}, len(matches))
			for _, match := range matches {
				if filepath.Ext(match.basename) != ".o" {
					return Config{}, nil, fmt.Errorf("%s glob matched %q, which does not have a .o extension", collectionWhere, match.basename)
				}
				if _, exists := basenames[match.basename]; exists {
					return Config{}, nil, fmt.Errorf("%s glob has duplicate basename %q; overrides would be ambiguous", collectionWhere, match.basename)
				}
				basenames[match.basename] = struct{}{}
				if err := validateObject(match.path, target); err != nil {
					return Config{}, nil, fmt.Errorf("%s object %q: %w", collectionWhere, match.basename, err)
				}
				objects = append(objects, match.path)

				settings := cloneSettings(collectionSettings)
				if override, ok := inputCollection.Overrides[match.basename]; ok {
					settings, err = normalizeSettings(settings, override, paths, fixtureVariables, collectionWhere+".overrides["+match.basename+"]")
					if err != nil {
						return Config{}, nil, err
					}
				}
				plan := Plan{
					ID:         input.Name + "/" + inputCollection.Name + "/" + match.basename,
					Suite:      input.Name,
					Name:       match.basename,
					Collection: inputCollection.Name,
					Object:     match.path,
					ObjectPath: match.workspacePath,
					Target:     target,
					Settings:   settings,
				}
				if err := appendUniquePlan(&plans, planIDs, plan); err != nil {
					return Config{}, nil, err
				}
			}
			for basename := range inputCollection.Overrides {
				if filepath.Base(basename) != basename || strings.ContainsAny(basename, "/\\") || basename == "." || basename == ".." {
					return Config{}, nil, fmt.Errorf("%s.overrides key %q must be a basename", collectionWhere, basename)
				}
				if _, exists := basenames[basename]; !exists {
					return Config{}, nil, fmt.Errorf("%s.overrides key %q does not match a collection object", collectionWhere, basename)
				}
			}
			suite.Collections = append(suite.Collections, Collection{
				Name:     inputCollection.Name,
				Glob:     filepath.ToSlash(inputCollection.Glob),
				Settings: cloneSettings(collectionSettings),
				Objects:  objects,
			})
		}
		config.Suites = append(config.Suites, suite)
	}
	return config, plans, nil
}

func normalizeFixtures(raw *rawFixtures) (Fixtures, map[string]struct{}, error) {
	variables := make(map[string]struct{}, 2)
	if raw == nil {
		return Fixtures{}, variables, nil
	}
	if raw.File == nil && raw.Directory == nil {
		return Fixtures{}, nil, fmt.Errorf("fixtures must define file or directory")
	}
	fixtures := Fixtures{}
	totalBytes := 0
	addVariable := func(value, where string) error {
		if !fixtureVariablePattern.MatchString(value) {
			return fmt.Errorf("%s.variable %q must match %s", where, value, fixtureVariablePattern)
		}
		folded := strings.ToUpper(value)
		if _, exists := variables[folded]; exists {
			return fmt.Errorf("%s.variable %q is duplicated", where, value)
		}
		variables[folded] = struct{}{}
		return nil
	}
	if raw.File != nil {
		if err := addVariable(raw.File.Variable, "fixtures.file"); err != nil {
			return Fixtures{}, nil, err
		}
		if raw.File.ContentsUTF8 == nil {
			return Fixtures{}, nil, fmt.Errorf("fixtures.file.contents_utf8 is required")
		}
		contents := *raw.File.ContentsUTF8
		if !utf8.ValidString(contents) {
			return Fixtures{}, nil, fmt.Errorf("fixtures.file.contents_utf8 must contain valid UTF-8")
		}
		if len(contents) > maxFixtureFileBytes {
			return Fixtures{}, nil, fmt.Errorf("fixtures.file.contents_utf8 exceeds %d bytes", maxFixtureFileBytes)
		}
		totalBytes += len(contents)
		fixtures.File = &FileFixture{Variable: raw.File.Variable, ContentsUTF8: contents}
	}
	if raw.Directory != nil {
		if err := addVariable(raw.Directory.Variable, "fixtures.directory"); err != nil {
			return Fixtures{}, nil, err
		}
		if len(raw.Directory.Files) > maxFixtureFiles {
			return Fixtures{}, nil, fmt.Errorf("fixtures.directory.files contains %d entries; maximum is %d", len(raw.Directory.Files), maxFixtureFiles)
		}
		files := make(map[string]string, len(raw.Directory.Files))
		seen := make(map[string]string, len(raw.Directory.Files))
		for relative, contents := range raw.Directory.Files {
			if err := validateFixtureRelativePath(relative); err != nil {
				return Fixtures{}, nil, fmt.Errorf("fixtures.directory.files[%q]: %w", relative, err)
			}
			folded := strings.ToUpper(relative)
			if previous, duplicate := seen[folded]; duplicate {
				return Fixtures{}, nil, fmt.Errorf("fixtures.directory.files paths %q and %q collide on case-insensitive filesystems", previous, relative)
			}
			seen[folded] = relative
			if !utf8.ValidString(contents) {
				return Fixtures{}, nil, fmt.Errorf("fixtures.directory.files[%q] must contain valid UTF-8", relative)
			}
			if len(contents) > maxFixtureFileBytes {
				return Fixtures{}, nil, fmt.Errorf("fixtures.directory.files[%q] exceeds %d bytes", relative, maxFixtureFileBytes)
			}
			totalBytes += len(contents)
			if totalBytes > maxFixtureTotalBytes {
				return Fixtures{}, nil, fmt.Errorf("fixture contents exceed %d bytes", maxFixtureTotalBytes)
			}
			files[relative] = contents
		}
		for folded, original := range seen {
			components := strings.Split(folded, "/")
			for index := 1; index < len(components); index++ {
				ancestor := strings.Join(components[:index], "/")
				if conflicting, exists := seen[ancestor]; exists {
					return Fixtures{}, nil, fmt.Errorf("fixtures.directory.files paths %q and %q have a file/directory prefix collision", conflicting, original)
				}
			}
		}
		fixtures.Directory = &DirectoryFixture{Variable: raw.Directory.Variable, Files: files}
	}
	if totalBytes > maxFixtureTotalBytes {
		return Fixtures{}, nil, fmt.Errorf("fixture contents exceed %d bytes", maxFixtureTotalBytes)
	}
	return fixtures, variables, nil
}

func validateFixtureRelativePath(value string) error {
	return safepath.ValidatePortableRelative(value)
}

func normalizeDefaults(raw rawDefaults) (Settings, error) {
	settings := Settings{
		BOFExecutor:    ExecutorReflektor,
		Entrypoint:     "go",
		Modes:          []string{ModeSession, ModeBeacon},
		Repeat:         1,
		CommandTimeout: 2 * time.Minute,
	}
	if raw.BOFExecutor != nil {
		settings.BOFExecutor = *raw.BOFExecutor
	}
	if raw.Entrypoint != nil {
		settings.Entrypoint = *raw.Entrypoint
	}
	if raw.Modes != nil {
		settings.Modes = append([]string(nil), (*raw.Modes)...)
	}
	if raw.Repeat != nil {
		settings.Repeat = *raw.Repeat
	}
	if raw.CommandTimeout != nil {
		duration, err := time.ParseDuration(*raw.CommandTimeout)
		if err != nil {
			return Settings{}, fmt.Errorf("defaults.command_timeout: %w", err)
		}
		settings.CommandTimeout = duration
	}
	if err := validateCoreSettings(settings, "defaults"); err != nil {
		return Settings{}, err
	}
	return settings, nil
}

func normalizeSettings(base Settings, raw rawSettings, paths pathResolver, fixtureVariables map[string]struct{}, where string) (Settings, error) {
	settings := cloneSettings(base)
	if raw.BOFExecutor != nil {
		settings.BOFExecutor = *raw.BOFExecutor
	}
	if raw.Entrypoint != nil {
		settings.Entrypoint = *raw.Entrypoint
	}
	if raw.Modes != nil {
		settings.Modes = append([]string(nil), (*raw.Modes)...)
	}
	if raw.Repeat != nil {
		settings.Repeat = *raw.Repeat
	}
	if raw.CommandTimeout != nil {
		duration, err := time.ParseDuration(*raw.CommandTimeout)
		if err != nil {
			return Settings{}, fmt.Errorf("%s.command_timeout: %w", where, err)
		}
		settings.CommandTimeout = duration
	}
	if raw.Args != nil && raw.ArgsRawBase64 != nil {
		return Settings{}, fmt.Errorf("%s cannot set both args and args_raw_base64", where)
	}
	if raw.Args != nil {
		args, err := normalizeArguments(*raw.Args, paths, fixtureVariables, where+".args")
		if err != nil {
			return Settings{}, err
		}
		settings.Args = args
		settings.RawArgs = nil
		settings.HasRawArgs = false
	}
	if raw.ArgsRawBase64 != nil {
		decoded, err := decodeBase64(*raw.ArgsRawBase64, where+".args_raw_base64")
		if err != nil {
			return Settings{}, err
		}
		settings.RawArgs = decoded
		settings.HasRawArgs = true
		settings.Args = nil
	}
	if raw.TCPListener != nil {
		if raw.TCPListener.PortArgument == nil {
			return Settings{}, fmt.Errorf("%s.tcp_listener.port_argument is required", where)
		}
		listener := &TCPListener{PortArgument: *raw.TCPListener.PortArgument}
		settings.TCPListener = listener
	}
	if raw.Expect != nil {
		expect, err := normalizeExpectation(*raw.Expect, where+".expect")
		if err != nil {
			return Settings{}, err
		}
		settings.Expect = expect
	}
	if err := validateCoreSettings(settings, where); err != nil {
		return Settings{}, err
	}
	if !validExpectation(settings.Expect) {
		return Settings{}, fmt.Errorf("%s.expect must explicitly set success_only: true or at least one output assertion", where)
	}
	if err := validateTCPListener(settings, where); err != nil {
		return Settings{}, err
	}
	return settings, nil
}

func validateCoreSettings(settings Settings, where string) error {
	switch settings.BOFExecutor {
	case ExecutorReflektor, ExecutorCOFFLoader:
	default:
		return fmt.Errorf("%s.bof_executor must be %q or %q", where, ExecutorReflektor, ExecutorCOFFLoader)
	}
	if strings.TrimSpace(settings.Entrypoint) == "" || strings.ContainsRune(settings.Entrypoint, '\x00') {
		return fmt.Errorf("%s.entrypoint must be a non-empty string without NUL bytes", where)
	}
	if len(settings.Modes) == 0 {
		return fmt.Errorf("%s.modes must contain at least one mode", where)
	}
	seen := make(map[string]struct{}, len(settings.Modes))
	for _, mode := range settings.Modes {
		if mode != ModeSession && mode != ModeBeacon {
			return fmt.Errorf("%s.modes contains %q; only %q and %q are supported", where, mode, ModeSession, ModeBeacon)
		}
		if _, exists := seen[mode]; exists {
			return fmt.Errorf("%s.modes contains duplicate mode %q", where, mode)
		}
		seen[mode] = struct{}{}
	}
	if settings.Repeat < 1 || settings.Repeat > maxRepeat {
		return fmt.Errorf("%s.repeat must be between 1 and %d", where, maxRepeat)
	}
	if settings.CommandTimeout <= 0 || settings.CommandTimeout > maxTimeout {
		return fmt.Errorf("%s.command_timeout must be greater than zero and no more than %s", where, maxTimeout)
	}
	return nil
}

func normalizeArguments(raw []rawArgument, paths pathResolver, fixtureVariables map[string]struct{}, where string) ([]Argument, error) {
	if len(raw) > maxArguments {
		return nil, fmt.Errorf("%s contains %d arguments; maximum is %d", where, len(raw), maxArguments)
	}
	args := make([]Argument, 0, len(raw))
	inlineBytes := 0
	for index, input := range raw {
		argWhere := fmt.Sprintf("%s[%d]", where, index)
		arg := Argument{Type: input.Type}
		switch input.Type {
		case "int", "integer":
			var value uint64
			if input.Value.Kind != yaml.ScalarNode || input.Value.Tag != "!!int" {
				return nil, fmt.Errorf("%s.value must be an unsigned 32-bit integer", argWhere)
			}
			if err := input.Value.Decode(&value); err != nil || value > uint64(^uint32(0)) {
				return nil, fmt.Errorf("%s.value must be an unsigned 32-bit integer", argWhere)
			}
			arg.Type = "int"
			arg.Integer = uint32(value)
		case "short":
			var value uint64
			if input.Value.Kind != yaml.ScalarNode || input.Value.Tag != "!!int" {
				return nil, fmt.Errorf("%s.value must be an unsigned 16-bit integer", argWhere)
			}
			if err := input.Value.Decode(&value); err != nil || value > uint64(^uint16(0)) {
				return nil, fmt.Errorf("%s.value must be an unsigned 16-bit integer", argWhere)
			}
			arg.Short = uint16(value)
		case "string", "wstring":
			value, err := scalarString(input.Value)
			if err != nil {
				return nil, fmt.Errorf("%s.value: %w", argWhere, err)
			}
			if strings.ContainsRune(value, '\x00') {
				return nil, fmt.Errorf("%s.value cannot contain a NUL byte", argWhere)
			}
			if err := validateFixturePlaceholder(value, fixtureVariables); err != nil {
				return nil, fmt.Errorf("%s.value: %w", argWhere, err)
			}
			arg.String = value
		case "file":
			value, err := scalarString(input.Value)
			if err != nil {
				return nil, fmt.Errorf("%s.value: %w", argWhere, err)
			}
			path, err := paths.file(value, argWhere+".value")
			if err != nil {
				return nil, err
			}
			arg.File = path
		case "bytes_base64":
			value, err := scalarString(input.Value)
			if err != nil {
				return nil, fmt.Errorf("%s.value: %w", argWhere, err)
			}
			decoded, err := decodeBase64(value, argWhere+".value")
			if err != nil {
				return nil, err
			}
			inlineBytes += len(decoded)
			if inlineBytes > maxInlineBytes {
				return nil, fmt.Errorf("%s inline byte arguments exceed %d bytes", where, maxInlineBytes)
			}
			arg.Bytes = decoded
		default:
			return nil, fmt.Errorf("%s.type %q is unsupported", argWhere, input.Type)
		}
		args = append(args, arg)
	}
	return args, nil
}

func validateFixturePlaceholder(value string, fixtureVariables map[string]struct{}) error {
	if !strings.Contains(value, "${") {
		return nil
	}
	match := fixturePlaceholderPattern.FindStringSubmatch(value)
	if match == nil {
		return fmt.Errorf("fixture substitution must be the entire value in the form ${NAME}")
	}
	if _, ok := fixtureVariables[strings.ToUpper(match[1])]; !ok {
		return fmt.Errorf("unknown fixture variable %q", match[1])
	}
	return nil
}

func validateTCPListener(settings Settings, where string) error {
	if settings.TCPListener == nil {
		return nil
	}
	if settings.HasRawArgs {
		return fmt.Errorf("%s.tcp_listener cannot be used with args_raw_base64", where)
	}
	index := settings.TCPListener.PortArgument
	if index < 0 || index >= len(settings.Args) {
		return fmt.Errorf("%s.tcp_listener.port_argument %d must index args", where, index)
	}
	if settings.Args[index].Type != "int" {
		return fmt.Errorf("%s.args[%d].type must be int for tcp_listener.port_argument", where, index)
	}
	return nil
}

func scalarString(node yaml.Node) (string, error) {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return "", fmt.Errorf("must be a string")
	}
	var value string
	if err := node.Decode(&value); err != nil {
		return "", fmt.Errorf("must be a string")
	}
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("must contain valid UTF-8")
	}
	return value, nil
}

func normalizeExpectation(raw rawExpectation, where string) (Expectation, error) {
	expect := Expectation{}
	if raw.SuccessOnly != nil {
		expect.SuccessOnly = *raw.SuccessOnly
	}
	if raw.Output != nil {
		output, err := normalizeOutputExpectation(*raw.Output, where+".output")
		if err != nil {
			return Expectation{}, err
		}
		expect.Output = output
	}
	if expect.SuccessOnly && hasOutputExpectation(expect.Output) {
		return Expectation{}, fmt.Errorf("%s.success_only cannot be true when output assertions are set", where)
	}
	if !validExpectation(expect) {
		return Expectation{}, fmt.Errorf("%s must explicitly set success_only: true or at least one output assertion", where)
	}
	return expect, nil
}

func normalizeOutputExpectation(raw rawOutputExpectation, where string) (OutputExpectation, error) {
	output := OutputExpectation{
		Equals:      raw.Equals,
		Contains:    append([]string(nil), raw.Contains...),
		NotContains: append([]string(nil), raw.NotContains...),
		Matches:     append([]string(nil), raw.Matches...),
	}
	for index, value := range output.Contains {
		if value == "" {
			return OutputExpectation{}, fmt.Errorf("%s.contains[%d] cannot be empty", where, index)
		}
	}
	for index, value := range output.NotContains {
		if value == "" {
			return OutputExpectation{}, fmt.Errorf("%s.not_contains[%d] cannot be empty", where, index)
		}
	}
	for index, pattern := range output.Matches {
		if pattern == "" {
			return OutputExpectation{}, fmt.Errorf("%s.matches[%d] cannot be empty", where, index)
		}
		if _, err := regexp.Compile(pattern); err != nil {
			return OutputExpectation{}, fmt.Errorf("%s.matches[%d] is not a valid Go regexp: %w", where, index, err)
		}
	}
	if raw.SHA256 != "" {
		if len(raw.SHA256) != 64 {
			return OutputExpectation{}, fmt.Errorf("%s.sha256 must contain exactly 64 hexadecimal characters", where)
		}
		if _, err := hex.DecodeString(raw.SHA256); err != nil {
			return OutputExpectation{}, fmt.Errorf("%s.sha256 must be hexadecimal", where)
		}
		output.SHA256 = strings.ToLower(raw.SHA256)
	}
	if raw.EqualsBase64 != nil {
		if raw.Equals != nil {
			return OutputExpectation{}, fmt.Errorf("%s cannot set both equals and equals_base64", where)
		}
		decoded, err := decodeBase64(*raw.EqualsBase64, where+".equals_base64")
		if err != nil {
			return OutputExpectation{}, err
		}
		output.EqualsBytes = decoded
		output.HasEqualsB64 = true
	}
	return output, nil
}

func validExpectation(expect Expectation) bool {
	return expect.SuccessOnly || hasOutputExpectation(expect.Output)
}

func hasOutputExpectation(output OutputExpectation) bool {
	return output.Equals != nil || len(output.Contains) != 0 || len(output.NotContains) != 0 ||
		len(output.Matches) != 0 || output.SHA256 != "" || output.HasEqualsB64
}

func cloneSettings(input Settings) Settings {
	output := input
	if input.TCPListener != nil {
		listener := *input.TCPListener
		output.TCPListener = &listener
	}
	if input.Expect.Output.Equals != nil {
		equals := *input.Expect.Output.Equals
		output.Expect.Output.Equals = &equals
	}
	output.Modes = append([]string(nil), input.Modes...)
	output.Args = append([]Argument(nil), input.Args...)
	for index := range output.Args {
		output.Args[index].Bytes = append([]byte(nil), input.Args[index].Bytes...)
	}
	if input.HasRawArgs {
		output.RawArgs = append([]byte{}, input.RawArgs...)
	} else {
		output.RawArgs = nil
	}
	output.Expect.Output.Contains = append([]string(nil), input.Expect.Output.Contains...)
	output.Expect.Output.NotContains = append([]string(nil), input.Expect.Output.NotContains...)
	output.Expect.Output.Matches = append([]string(nil), input.Expect.Output.Matches...)
	output.Expect.Output.EqualsBytes = append([]byte(nil), input.Expect.Output.EqualsBytes...)
	return output
}

func decodeBase64(value, where string) ([]byte, error) {
	decoded, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("%s must be canonical base64: %w", where, err)
	}
	if len(decoded) > maxInlineBytes {
		return nil, fmt.Errorf("%s decodes to more than %d bytes", where, maxInlineBytes)
	}
	return decoded, nil
}

func validateName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("must be non-empty")
	}
	if name != strings.TrimSpace(name) {
		return fmt.Errorf("must not have leading or trailing whitespace")
	}
	if strings.ContainsAny(name, "/\\") || strings.ContainsRune(name, '\x00') {
		return fmt.Errorf("must not contain path separators or NUL bytes")
	}
	return nil
}

func validateVersion(version string) error {
	if version == "latest" || version == "master" || versionPattern.MatchString(version) || commitPattern.MatchString(version) {
		return nil
	}
	return fmt.Errorf("must be latest, master, a vX.Y.Z release tag, or a full 40-character commit SHA")
}

func appendUniquePlan(plans *[]Plan, ids map[string]struct{}, plan Plan) error {
	if _, exists := ids[plan.ID]; exists {
		return fmt.Errorf("expanded plan ID %q is duplicated", plan.ID)
	}
	ids[plan.ID] = struct{}{}
	*plans = append(*plans, plan)
	return nil
}
