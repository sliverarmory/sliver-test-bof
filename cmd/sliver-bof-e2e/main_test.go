package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sliverarmory/sliver-test-bof/internal/acquire"
	"github.com/sliverarmory/sliver-test-bof/internal/manifest"
	"github.com/sliverarmory/sliver-test-bof/internal/results"
)

func TestRunRequiresPreparedDescriptor(t *testing.T) {
	err := run(context.Background(), []string{"run"})
	if err == nil || !strings.Contains(err.Error(), "--prepared is required") {
		t.Fatalf("run without prepared descriptor error = %v", err)
	}
}

func TestChooseTarget(t *testing.T) {
	single := manifest.Matrix{Include: []manifest.MatrixEntry{
		{Target: "windows/amd64", Runner: "windows-2022", Key: "windows-amd64"},
	}}
	multiple := manifest.Matrix{Include: []manifest.MatrixEntry{
		{Target: "linux/arm64", Runner: "ubuntu-24.04-arm", Key: "linux-arm64"},
		{Target: "windows/amd64", Runner: "windows-2022", Key: "windows-amd64"},
	}}

	tests := []struct {
		name      string
		requested string
		matrix    manifest.Matrix
		want      string
		wantError string
	}{
		{name: "implicit single target", matrix: single, want: "windows/amd64"},
		{name: "explicit target", requested: "linux/arm64", matrix: multiple, want: "linux/arm64"},
		{name: "explicit target trims whitespace", requested: "  windows/amd64\t", matrix: multiple, want: "windows/amd64"},
		{name: "multiple require explicit", matrix: multiple, wantError: "--target is required when the manifest contains 2 targets"},
		{name: "empty matrix", matrix: manifest.Matrix{}, wantError: "manifest contains 0 targets"},
		{name: "target absent", requested: "darwin/arm64", matrix: multiple, wantError: `target "darwin/arm64" is not present`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := chooseTarget(test.requested, test.matrix)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("chooseTarget() error = %v, want substring %q", err, test.wantError)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("chooseTarget() = %q, %v; want %q, nil", got, err, test.want)
			}
		})
	}
}

func TestParseTarget(t *testing.T) {
	target, err := parseTarget("linux/arm64")
	if err != nil || target.OS != "linux" || target.Arch != "arm64" {
		t.Fatalf("parseTarget(valid) = %#v, %v", target, err)
	}
	for _, value := range []string{"", "linux", "/amd64", "linux/", "linux/amd64/extra"} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseTarget(value); err == nil {
				t.Fatalf("parseTarget(%q) unexpectedly succeeded", value)
			}
		})
	}
}

func TestVerifyTargetCoverage(t *testing.T) {
	matrix := manifest.Matrix{Include: []manifest.MatrixEntry{
		{Target: "linux/arm64", Runner: "ubuntu-24.04-arm", Key: "linux-arm64"},
		{Target: "windows/amd64", Runner: "windows-2022", Key: "windows-amd64"},
	}}
	tests := []struct {
		name      string
		reports   []results.Report
		wantError []string
	}{
		{
			name:    "complete",
			reports: []results.Report{{Target: "windows/amd64"}, {Target: "linux/arm64"}},
		},
		{
			name:      "missing",
			reports:   []results.Report{{Target: "windows/amd64"}},
			wantError: []string{"missing report for linux/arm64"},
		},
		{
			name:      "duplicate",
			reports:   []results.Report{{Target: "linux/arm64"}, {Target: "linux/arm64"}, {Target: "windows/amd64"}},
			wantError: []string{"duplicate report for linux/arm64"},
		},
		{
			name:      "unexpected",
			reports:   []results.Report{{Target: "linux/arm64"}, {Target: "windows/amd64"}, {Target: "darwin/arm64"}},
			wantError: []string{"unexpected report for darwin/arm64"},
		},
		{
			name:    "combined diagnostics are sorted",
			reports: []results.Report{{Target: "darwin/arm64"}, {Target: "darwin/arm64"}},
			wantError: []string{
				"duplicate report for darwin/arm64",
				"missing report for linux/arm64",
				"missing report for windows/amd64",
				"unexpected report for darwin/arm64",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := verifyTargetCoverage(matrix, test.reports)
			if len(test.wantError) == 0 {
				if err != nil {
					t.Fatalf("verifyTargetCoverage() error = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("verifyTargetCoverage() unexpectedly succeeded")
			}
			got := strings.Split(err.Error(), "; ")
			if !equalStringSlices(got, test.wantError) {
				t.Fatalf("verifyTargetCoverage() diagnostics = %v, want %v", got, test.wantError)
			}
		})
	}
}

func TestVerifyCaseCoverage(t *testing.T) {
	object := filepath.Join(t.TempDir(), "example.o")
	objectData := []byte("test object bytes")
	if err := os.WriteFile(object, objectData, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(objectData)
	digestText := hex.EncodeToString(digest[:])
	plan := manifest.Plan{
		ID: "core/example", Suite: "core", Name: "example", Object: object,
		Target: manifest.Target{OS: "linux", Arch: "amd64"},
		Settings: manifest.Settings{
			BOFExecutor: manifest.ExecutorReflektor,
			Modes:       []string{manifest.ModeSession, manifest.ModeBeacon},
			Repeat:      2,
		},
	}
	result := func(mode string, repeat int) results.CaseResult {
		return results.CaseResult{
			ID: plan.ID, Suite: plan.Suite, Test: plan.Name, ObjectSHA256: digestText,
			Mode: mode, Executor: plan.Settings.BOFExecutor, Repeat: repeat, Status: "pass",
		}
	}
	complete := results.Report{Target: plan.Target.Key(), Cases: []results.CaseResult{
		result(manifest.ModeSession, 1), result(manifest.ModeSession, 2),
		result(manifest.ModeBeacon, 1), result(manifest.ModeBeacon, 2),
	}}

	if err := verifyCaseCoverage([]manifest.Plan{plan}, []results.Report{complete}); err != nil {
		t.Fatalf("verifyCaseCoverage(complete) error = %v", err)
	}

	tests := []struct {
		name string
		edit func(*results.Report)
		want string
	}{
		{name: "missing", edit: func(report *results.Report) { report.Cases = report.Cases[:3] }, want: "missing case linux/amd64/core/example/beacon#2"},
		{name: "duplicate", edit: func(report *results.Report) { report.Cases = append(report.Cases, report.Cases[0]) }, want: "duplicate case linux/amd64/core/example/session#1"},
		{name: "unexpected", edit: func(report *results.Report) { report.Cases[0].Repeat = 3 }, want: "unexpected case linux/amd64/core/example/session#3"},
		{name: "metadata mismatch", edit: func(report *results.Report) { report.Cases[0].ObjectSHA256 = "wrong" }, want: "metadata mismatch for linux/amd64/core/example/session#1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			modified := complete
			modified.Cases = append([]results.CaseResult(nil), complete.Cases...)
			test.edit(&modified)
			err := verifyCaseCoverage([]manifest.Plan{plan}, []results.Report{modified})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("verifyCaseCoverage() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestVerifyReportProvenance(t *testing.T) {
	reports := []results.Report{
		{Target: "linux/amd64", ActionVersion: "owner/action@abc", Sliver: results.Provenance{Selector: "release:v1.2.3@0123456789012345678901234567890123456789", ResolvedSHA: "0123456789012345678901234567890123456789"}},
		{Target: "windows/amd64", ActionVersion: "owner/action@abc", Sliver: results.Provenance{Selector: "release:v1.2.3@0123456789012345678901234567890123456789", ResolvedSHA: "0123456789012345678901234567890123456789"}},
	}
	if err := verifyReportProvenance(reports); err != nil {
		t.Fatalf("verifyReportProvenance(matching) error = %v", err)
	}

	tests := []struct {
		name string
		edit func([]results.Report)
		want string
	}{
		{name: "missing SHA", edit: func(values []results.Report) { values[1].Sliver.ResolvedSHA = "" }, want: "missing resolved Sliver SHA for windows/amd64"},
		{name: "SHA mismatch", edit: func(values []results.Report) { values[1].Sliver.ResolvedSHA = strings.Repeat("a", 40) }, want: "Sliver SHA mismatch for windows/amd64"},
		{name: "selector mismatch", edit: func(values []results.Report) { values[1].Sliver.Selector = "master" }, want: "Sliver selector mismatch for windows/amd64"},
		{name: "action mismatch", edit: func(values []results.Report) { values[1].ActionVersion = "owner/action@def" }, want: "action version mismatch for windows/amd64"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			modified := append([]results.Report(nil), reports...)
			test.edit(modified)
			err := verifyReportProvenance(modified)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("verifyReportProvenance() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestSelectedAndResolvedSliverVersions(t *testing.T) {
	if got := selectedSliverVersion("  v1.7.3 ", "master"); got != "v1.7.3" {
		t.Fatalf("selectedSliverVersion(override) = %q", got)
	}
	if got := selectedSliverVersion("", " master "); got != "master" {
		t.Fatalf("selectedSliverVersion(configured) = %q", got)
	}
	pinnedRelease := "release:v1.7.3@0123456789012345678901234567890123456789"
	if got := resolvedSelector(acquire.Provenance{
		Selector: pinnedRelease, ResolvedTag: "v1.7.3", ResolvedSHA: strings.Repeat("0", 40),
	}); got != pinnedRelease {
		t.Fatalf("resolvedSelector(release) = %q, want %q", got, pinnedRelease)
	}
	if got := resolvedSelector(acquire.Provenance{Selector: "path", ResolvedSHA: "server-digest"}); got != "server-digest" {
		t.Fatalf("resolvedSelector(path) = %q, want server digest", got)
	}
}

func TestToolCacheDirUsesJobPrivateRunnerTemp(t *testing.T) {
	t.Setenv("RUNNER_TEMP", filepath.Join(t.TempDir(), "job-temp"))
	t.Setenv("RUNNER_TOOL_CACHE", filepath.Join(t.TempDir(), "shared-tool-cache"))
	got, err := toolCacheDir()
	if err != nil {
		t.Fatalf("toolCacheDir() error = %v", err)
	}
	if !strings.HasSuffix(got, filepath.Join("job-temp", "sliver-bof-e2e-cache")) {
		t.Fatalf("toolCacheDir() = %q, want private RUNNER_TEMP cache", got)
	}
}

func TestPreparedDescriptorRoundTripAndIntegrity(t *testing.T) {
	directory := t.TempDir()
	serverPath := filepath.Join(directory, "sliver-server")
	serverData := []byte("prepared server bytes")
	if err := os.WriteFile(serverPath, serverData, 0o700); err != nil {
		t.Fatal(err)
	}
	serverDigest := sha256.Sum256(serverData)
	loaderData := []byte("prepared loader bytes")
	loaderDigest := sha256.Sum256(loaderData)
	document := preparedDocument{
		Schema: preparedSchema, Target: "windows/amd64", Selector: "master",
		ServerPath: serverPath,
		Sliver: acquire.Provenance{
			Selector: "master", ResolvedSHA: strings.Repeat("a", 40),
			Digest: hex.EncodeToString(serverDigest[:]),
		},
		Loader: &preparedLoader{
			Data: loaderData, Export: "LoadAndRun", Digest: hex.EncodeToString(loaderDigest[:]),
			Provenance: acquire.Provenance{Digest: hex.EncodeToString(loaderDigest[:])},
		},
	}
	descriptorPath := filepath.Join(directory, "private", "prepared.json")
	if err := writePreparedDescriptor(descriptorPath, document); err != nil {
		t.Fatalf("writePreparedDescriptor() error = %v", err)
	}
	if info, err := os.Stat(descriptorPath); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("prepared descriptor mode = %o, want 600", info.Mode().Perm())
	}
	loaded, err := readPreparedDescriptor(descriptorPath, "windows/amd64", "master", true)
	if err != nil {
		t.Fatalf("readPreparedDescriptor() error = %v", err)
	}
	if loaded.ServerPath != serverPath || loaded.Loader == nil || string(loaded.Loader.Data) != string(loaderData) {
		t.Fatalf("readPreparedDescriptor() = %#v", loaded)
	}
	if _, err := readPreparedDescriptor(descriptorPath, "windows/386", "master", true); err == nil || !strings.Contains(err.Error(), "does not match requested target") {
		t.Fatalf("target mismatch error = %v", err)
	}
	if err := os.WriteFile(serverPath, []byte("changed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readPreparedDescriptor(descriptorPath, "windows/amd64", "master", true); err == nil || !strings.Contains(err.Error(), "does not match prepared provenance") {
		t.Fatalf("server tamper error = %v", err)
	}
}

func TestPreparedDescriptorRejectsPublicOrUnexpectedContent(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "prepared.json")
	if err := os.WriteFile(path, []byte(`{"schema":"sliver-bof-e2e-prepared/v1","unexpected":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPreparedDescriptor(path, "linux/amd64", "master", false); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := readPreparedDescriptor(path, "linux/amd64", "master", false); err == nil || !strings.Contains(err.Error(), "must not be accessible") {
			t.Fatalf("public descriptor error = %v", err)
		}
	}
}

func TestResolveActionVersionPrecedenceAndValidation(t *testing.T) {
	t.Setenv("GITHUB_ACTION_REPOSITORY", "owner/action")
	t.Setenv("GITHUB_ACTION_REF", strings.Repeat("a", 40))
	t.Setenv("SLIVER_BOF_ACTION_VERSION", "owner/workflow@"+strings.Repeat("b", 40))
	t.Setenv("SLIVER_BOF_E2E_ACTION_VERSION", "owner/e2e@"+strings.Repeat("c", 40))
	if got, err := resolveActionVersion(""); err != nil || got != "owner/e2e@"+strings.Repeat("c", 40) {
		t.Fatalf("resolveActionVersion(env) = %q, %v", got, err)
	}
	if got, err := resolveActionVersion("owner/explicit@" + strings.Repeat("d", 40)); err != nil || got != "owner/explicit@"+strings.Repeat("d", 40) {
		t.Fatalf("resolveActionVersion(explicit) = %q, %v", got, err)
	}
	if _, err := resolveActionVersion("bad\nversion"); err == nil {
		t.Fatal("resolveActionVersion(newline) unexpectedly succeeded")
	}
}

func equalStringSlices(got, want []string) bool {
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
