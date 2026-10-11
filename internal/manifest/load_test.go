package manifest

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLoadNormalizesExpandsAndBuildsMatrix(t *testing.T) {
	workspace := t.TempDir()
	writeCOFFObject(t, workspace, "objects/direct.o", 0x8664)
	writeCOFFObject(t, workspace, "objects/group/b.o", 0x8664)
	writeCOFFObject(t, workspace, "objects/group/a.o", 0x8664)
	writeFile(t, workspace, "fixtures/input.bin", []byte("file argument"))

	manifestPath := writeManifest(t, workspace, `
schema: sliver-bof-e2e/v1
sliver:
  version: master
defaults:
  bof_executor: reflektor
  entrypoint: go
  modes: [session, beacon]
  repeat: 1
  command_timeout: 2m
suites:
  - name: windows-amd64
    target: {os: windows, arch: amd64}
    tests:
      - name: direct
        object: objects/direct.o
        modes: [session]
        repeat: 2
        args:
          - {type: integer, value: 4294967295}
          - {type: short, value: 65535}
          - {type: string, value: hello}
          - {type: wstring, value: world}
          - {type: file, value: fixtures/input.bin}
          - {type: bytes_base64, value: AQID}
        expect:
          output:
            contains: [hello]
            not_contains: [failure]
            matches: ['(?m)^hello']
    collections:
      - name: smoke
        glob: objects/group/*.o
        expect:
          success_only: true
        overrides:
          a.o:
            expect:
              output:
                equals_base64: ''
          b.o:
            bof_executor: coff-loader
            modes: [beacon]
            repeat: 3
            args_raw_base64: AQI=
`)

	loaded, err := Load(filepath.Base(manifestPath), workspace)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Workspace != canonical(t, workspace) {
		t.Fatalf("Workspace = %q, want %q", loaded.Workspace, canonical(t, workspace))
	}
	if len(loaded.Plans) != 3 {
		t.Fatalf("len(Plans) = %d, want 3", len(loaded.Plans))
	}
	direct := loaded.Plans[0]
	if direct.ID != "windows-amd64/direct" || direct.Settings.Repeat != 2 {
		t.Fatalf("direct plan = %#v", direct)
	}
	if got, want := direct.Settings.Modes, []string{ModeSession}; !equalStrings(got, want) {
		t.Fatalf("direct modes = %v, want %v", got, want)
	}
	if direct.Settings.CommandTimeout != 2*time.Minute {
		t.Fatalf("direct timeout = %s", direct.Settings.CommandTimeout)
	}
	if len(direct.Settings.Args) != 6 {
		t.Fatalf("len(direct args) = %d, want 6", len(direct.Settings.Args))
	}
	if direct.Settings.Args[0].Type != "int" || direct.Settings.Args[0].Integer != ^uint32(0) {
		t.Fatalf("normalized int = %#v", direct.Settings.Args[0])
	}
	if direct.Settings.Args[1].Short != ^uint16(0) {
		t.Fatalf("normalized short = %#v", direct.Settings.Args[1])
	}
	if direct.Settings.Args[4].File != canonical(t, filepath.Join(workspace, "fixtures/input.bin")) {
		t.Fatalf("normalized file = %q", direct.Settings.Args[4].File)
	}
	if got := direct.Settings.Args[5].Bytes; string(got) != "\x01\x02\x03" {
		t.Fatalf("normalized bytes = %x", got)
	}

	if loaded.Plans[1].ID != "windows-amd64/smoke/a.o" || loaded.Plans[2].ID != "windows-amd64/smoke/b.o" {
		t.Fatalf("collection plans are not basename-sorted: %q, %q", loaded.Plans[1].ID, loaded.Plans[2].ID)
	}
	if !loaded.Plans[1].Settings.Expect.Output.HasEqualsB64 || len(loaded.Plans[1].Settings.Expect.Output.EqualsBytes) != 0 {
		t.Fatalf("empty binary equality was not preserved: %#v", loaded.Plans[1].Settings.Expect.Output)
	}
	overridden := loaded.Plans[2].Settings
	if overridden.BOFExecutor != ExecutorCOFFLoader || overridden.Repeat != 3 || !equalStrings(overridden.Modes, []string{ModeBeacon}) {
		t.Fatalf("collection override = %#v", overridden)
	}
	if !overridden.HasRawArgs || string(overridden.RawArgs) != "\x01\x02" || len(overridden.Args) != 0 {
		t.Fatalf("collection raw args = %x; typed args = %#v", overridden.RawArgs, overridden.Args)
	}

	matrixJSON, err := json.Marshal(loaded.Matrix())
	if err != nil {
		t.Fatalf("json.Marshal(Matrix()) error = %v", err)
	}
	wantMatrix := `{"include":[{"target":"windows/amd64","runner":"windows-2022","key":"windows-amd64"}]}`
	if string(matrixJSON) != wantMatrix {
		t.Fatalf("Matrix JSON = %s, want %s", matrixJSON, wantMatrix)
	}
	if got := len(loaded.PlansForTarget("windows/amd64")); got != 3 {
		t.Fatalf("PlansForTarget() length = %d, want 3", got)
	}
}

func TestLoadExpandsPluralBOFExecutorsAndPreservesScalarIDs(t *testing.T) {
	workspace := t.TempDir()
	writeCOFFObject(t, workspace, "objects/shared.o", 0x8664)
	path := writeManifest(t, workspace, `
schema: sliver-bof-e2e/v1
sliver: {version: master}
suites:
  - name: suite
    target: {os: windows, arch: amd64}
    tests:
      - name: both
        object: objects/shared.o
        bof_executors: [reflektor, coff-loader]
        modes: [session, beacon]
        repeat: 2
        args: [{type: string, value: shared}]
        expect: {output: {contains: [shared]}}
      - name: singular
        object: objects/shared.o
        bof_executor: coff-loader
        expect: {success_only: true}
      - name: implicit
        object: objects/shared.o
        expect: {success_only: true}
      - name: singleton-list
        object: objects/shared.o
        bof_executors: [coff-loader]
        expect: {success_only: true}
`)
	loaded, err := Load(path, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Plans) != 5 {
		t.Fatalf("len(Plans) = %d, want 5", len(loaded.Plans))
	}
	want := map[string]string{
		"suite/both/reflektor":   ExecutorReflektor,
		"suite/both/coff-loader": ExecutorCOFFLoader,
		"suite/singular":         ExecutorCOFFLoader,
		"suite/implicit":         ExecutorReflektor,
		"suite/singleton-list":   ExecutorCOFFLoader,
	}
	seen := make(map[string]struct{}, len(want))
	for _, plan := range loaded.Plans {
		executor, ok := want[plan.ID]
		if !ok {
			t.Errorf("unexpected plan ID %q", plan.ID)
			continue
		}
		if _, exists := seen[plan.ID]; exists {
			t.Errorf("duplicate plan ID %q", plan.ID)
		}
		seen[plan.ID] = struct{}{}
		if plan.Settings.BOFExecutor != executor || plan.ObjectPath != "objects/shared.o" {
			t.Errorf("plan %q = executor %q, object %q", plan.ID, plan.Settings.BOFExecutor, plan.ObjectPath)
		}
		if plan.Name == "both" && (plan.Settings.Repeat != 2 || !equalStrings(plan.Settings.Modes, []string{ModeSession, ModeBeacon}) || len(plan.Settings.Args) != 1 || !equalStrings(plan.Settings.Expect.Output.Contains, []string{"shared"})) {
			t.Errorf("dual executor settings differ from manifest: %#v", plan.Settings)
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("seen plan IDs = %v, want %v", seen, want)
	}
}

func TestLoadExpandsCollectionBOFExecutorsWithOverride(t *testing.T) {
	workspace := t.TempDir()
	writeCOFFObject(t, workspace, "objects/a.o", 0x8664)
	writeCOFFObject(t, workspace, "objects/b.o", 0x8664)
	path := writeManifest(t, workspace, `
schema: sliver-bof-e2e/v1
sliver: {version: master}
suites:
  - name: suite
    target: {os: windows, arch: amd64}
    collections:
      - name: broad
        glob: objects/*.o
        bof_executors: [reflektor, coff-loader]
        expect: {success_only: true}
        overrides:
          a.o:
            bof_executors: [coff-loader]
      - name: selected
        glob: objects/*.o
        bof_executor: reflektor
        expect: {success_only: true}
        overrides:
          b.o:
            bof_executors: [reflektor, coff-loader]
`)
	loaded, err := Load(path, workspace)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"suite/broad/a.o":                ExecutorCOFFLoader,
		"suite/broad/b.o/reflektor":      ExecutorReflektor,
		"suite/broad/b.o/coff-loader":    ExecutorCOFFLoader,
		"suite/selected/a.o":             ExecutorReflektor,
		"suite/selected/b.o/reflektor":   ExecutorReflektor,
		"suite/selected/b.o/coff-loader": ExecutorCOFFLoader,
	}
	if len(loaded.Plans) != len(want) {
		t.Fatalf("len(Plans) = %d, want %d", len(loaded.Plans), len(want))
	}
	for _, plan := range loaded.Plans {
		executor, ok := want[plan.ID]
		if !ok {
			t.Errorf("unexpected plan ID %q", plan.ID)
			continue
		}
		if plan.Settings.BOFExecutor != executor {
			t.Errorf("plan %q executor = %q, want %q", plan.ID, plan.Settings.BOFExecutor, executor)
		}
		delete(want, plan.ID)
	}
	if len(want) != 0 {
		t.Fatalf("missing collection plans: %v", want)
	}
}

func TestLoadInheritsPluralBOFExecutorDefaults(t *testing.T) {
	workspace := t.TempDir()
	writeCOFFObject(t, workspace, "shared.o", 0x8664)
	path := writeManifest(t, workspace, `
schema: sliver-bof-e2e/v1
sliver: {version: master}
defaults:
  bof_executors: [reflektor, coff-loader]
suites:
  - name: suite
    target: {os: windows, arch: amd64}
    tests:
      - name: inherited
        object: shared.o
        expect: {success_only: true}
      - name: narrowed
        object: shared.o
        bof_executor: reflektor
        expect: {success_only: true}
`)
	loaded, err := Load(path, workspace)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"suite/inherited/reflektor":   ExecutorReflektor,
		"suite/inherited/coff-loader": ExecutorCOFFLoader,
		"suite/narrowed":              ExecutorReflektor,
	}
	if len(loaded.Plans) != len(want) {
		t.Fatalf("len(Plans) = %d, want %d", len(loaded.Plans), len(want))
	}
	for _, plan := range loaded.Plans {
		if executor, ok := want[plan.ID]; !ok || plan.Settings.BOFExecutor != executor {
			t.Errorf("unexpected inherited plan: ID %q executor %q", plan.ID, plan.Settings.BOFExecutor)
		}
	}
}

func TestLoadRejectsInvalidPluralBOFExecutors(t *testing.T) {
	workspace := t.TempDir()
	writeCOFFObject(t, workspace, "test.o", 0x8664)
	writeCOFFObject(t, workspace, "arm64.o", 0xaa64)
	base := `
schema: sliver-bof-e2e/v1
sliver: {version: master}
suites:
  - name: suite
    target: {os: windows, arch: amd64}
    tests:
      - name: test
        object: test.o
        bof_executors: [reflektor, coff-loader]
        expect: {success_only: true}
`
	tests := []struct {
		name, manifest string
		want           []string
	}{
		{"empty", strings.Replace(base, "[reflektor, coff-loader]", "[]", 1), []string{"bof_executors"}},
		{"too many", strings.Replace(base, "[reflektor, coff-loader]", "[reflektor, coff-loader, reflektor]", 1), []string{"bof_executors"}},
		{"duplicate", strings.Replace(base, "[reflektor, coff-loader]", "[reflektor, reflektor]", 1), []string{"bof_executors", "duplicate"}},
		{"unknown", strings.Replace(base, "[reflektor, coff-loader]", "[reflektor, auto]", 1), []string{"bof_executors", "auto"}},
		{"scalar and plural", strings.Replace(base, "        bof_executors:", "        bof_executor: reflektor\n        bof_executors:", 1), []string{"bof_executor", "bof_executors"}},
		{"defaults scalar and plural", strings.Replace(base, "suites:\n", "defaults: {bof_executor: reflektor, bof_executors: [coff-loader]}\nsuites:\n", 1), []string{"bof_executor", "bof_executors"}},
		{"unsupported target", strings.Replace(strings.Replace(base, "windows, arch: amd64", "windows, arch: arm64", 1), "object: test.o", "object: arm64.o", 1), []string{"coff-loader", "windows/arm64"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeManifest(t, workspace, test.manifest)
			_, err := Load(path, workspace)
			if err == nil {
				t.Fatal("Load() unexpectedly accepted invalid bof_executors")
			}
			for _, want := range test.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("Load() error = %v, want substring %q", err, want)
				}
			}
		})
	}
}

func TestMatrixUsesUniqueDeterministicTargetOrder(t *testing.T) {
	loaded := &Manifest{Plans: []Plan{
		{Target: Target{OS: "windows", Arch: "amd64"}},
		{Target: Target{OS: "linux", Arch: "arm64"}},
		{Target: Target{OS: "windows", Arch: "amd64"}},
		{Target: Target{OS: "darwin", Arch: "arm64"}},
	}}
	matrix := loaded.Matrix()
	got := make([]string, 0, len(matrix.Include))
	for _, entry := range matrix.Include {
		got = append(got, entry.Target+"@"+entry.Runner)
	}
	want := []string{
		"darwin/arm64@macos-15",
		"linux/arm64@ubuntu-24.04-arm",
		"windows/amd64@windows-2022",
	}
	if !equalStrings(got, want) {
		t.Fatalf("Matrix order = %v, want %v", got, want)
	}
}

func TestLoadNormalizesFixturesAndTCPListener(t *testing.T) {
	workspace := t.TempDir()
	writeCOFFObject(t, workspace, "probe.o", 0x8664)
	manifestPath := writeManifest(t, workspace, `
schema: sliver-bof-e2e/v1
sliver: {version: master}
fixtures:
  file:
    variable: FIXTURE_FILE
    contents_utf8: "fixture contents\n"
  directory:
    variable: FIXTURE_DIR
    files:
      marker.txt: "directory contents\n"
      nested/child.txt: child
suites:
  - name: windows-amd64
    target: {os: windows, arch: amd64}
    tests:
      - name: probe
        object: probe.o
        args:
          - {type: string, value: '${FIXTURE_FILE}'}
          - {type: wstring, value: '${FIXTURE_DIR}'}
          - {type: int, value: 1}
        tcp_listener: {port_argument: 2}
        expect: {success_only: true}
`)
	loaded, err := Load(manifestPath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Config.Fixtures.File == nil || loaded.Config.Fixtures.File.Variable != "FIXTURE_FILE" || loaded.Config.Fixtures.File.ContentsUTF8 != "fixture contents\n" {
		t.Fatalf("file fixture = %#v", loaded.Config.Fixtures.File)
	}
	if loaded.Config.Fixtures.Directory == nil || loaded.Config.Fixtures.Directory.Variable != "FIXTURE_DIR" || loaded.Config.Fixtures.Directory.Files["nested/child.txt"] != "child" {
		t.Fatalf("directory fixture = %#v", loaded.Config.Fixtures.Directory)
	}
	settings := loaded.Plans[0].Settings
	if settings.Args[0].String != "${FIXTURE_FILE}" || settings.Args[1].String != "${FIXTURE_DIR}" {
		t.Fatalf("fixture arguments = %#v", settings.Args)
	}
	if settings.TCPListener == nil || settings.TCPListener.PortArgument != 2 {
		t.Fatalf("TCP listener = %#v", settings.TCPListener)
	}
	if loaded.Plans[0].ObjectPath != "probe.o" {
		t.Fatalf("ObjectPath = %q, want probe.o", loaded.Plans[0].ObjectPath)
	}
}

func TestLoadRejectsUnsafeFixturesAndTCPListeners(t *testing.T) {
	workspace := t.TempDir()
	writeCOFFObject(t, workspace, "test.o", 0x8664)
	base := `
schema: sliver-bof-e2e/v1
sliver: {version: master}
fixtures:
  file: {variable: FIXTURE_FILE, contents_utf8: fixture}
suites:
  - name: suite
    target: {os: windows, arch: amd64}
    tests:
      - name: test
        object: test.o
        args:
          - {type: string, value: '${FIXTURE_FILE}'}
          - {type: int, value: 1}
        expect: {success_only: true}
`
	tests := []struct {
		name, manifest, want string
	}{
		{"unknown variable", strings.Replace(base, "${FIXTURE_FILE}", "${UNKNOWN}", 1), "unknown fixture variable"},
		{"partial substitution", strings.Replace(base, "${FIXTURE_FILE}", "prefix-${FIXTURE_FILE}", 1), "must be the entire value"},
		{"missing file contents", strings.Replace(base, ", contents_utf8: fixture", "", 1), "contents_utf8 is required"},
		{"null file contents", strings.Replace(base, "contents_utf8: fixture", "contents_utf8: null", 1), "contents_utf8 is required"},
		{"duplicate variable", strings.Replace(base, "suites:", "  directory: {variable: FIXTURE_FILE, files: {}}\nsuites:", 1), "is duplicated"},
		{"invalid variable", strings.Replace(base, "FIXTURE_FILE", "fixture-file", 1), "must match"},
		{"directory traversal", strings.Replace(base, "suites:", "  directory:\n    variable: FIXTURE_DIR\n    files: {'../escape.txt': value}\nsuites:", 1), "clean relative path"},
		{"directory backslash", strings.Replace(base, "suites:", "  directory:\n    variable: FIXTURE_DIR\n    files: {'dir\\\\escape.txt': value}\nsuites:", 1), "not portable"},
		{"directory wildcard", strings.Replace(base, "suites:", "  directory:\n    variable: FIXTURE_DIR\n    files: {'bad?.txt': value}\nsuites:", 1), "not portable"},
		{"directory case collision", strings.Replace(base, "suites:", "  directory:\n    variable: FIXTURE_DIR\n    files: {Marker.txt: one, marker.txt: two}\nsuites:", 1), "case-insensitive filesystems"},
		{"directory prefix collision", strings.Replace(base, "suites:", "  directory:\n    variable: FIXTURE_DIR\n    files: {parent: one, parent/child.txt: two}\nsuites:", 1), "prefix collision"},
		{"directory reserved name", strings.Replace(base, "suites:", "  directory:\n    variable: FIXTURE_DIR\n    files: {CON.txt: value}\nsuites:", 1), "Windows-reserved"},
		{"TCP index", strings.Replace(base, "        expect:", "        tcp_listener: {port_argument: 2}\n        expect:", 1), "must index args"},
		{"TCP missing port argument", strings.Replace(base, "        expect:", "        tcp_listener: {}\n        expect:", 1), "port_argument is required"},
		{"TCP null port argument", strings.Replace(base, "        expect:", "        tcp_listener: {port_argument: null}\n        expect:", 1), "port_argument is required"},
		{"TCP string type", strings.Replace(base, "        expect:", "        tcp_listener: {port_argument: 0}\n        expect:", 1), "must be int"},
		{"TCP raw args", strings.Replace(strings.Replace(base, "        args:\n          - {type: string, value: '${FIXTURE_FILE}'}\n          - {type: int, value: 1}\n", "        args_raw_base64: ''\n", 1), "        expect:", "        tcp_listener: {port_argument: 0}\n        expect:", 1), "cannot be used with args_raw_base64"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeManifest(t, workspace, test.manifest)
			_, err := Load(path, workspace)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestTCPListenerCollectionOverrideInheritance(t *testing.T) {
	workspace := t.TempDir()
	writeCOFFObject(t, workspace, "objects/probe.o", 0x8664)
	valid := writeManifest(t, workspace, `
schema: sliver-bof-e2e/v1
sliver: {version: master}
suites:
  - name: suite
    target: {os: windows, arch: amd64}
    collections:
      - name: probes
        glob: objects/*.o
        args:
          - {type: int, value: 1}
        expect: {success_only: true}
        overrides:
          probe.o:
            tcp_listener: {port_argument: 0}
`)
	loaded, err := Load(valid, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Plans[0].Settings.TCPListener == nil || loaded.Plans[0].Settings.TCPListener.PortArgument != 0 {
		t.Fatalf("inherited TCP listener settings = %#v", loaded.Plans[0].Settings)
	}

	invalid := writeManifest(t, workspace, `
schema: sliver-bof-e2e/v1
sliver: {version: master}
suites:
  - name: suite
    target: {os: windows, arch: amd64}
    collections:
      - name: probes
        glob: objects/*.o
        args:
          - {type: int, value: 1}
        tcp_listener: {port_argument: 0}
        expect: {success_only: true}
        overrides:
          probe.o:
            args_raw_base64: ''
`)
	if _, err := Load(invalid, workspace); err == nil || !strings.Contains(err.Error(), "cannot be used with args_raw_base64") {
		t.Fatalf("inherited TCP listener raw-argument error = %v", err)
	}
}

func TestRunnerForTargetAllowlist(t *testing.T) {
	tests := []struct {
		target Target
		runner string
	}{
		{Target{OS: "darwin", Arch: "amd64"}, "macos-15-intel"},
		{Target{OS: "darwin", Arch: "arm64"}, "macos-15"},
		{Target{OS: "linux", Arch: "386"}, "ubuntu-24.04"},
		{Target{OS: "linux", Arch: "amd64"}, "ubuntu-24.04"},
		{Target{OS: "linux", Arch: "arm64"}, "ubuntu-24.04-arm"},
		{Target{OS: "windows", Arch: "386"}, "windows-2022"},
		{Target{OS: "windows", Arch: "amd64"}, "windows-2022"},
		{Target{OS: "windows", Arch: "arm64"}, "windows-11-arm"},
	}
	for _, test := range tests {
		t.Run(test.target.Key(), func(t *testing.T) {
			runner, err := RunnerForTarget(test.target)
			if err != nil || runner != test.runner {
				t.Fatalf("RunnerForTarget(%s) = %q, %v; want %q", test.target.Key(), runner, err, test.runner)
			}
		})
	}
	if _, err := RunnerForTarget(Target{OS: "darwin", Arch: "386"}); err == nil {
		t.Fatal("RunnerForTarget(darwin/386) unexpectedly succeeded")
	}
}

func TestLoadRejectsInvalidManifests(t *testing.T) {
	baseWorkspace := t.TempDir()
	writeCOFFObject(t, baseWorkspace, "test.o", 0x8664)
	base := `
schema: sliver-bof-e2e/v1
sliver: {version: master}
suites:
  - name: suite
    target: {os: windows, arch: amd64}
    tests:
      - name: test
        object: test.o
        expect: {success_only: true}
`

	tests := []struct {
		name     string
		manifest string
		want     string
	}{
		{"unknown field", strings.Replace(base, "schema: sliver-bof-e2e/v1", "schema: sliver-bof-e2e/v1\nunknown: true", 1), "field unknown not found"},
		{"unknown nested field", strings.Replace(base, "        expect:", "        mystery: true\n        expect:", 1), "field mystery not found"},
		{"duplicate YAML key", strings.Replace(base, "        object: test.o", "        object: test.o\n        object: test.o", 1), "already defined"},
		{"wrong schema", strings.Replace(base, SchemaV1, "sliver-bof-e2e/v2", 1), `schema must be "sliver-bof-e2e/v1"`},
		{"bad version", strings.Replace(base, "master", "main", 1), "sliver.version"},
		{"unsupported target", strings.Replace(base, "windows, arch: amd64", "darwin, arch: 386", 1), `unsupported target "darwin/386"`},
		{"missing expectation", strings.Replace(base, "        expect: {success_only: true}\n", "", 1), ".expect must explicitly"},
		{"success and output", strings.Replace(base, "expect: {success_only: true}", "expect: {success_only: true, output: {equals: ok}}", 1), "success_only cannot be true"},
		{"empty output", strings.Replace(base, "expect: {success_only: true}", "expect: {output: {contains: []}}", 1), ".expect must explicitly"},
		{"bad regexp", strings.Replace(base, "expect: {success_only: true}", "expect: {output: {matches: ['[']}}", 1), "not a valid Go regexp"},
		{"bad executor", strings.Replace(base, "        expect:", "        bof_executor: auto\n        expect:", 1), ".bof_executor must be"},
		{"duplicate mode", strings.Replace(base, "        expect:", "        modes: [session, session]\n        expect:", 1), "duplicate mode"},
		{"repeat zero", strings.Replace(base, "        expect:", "        repeat: 0\n        expect:", 1), ".repeat must be"},
		{"bad timeout", strings.Replace(base, "        expect:", "        command_timeout: forever\n        expect:", 1), ".command_timeout"},
		{"duplicate test name", strings.Replace(base, "        expect: {success_only: true}\n", "        expect: {success_only: true}\n      - name: test\n        object: test.o\n        expect: {success_only: true}\n", 1), "duplicated in suite"},
		{"object traversal", strings.Replace(base, "object: test.o", "object: ../test.o", 1), "escapes workspace"},
		{"wrong extension", strings.Replace(base, "object: test.o", "object: test.obj", 1), "must have a .o extension"},
		{"args and raw", strings.Replace(base, "        expect:", "        args: []\n        args_raw_base64: ''\n        expect:", 1), "cannot set both args and args_raw_base64"},
		{"integer overflow", strings.Replace(base, "        expect:", "        args: [{type: int, value: 4294967296}]\n        expect:", 1), "unsigned 32-bit"},
		{"integer string", strings.Replace(base, "        expect:", "        args: [{type: int, value: '1'}]\n        expect:", 1), "unsigned 32-bit"},
		{"unsupported arg", strings.Replace(base, "        expect:", "        args: [{type: pointer, value: 1}]\n        expect:", 1), "is unsupported"},
		{"invalid base64", strings.Replace(base, "        expect:", "        args_raw_base64: '*bad*'\n        expect:", 1), "must be canonical base64"},
		{"second document", base + "---\n{}\n", "exactly one YAML document"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeManifest(t, baseWorkspace, test.manifest)
			_, err := Load(path, baseWorkspace)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestLoadRejectsCollectionErrors(t *testing.T) {
	workspace := t.TempDir()
	writeCOFFObject(t, workspace, "objects/a.o", 0x8664)
	writeCOFFObject(t, workspace, "objects/one/same.o", 0x8664)
	writeCOFFObject(t, workspace, "objects/two/same.o", 0x8664)
	base := `
schema: sliver-bof-e2e/v1
sliver: {version: latest}
suites:
  - name: suite
    target: {os: windows, arch: amd64}
    collections:
      - name: all
        glob: objects/*.o
        expect: {success_only: true}
`
	tests := []struct {
		name, manifest, want string
	}{
		{"empty glob", strings.Replace(base, "objects/*.o", "missing/*.o", 1), "matched no files"},
		{"absolute glob", strings.Replace(base, "objects/*.o", filepath.ToSlash(filepath.Join(workspace, "objects/*.o")), 1), "must be relative"},
		{"traversal glob", strings.Replace(base, "objects/*.o", "../*.o", 1), "escapes workspace"},
		{"duplicate basename", strings.Replace(base, "objects/*.o", "objects/*/*.o", 1), "duplicate basename"},
		{"unknown override", strings.Replace(base, "        expect: {success_only: true}\n", "        expect: {success_only: true}\n        overrides:\n          missing.o: {repeat: 2}\n", 1), "does not match a collection object"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeManifest(t, workspace, test.manifest)
			_, err := Load(path, workspace)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestLoadRejectsSymlinkEscapes(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	writeCOFFObject(t, outside, "outside.o", 0x8664)
	writeCOFFObject(t, workspace, "inside.o", 0x8664)
	writeFile(t, outside, "outside.bin", []byte("secret"))
	if err := os.Symlink(filepath.Join(outside, "outside.o"), filepath.Join(workspace, "escape.o")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	path := writeManifest(t, workspace, `
schema: sliver-bof-e2e/v1
sliver: {version: master}
suites:
  - name: suite
    target: {os: windows, arch: amd64}
    tests:
      - name: escape
        object: escape.o
        expect: {success_only: true}
`)
	_, err := Load(path, workspace)
	if err == nil || !strings.Contains(err.Error(), "resolves outside workspace") {
		t.Fatalf("Load() error = %v, want symlink escape error", err)
	}

	if err := os.Symlink(filepath.Join(outside, "outside.bin"), filepath.Join(workspace, "escape.bin")); err != nil {
		t.Fatalf("create file argument symlink: %v", err)
	}
	path = writeManifest(t, workspace, `
schema: sliver-bof-e2e/v1
sliver: {version: master}
suites:
  - name: suite
    target: {os: windows, arch: amd64}
    tests:
      - name: escape
        object: inside.o
        args:
          - {type: file, value: escape.bin}
        expect: {success_only: true}
`)
	_, err = Load(path, workspace)
	if err == nil || !strings.Contains(err.Error(), "resolves outside workspace") {
		t.Fatalf("Load() file argument error = %v, want symlink escape error", err)
	}
}

func TestPlannerRejectsButDirectLoadAllowsConfinedObjectSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated Windows privileges")
	}
	workspace := t.TempDir()
	realObject := writeCOFFObject(t, workspace, "objects/real.o", 0x8664)
	if err := os.Symlink(realObject, filepath.Join(workspace, "objects", "link.o")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	manifestPath := writeManifest(t, workspace, `
schema: sliver-bof-e2e/v1
sliver: {version: master}
suites:
  - name: suite
    target: {os: windows, arch: amd64}
    tests:
      - name: link
        object: objects/link.o
        expect: {success_only: true}
`)
	if _, err := Load(manifestPath, workspace); err != nil {
		t.Fatalf("direct Load() rejected confined object symlink: %v", err)
	}
	if _, err := LoadForPlanning(manifestPath, workspace); err == nil || !strings.Contains(err.Error(), "symlink component") {
		t.Fatalf("LoadForPlanning() error = %v, want symlink rejection", err)
	}
}

func TestPlannerRejectsAbsoluteExplicitObjectPath(t *testing.T) {
	workspace := t.TempDir()
	object := writeCOFFObject(t, workspace, "objects/test.o", 0x8664)
	manifestPath := writeManifest(t, workspace, fmt.Sprintf(`
schema: sliver-bof-e2e/v1
sliver: {version: master}
suites:
  - name: suite
    target: {os: windows, arch: amd64}
    tests:
      - name: absolute
        object: %q
        expect: {success_only: true}
`, filepath.ToSlash(canonical(t, object))))
	if _, err := Load(manifestPath, workspace); err != nil {
		t.Fatalf("direct Load() no longer accepts a confined absolute object: %v", err)
	}
	if _, err := LoadForPlanning(manifestPath, workspace); err == nil || !strings.Contains(err.Error(), "must be relative to the object root") {
		t.Fatalf("LoadForPlanning() absolute object error = %v", err)
	}
}

func TestLoadWithObjectRootExcludesCheckoutObjects(t *testing.T) {
	workspace := t.TempDir()
	objectRoot := t.TempDir()
	writeCOFFObject(t, workspace, "objects/stale.o", 0x8664)
	writeFile(t, workspace, "fixtures/input.bin", []byte("fixture"))
	writeCOFFObject(t, objectRoot, "objects/planned.o", 0x8664)
	manifestPath := writeManifest(t, workspace, `
schema: sliver-bof-e2e/v1
sliver: {version: master}
suites:
  - name: suite
    target: {os: windows, arch: amd64}
    collections:
      - name: exact
        glob: objects/*.o
        args:
          - {type: file, value: fixtures/input.bin}
        expect: {success_only: true}
`)
	loaded, err := LoadWithObjectRoot(manifestPath, workspace, objectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Plans) != 1 || loaded.Plans[0].ObjectPath != "objects/planned.o" {
		t.Fatalf("isolated plans = %#v", loaded.Plans)
	}
	if loaded.Plans[0].Settings.Args[0].File != canonical(t, filepath.Join(workspace, "fixtures/input.bin")) {
		t.Fatalf("file argument escaped caller workspace: %#v", loaded.Plans[0].Settings.Args[0])
	}
	if _, err := LoadWithObjectRoot(manifestPath, workspace, workspace); err == nil || !strings.Contains(err.Error(), "must not overlap") {
		t.Fatalf("overlapping object root error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(objectRoot, "unexpected.txt"), []byte("unexpected"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWithObjectRoot(manifestPath, workspace, objectRoot); err == nil || !strings.Contains(err.Error(), "non-object file") {
		t.Fatalf("unexpected artifact file error = %v", err)
	}
}

func TestLoadRejectsObjectArchitectureMismatch(t *testing.T) {
	workspace := t.TempDir()
	writeCOFFObject(t, workspace, "arm64.o", 0xaa64)
	path := writeManifest(t, workspace, `
schema: sliver-bof-e2e/v1
sliver: {version: master}
suites:
  - name: suite
    target: {os: windows, arch: amd64}
    tests:
      - name: mismatch
        object: arm64.o
        expect: {success_only: true}
`)
	_, err := Load(path, workspace)
	if err == nil || !strings.Contains(err.Error(), "COFF machine") {
		t.Fatalf("Load() error = %v, want COFF architecture mismatch", err)
	}
}

func TestSchemaFileIsValidJSON(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "schema", "sliver-bof-e2e-v1.schema.json"))
	if err != nil {
		t.Fatalf("ReadFile(schema) error = %v", err)
	}
	var value any
	if err := json.Unmarshal(contents, &value); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
}

func writeManifest(t *testing.T, workspace, contents string) string {
	t.Helper()
	path := filepath.Join(workspace, strings.ReplaceAll(t.Name(), "/", "-")+"-manifest.yml")
	if err := os.WriteFile(path, []byte(strings.TrimSpace(contents)+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(manifest) error = %v", err)
	}
	return path
}

func writeFile(t *testing.T, workspace, relative string, contents []byte) string {
	t.Helper()
	path := filepath.Join(workspace, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", path, err)
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
	return path
}

func writeCOFFObject(t *testing.T, workspace, relative string, machine uint16) string {
	t.Helper()
	header := make([]byte, 96)
	binary.LittleEndian.PutUint16(header[0:2], machine)
	return writeFile(t, workspace, relative, header)
}

func canonical(t *testing.T, path string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q) error = %v", path, err)
	}
	abs, err := filepath.Abs(real)
	if err != nil {
		t.Fatalf("Abs(%q) error = %v", real, err)
	}
	return filepath.Clean(abs)
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func ExampleManifest_Matrix() {
	loaded := &Manifest{Plans: []Plan{{Target: Target{OS: "windows", Arch: "amd64"}}}}
	encoded, _ := json.Marshal(loaded.Matrix())
	fmt.Println(string(encoded))
	// Output: {"include":[{"target":"windows/amd64","runner":"windows-2022","key":"windows-amd64"}]}
}
