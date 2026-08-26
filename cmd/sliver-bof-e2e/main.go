package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"

	"github.com/sliverarmory/sliver-test-bof/internal/acquire"
	"github.com/sliverarmory/sliver-test-bof/internal/harness"
	"github.com/sliverarmory/sliver-test-bof/internal/manifest"
	"github.com/sliverarmory/sliver-test-bof/internal/results"
)

const (
	defaultManifest        = ".github/sliver-bof-e2e.yml"
	planSchema             = "sliver-bof-e2e-plan/v1"
	preparedSchema         = "sliver-bof-e2e-prepared/v1"
	maxPreparedDescriptor  = 48 << 20
	maxPreparedLoaderBytes = 32 << 20
)

type commonFlags struct {
	manifestPath  string
	workspace     string
	resultsDir    string
	githubOutput  string
	sliverVersion string
	actionVersion string
}

type planDocument struct {
	Schema     string             `json:"schema"`
	Manifest   string             `json:"manifest"`
	Matrix     manifest.Matrix    `json:"matrix"`
	Resolution acquire.Resolution `json:"sliver"`
}

type aggregateDocument struct {
	Schema  string           `json:"schema"`
	Reports []results.Report `json:"reports"`
}

// preparedDocument is a private, short-lived handoff from acquisition to
// execution. In particular, it never contains the credential used to acquire
// Sliver or COFFLoader.
type preparedDocument struct {
	Schema     string             `json:"schema"`
	Target     string             `json:"target"`
	Selector   string             `json:"selector"`
	ServerPath string             `json:"server_path"`
	Sliver     acquire.Provenance `json:"sliver"`
	Loader     *preparedLoader    `json:"coff_loader,omitempty"`
}

type preparedLoader struct {
	Data       []byte             `json:"data_base64"`
	Init       string             `json:"init,omitempty"`
	Export     string             `json:"export"`
	Digest     string             `json:"sha256"`
	Provenance acquire.Provenance `json:"provenance"`
}

type stderrLogger struct{ logger *log.Logger }

func (logger stderrLogger) Printf(format string, args ...any) {
	logger.logger.Printf(format, args...)
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "sliver-bof-e2e: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("expected a command: plan, prepare, run, or aggregate")
	}
	switch args[0] {
	case "plan":
		return runPlan(ctx, args[1:])
	case "prepare":
		return runPrepare(ctx, args[1:])
	case "run":
		return runTarget(ctx, args[1:])
	case "aggregate":
		return runAggregate(args[1:])
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q; expected plan, prepare, run, or aggregate", args[0])
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage: sliver-bof-e2e <plan|prepare|run|aggregate> [options]")
}

func addCommonFlags(flags *flag.FlagSet, includeVersion bool) *commonFlags {
	values := &commonFlags{}
	flags.StringVar(&values.manifestPath, "manifest", defaultManifest, "path to the test manifest")
	flags.StringVar(&values.workspace, "workspace", "", "caller workspace (defaults to current directory)")
	flags.StringVar(&values.resultsDir, "results-dir", ".sliver-bof-e2e/results", "result directory")
	flags.StringVar(&values.githubOutput, "github-output", "", "GitHub Actions output command file")
	flags.StringVar(&values.actionVersion, "action-version", "", "immutable action/workflow identity recorded in reports")
	if includeVersion {
		flags.StringVar(&values.sliverVersion, "sliver-version", "", "Sliver selector override")
	}
	return values
}

func (values *commonFlags) normalize() error {
	var err error
	if strings.TrimSpace(values.workspace) == "" {
		values.workspace, err = os.Getwd()
	} else {
		values.workspace, err = filepath.Abs(values.workspace)
	}
	if err != nil {
		return fmt.Errorf("resolve workspace: %w", err)
	}
	values.workspace, err = filepath.EvalSymlinks(values.workspace)
	if err != nil {
		return fmt.Errorf("resolve workspace symlinks: %w", err)
	}
	if !filepath.IsAbs(values.manifestPath) {
		values.manifestPath = filepath.Join(values.workspace, values.manifestPath)
	}
	if !filepath.IsAbs(values.resultsDir) {
		values.resultsDir = filepath.Join(values.workspace, values.resultsDir)
	}
	values.manifestPath = filepath.Clean(values.manifestPath)
	values.resultsDir = filepath.Clean(values.resultsDir)
	values.actionVersion, err = resolveActionVersion(values.actionVersion)
	if err != nil {
		return err
	}
	return nil
}

func runPlan(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	common := addCommonFlags(flags, true)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("plan: unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if err := common.normalize(); err != nil {
		return err
	}
	loaded, err := manifest.Load(common.manifestPath, common.workspace)
	if err != nil {
		return err
	}
	selector := selectedSliverVersion(common.sliverVersion, loaded.Config.Sliver.Version)
	resolution, err := acquire.Resolve(ctx, selector, os.Getenv("GITHUB_TOKEN"))
	if err != nil {
		return err
	}
	matrix := loaded.Matrix()
	if len(matrix.Include) == 0 {
		return errors.New("manifest produced an empty target matrix")
	}
	document := planDocument{Schema: planSchema, Manifest: loaded.Path, Matrix: matrix, Resolution: resolution}
	if err := writeJSON(filepath.Join(common.resultsDir, "plan.json"), document); err != nil {
		return fmt.Errorf("write plan: %w", err)
	}
	matrixJSON, err := json.Marshal(matrix)
	if err != nil {
		return err
	}
	if err := appendOutputs(common.githubOutput, map[string]string{
		"matrix":          string(matrixJSON),
		"resolved-sliver": resolution.Selector,
		"results-dir":     common.resultsDir,
	}); err != nil {
		return err
	}
	fmt.Printf("Planned %d target(s) with Sliver %s\n", len(matrix.Include), resolution.Selector)
	return nil
}

func runPrepare(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("prepare", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	common := addCommonFlags(flags, true)
	var targetKey, serverPath, preparedPath string
	flags.StringVar(&targetKey, "target", "", "target os/arch")
	flags.StringVar(&serverPath, "server-path", "", "existing sliver-server binary")
	flags.StringVar(&preparedPath, "prepared", "", "private prepared descriptor to create")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("prepare: unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if strings.TrimSpace(preparedPath) == "" {
		return errors.New("prepare: --prepared is required")
	}
	if err := common.normalize(); err != nil {
		return err
	}
	loaded, err := manifest.Load(common.manifestPath, common.workspace)
	if err != nil {
		return err
	}
	targetKey, err = chooseTarget(targetKey, loaded.Matrix())
	if err != nil {
		return err
	}
	plans := loaded.PlansForTarget(targetKey)
	if len(plans) == 0 {
		return fmt.Errorf("manifest has no plans for target %q", targetKey)
	}
	target, err := parseTarget(targetKey)
	if err != nil {
		return err
	}
	selector := selectedSliverVersion(common.sliverVersion, loaded.Config.Sliver.Version)
	if serverPath != "" && !filepath.IsAbs(serverPath) {
		serverPath = filepath.Join(common.workspace, serverPath)
	}
	preparedPath, err = auxiliaryPath(preparedPath, common.workspace)
	if err != nil {
		return err
	}
	cacheDir, err := toolCacheDir()
	if err != nil {
		return writeInfrastructureFailure(common, targetKey, selector, acquire.Provenance{}, err)
	}
	logger := stderrLogger{logger: log.New(os.Stderr, "", log.LstdFlags)}
	server, provenance, err := acquire.Server(ctx, acquire.Options{
		Version: selector, ServerPath: serverPath, CacheDir: cacheDir,
		Token: os.Getenv("GITHUB_TOKEN"), Logger: logger,
	})
	if err != nil {
		return writeInfrastructureFailure(common, targetKey, selector, acquire.Provenance{}, err)
	}
	document := preparedDocument{
		Schema: preparedSchema, Target: targetKey, Selector: selector,
		ServerPath: server, Sliver: provenance,
	}
	if plansRequireCOFFLoader(plans) {
		loader, loaderErr := acquire.COFFLoader(ctx, acquire.Target{OS: target.OS, Arch: target.Arch}, acquire.COFFLoaderOptions{
			CacheDir: cacheDir, Token: os.Getenv("GITHUB_TOKEN"), Logger: logger,
		})
		if loaderErr != nil {
			return writeInfrastructureFailure(common, targetKey, selector, provenance, loaderErr)
		}
		document.Loader = &preparedLoader{
			Data: append([]byte(nil), loader.Data...), Init: loader.Init, Export: loader.Export,
			Digest: loader.Digest, Provenance: loader.Provenance,
		}
	}
	if err := writePreparedDescriptor(preparedPath, document); err != nil {
		return writeInfrastructureFailure(common, targetKey, selector, provenance, fmt.Errorf("write prepared descriptor: %w", err))
	}
	resolved := resolvedSelector(provenance)
	if err := appendOutputs(common.githubOutput, map[string]string{
		"resolved-sliver": resolved,
		"results-dir":     common.resultsDir,
		"target":          targetKey,
	}); err != nil {
		return err
	}
	fmt.Printf("Prepared Sliver %s for %s\n", resolved, targetKey)
	return nil
}

func runTarget(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	common := addCommonFlags(flags, true)
	var targetKey, stateFile, preparedPath string
	flags.StringVar(&targetKey, "target", "", "target os/arch")
	flags.StringVar(&stateFile, "state-file", "", "cleanup state file")
	flags.StringVar(&preparedPath, "prepared", "", "private descriptor created by prepare")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("run: unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if strings.TrimSpace(preparedPath) == "" {
		return errors.New("run: --prepared is required; acquire Sliver with prepare first")
	}
	if err := common.normalize(); err != nil {
		return err
	}
	// Execution never performs network acquisition. Remove an ambient token
	// before parsing caller-controlled inputs or launching any Sliver process,
	// even for direct CLI users outside the composite action.
	_ = os.Unsetenv("GITHUB_TOKEN")
	loaded, err := manifest.Load(common.manifestPath, common.workspace)
	if err != nil {
		return err
	}
	targetKey, err = chooseTarget(targetKey, loaded.Matrix())
	if err != nil {
		return err
	}
	plans := loaded.PlansForTarget(targetKey)
	if len(plans) == 0 {
		return fmt.Errorf("manifest has no plans for target %q", targetKey)
	}
	target, err := parseTarget(targetKey)
	if err != nil {
		return err
	}
	selector := selectedSliverVersion(common.sliverVersion, loaded.Config.Sliver.Version)
	cases, err := convertPlans(common.workspace, plans)
	if err != nil {
		return writeInfrastructureFailure(common, targetKey, selector, acquire.Provenance{}, err)
	}
	if stateFile == "" {
		stateFile = filepath.Join(common.resultsDir, "cleanup-state.json")
	} else if !filepath.IsAbs(stateFile) {
		stateFile = filepath.Join(common.workspace, stateFile)
	}
	preparedPath, err = auxiliaryPath(preparedPath, common.workspace)
	if err != nil {
		return writeInfrastructureFailure(common, targetKey, selector, acquire.Provenance{}, err)
	}
	prepared, preparedErr := readPreparedDescriptor(preparedPath, targetKey, selector, requiresCOFFLoader(cases))
	if preparedErr != nil {
		return writeInfrastructureFailure(common, targetKey, selector, acquire.Provenance{}, fmt.Errorf("read prepared descriptor: %w", preparedErr))
	}
	server, provenance := prepared.ServerPath, prepared.Sliver
	var loader *harness.Loader
	if prepared.Loader != nil {
		loader = &harness.Loader{
			Data: append([]byte(nil), prepared.Loader.Data...), Init: prepared.Loader.Init,
			Export: prepared.Loader.Export, Digest: prepared.Loader.Digest,
		}
	}
	report, runErr := harness.Run(ctx, harness.Options{
		Workspace: common.workspace, ServerPath: server, Target: target, Cases: cases,
		ResultsDir: common.resultsDir, StateFile: stateFile, Loader: loader,
		Sliver: toResultProvenance(provenance), ActionVersion: common.actionVersion,
	})
	if report == nil {
		return errors.Join(runErr, errors.New("harness returned no report"))
	}
	writeErr := report.Write(common.resultsDir)
	summaryErr := appendSummary(report.Markdown())
	resolved := resolvedSelector(provenance)
	outputErr := appendOutputs(common.githubOutput, map[string]string{
		"resolved-sliver": resolved,
		"results-dir":     common.resultsDir,
	})
	return errors.Join(runErr, writeErr, summaryErr, outputErr)
}

func writeInfrastructureFailure(common *commonFlags, target, selector string, provenance acquire.Provenance, cause error) error {
	report := results.New(target)
	report.ActionVersion = common.actionVersion
	report.InfrastructureError = cause.Error()
	if provenance.Selector == "" {
		provenance.Selector = selector
	}
	report.Sliver = toResultProvenance(provenance)
	writeErr := report.Write(common.resultsDir)
	summaryErr := appendSummary(report.Markdown())
	resolved := resolvedSelector(provenance)
	if resolved == "" {
		resolved = selector
	}
	outputErr := appendOutputs(common.githubOutput, map[string]string{
		"resolved-sliver": resolved,
		"results-dir":     common.resultsDir,
	})
	return errors.Join(cause, writeErr, summaryErr, outputErr)
}

func runAggregate(args []string) error {
	flags := flag.NewFlagSet("aggregate", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	common := addCommonFlags(flags, false)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("aggregate: unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if err := common.normalize(); err != nil {
		return err
	}
	loaded, err := manifest.Load(common.manifestPath, common.workspace)
	if err != nil {
		return err
	}
	reports, summary, aggregateErr := results.Aggregate(common.resultsDir)
	coverageErr := verifyTargetCoverage(loaded.Matrix(), reports)
	caseCoverageErr := verifyCaseCoverage(loaded.Plans, reports)
	provenanceErr := verifyReportProvenance(reports)
	if coverageErr != nil {
		summary += "\nMatrix completeness: **FAIL** — " + coverageErr.Error() + "\n"
	} else {
		summary += "\nMatrix completeness: **PASS**\n"
	}
	if caseCoverageErr != nil {
		summary += "\nCase completeness: **FAIL** — " + caseCoverageErr.Error() + "\n"
	} else {
		summary += "\nCase completeness: **PASS**\n"
	}
	if provenanceErr != nil {
		summary += "\nProvenance consistency: **FAIL** — " + provenanceErr.Error() + "\n"
	} else {
		summary += "\nProvenance consistency: **PASS**\n"
	}
	writeJSONErr := writeJSON(filepath.Join(common.resultsDir, "aggregate.json"), aggregateDocument{
		Schema: "sliver-bof-e2e-aggregate/v1", Reports: reports,
	})
	writeSummaryErr := os.WriteFile(filepath.Join(common.resultsDir, "aggregate.md"), []byte(summary), 0o600)
	stepSummaryErr := appendSummary(summary)
	outputErr := appendOutputs(common.githubOutput, map[string]string{"results-dir": common.resultsDir})
	return errors.Join(aggregateErr, coverageErr, caseCoverageErr, provenanceErr, writeJSONErr, writeSummaryErr, stepSummaryErr, outputErr)
}

func selectedSliverVersion(override, configured string) string {
	if strings.TrimSpace(override) != "" {
		return strings.TrimSpace(override)
	}
	return strings.TrimSpace(configured)
}

func chooseTarget(requested string, matrix manifest.Matrix) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		if len(matrix.Include) == 1 {
			return matrix.Include[0].Target, nil
		}
		return "", fmt.Errorf("--target is required when the manifest contains %d targets", len(matrix.Include))
	}
	for _, entry := range matrix.Include {
		if entry.Target == requested {
			return requested, nil
		}
	}
	return "", fmt.Errorf("target %q is not present in the manifest", requested)
}

func parseTarget(value string) (harness.Target, error) {
	osName, architecture, ok := strings.Cut(value, "/")
	if !ok || osName == "" || architecture == "" || strings.Contains(architecture, "/") {
		return harness.Target{}, fmt.Errorf("invalid target %q; expected os/arch", value)
	}
	return harness.Target{OS: osName, Arch: architecture}, nil
}

func convertPlans(workspace string, plans []manifest.Plan) ([]harness.Case, error) {
	cases := make([]harness.Case, 0, len(plans))
	for _, plan := range plans {
		object, err := os.ReadFile(plan.Object)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", plan.ID, err)
		}
		digest := sha256.Sum256(object)
		arguments := make([]harness.Argument, 0, len(plan.Settings.Args))
		for index, argument := range plan.Settings.Args {
			converted := harness.Argument{Type: argument.Type, String: argument.String}
			switch argument.Type {
			case "int":
				converted.Uint64 = uint64(argument.Integer)
			case "short":
				converted.Uint64 = uint64(argument.Short)
			case "file":
				converted.Data, err = os.ReadFile(argument.File)
				if err != nil {
					return nil, fmt.Errorf("read %s argument %d: %w", plan.ID, index+1, err)
				}
			case "bytes_base64":
				converted.Data = append([]byte(nil), argument.Bytes...)
			}
			arguments = append(arguments, converted)
		}
		relativeObject, relErr := filepath.Rel(workspace, plan.Object)
		if relErr != nil {
			relativeObject = plan.Object
		}
		expected := harness.OutputExpectation{
			Equals: plan.Settings.Expect.Output.Equals, Contains: append([]string(nil), plan.Settings.Expect.Output.Contains...),
			NotContains: append([]string(nil), plan.Settings.Expect.Output.NotContains...),
			Matches:     append([]string(nil), plan.Settings.Expect.Output.Matches...), SHA256: plan.Settings.Expect.Output.SHA256,
		}
		if plan.Settings.Expect.Output.HasEqualsB64 {
			expected.EqualsBase64 = base64.StdEncoding.EncodeToString(plan.Settings.Expect.Output.EqualsBytes)
			expected.HasEqualsBase64 = true
		}
		cases = append(cases, harness.Case{
			ID: plan.ID, Suite: plan.Suite, Name: plan.Name, Collection: plan.Collection,
			ObjectPath: plan.Object, ObjectReportPath: filepath.ToSlash(relativeObject),
			ObjectData: object, ObjectSHA256: hex.EncodeToString(digest[:]), Entrypoint: plan.Settings.Entrypoint,
			Executor: plan.Settings.BOFExecutor, Modes: append([]string(nil), plan.Settings.Modes...),
			Repeat: plan.Settings.Repeat, CommandTimeout: plan.Settings.CommandTimeout,
			Arguments: arguments, RawArguments: cloneOptionalBytes(plan.Settings.RawArgs, plan.Settings.HasRawArgs),
			HasRawArguments: plan.Settings.HasRawArgs,
			Expectation:     harness.Expectation{SuccessOnly: plan.Settings.Expect.SuccessOnly, Output: expected},
		})
	}
	return cases, nil
}

func cloneOptionalBytes(value []byte, present bool) []byte {
	if !present {
		return nil
	}
	return append([]byte{}, value...)
}

func requiresCOFFLoader(cases []harness.Case) bool {
	for _, testCase := range cases {
		if testCase.Executor == harness.ExecutorCOFFLoader {
			return true
		}
	}
	return false
}

func plansRequireCOFFLoader(plans []manifest.Plan) bool {
	for _, plan := range plans {
		if plan.Settings.BOFExecutor == harness.ExecutorCOFFLoader {
			return true
		}
	}
	return false
}

func auxiliaryPath(value, workspace string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", errors.New("path is required")
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(workspace, value)
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	return filepath.Clean(absolute), nil
}

func writePreparedDescriptor(path string, document preparedDocument) error {
	if document.Schema != preparedSchema {
		return fmt.Errorf("prepared descriptor schema must be %q", preparedSchema)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return errors.New("prepared descriptor already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > maxPreparedDescriptor {
		return fmt.Errorf("prepared descriptor exceeds %d bytes", maxPreparedDescriptor)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".prepared-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	ok := false
	defer func() {
		_ = temporary.Close()
		if !ok {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	ok = true
	return nil
}

func readPreparedDescriptor(path, target, selector string, needLoader bool) (preparedDocument, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return preparedDocument{}, err
	}
	if !before.Mode().IsRegular() || before.Size() <= 0 || before.Size() > maxPreparedDescriptor {
		return preparedDocument{}, errors.New("prepared descriptor is not a bounded regular file")
	}
	if runtime.GOOS != "windows" && before.Mode().Perm()&0o077 != 0 {
		return preparedDocument{}, errors.New("prepared descriptor must not be accessible by group or other users")
	}
	file, err := os.Open(path)
	if err != nil {
		return preparedDocument{}, err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil {
		return preparedDocument{}, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) || after.Size() != before.Size() {
		return preparedDocument{}, errors.New("prepared descriptor changed while opening")
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxPreparedDescriptor+1))
	decoder.DisallowUnknownFields()
	document := preparedDocument{}
	if err := decoder.Decode(&document); err != nil {
		return preparedDocument{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return preparedDocument{}, errors.New("prepared descriptor contains trailing JSON data")
	}
	if document.Schema != preparedSchema {
		return preparedDocument{}, fmt.Errorf("unsupported prepared descriptor schema %q", document.Schema)
	}
	if document.Target != target {
		return preparedDocument{}, fmt.Errorf("prepared target %q does not match requested target %q", document.Target, target)
	}
	if document.Selector != selector {
		return preparedDocument{}, fmt.Errorf("prepared selector %q does not match requested selector %q", document.Selector, selector)
	}
	if !filepath.IsAbs(document.ServerPath) {
		return preparedDocument{}, errors.New("prepared server path is not absolute")
	}
	if err := verifyPreparedFile(document.ServerPath, document.Sliver.Digest); err != nil {
		return preparedDocument{}, fmt.Errorf("verify prepared Sliver server: %w", err)
	}
	if needLoader != (document.Loader != nil) {
		if needLoader {
			return preparedDocument{}, errors.New("prepared descriptor is missing required COFFLoader material")
		}
		return preparedDocument{}, errors.New("prepared descriptor contains unexpected COFFLoader material")
	}
	if document.Loader != nil {
		if len(document.Loader.Data) == 0 || len(document.Loader.Data) > maxPreparedLoaderBytes {
			return preparedDocument{}, errors.New("prepared COFFLoader material is outside size limits")
		}
		actual := sha256.Sum256(document.Loader.Data)
		actualDigest := hex.EncodeToString(actual[:])
		if document.Loader.Digest == "" || actualDigest != strings.ToLower(document.Loader.Digest) {
			return preparedDocument{}, errors.New("prepared COFFLoader digest does not match its material")
		}
		if document.Loader.Provenance.Digest != document.Loader.Digest {
			return preparedDocument{}, errors.New("prepared COFFLoader provenance digest does not match its material")
		}
		if strings.TrimSpace(document.Loader.Export) == "" || strings.ContainsAny(document.Loader.Export, " \t\r\n\x00") {
			return preparedDocument{}, errors.New("prepared COFFLoader export is invalid")
		}
		if strings.ContainsAny(document.Loader.Init, "\r\n\x00") {
			return preparedDocument{}, errors.New("prepared COFFLoader init is invalid")
		}
	}
	return document, nil
}

func verifyPreparedFile(path, wantDigest string) error {
	if strings.TrimSpace(wantDigest) == "" {
		return errors.New("prepared provenance has no SHA-256")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("prepared server is not a regular file")
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return err
	}
	actual := hex.EncodeToString(digest.Sum(nil))
	if actual != strings.ToLower(wantDigest) {
		return fmt.Errorf("SHA-256 %s does not match prepared provenance %s", actual, wantDigest)
	}
	return nil
}

func toolCacheDir() (string, error) {
	// RUNNER_TOOL_CACHE can be shared by unrelated jobs on a self-hosted runner.
	// Keep executable cache entries inside the current job's private temp root.
	if root := strings.TrimSpace(os.Getenv("RUNNER_TEMP")); root != "" {
		return filepath.Join(root, "sliver-bof-e2e-cache"), nil
	}
	root, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache: %w", err)
	}
	return filepath.Join(root, "sliver-bof-e2e"), nil
}

func toResultProvenance(value acquire.Provenance) results.Provenance {
	resolved := ""
	if value.ResolvedTag != "" {
		resolved = value.ResolvedTag
	} else if value.ResolvedSHA != "" {
		resolved = value.ResolvedSHA
	}
	return results.Provenance{
		Selector: value.Selector, Resolved: resolved, ResolvedTag: value.ResolvedTag,
		ResolvedSHA: value.ResolvedSHA, SHA256: value.Digest, SourceBuilt: value.SourceBuilt,
		CacheHit: value.CacheHit, Verified: value.Verified, Source: value.Source, Toolchain: value.Toolchain,
	}
}

func resolvedSelector(value acquire.Provenance) string {
	if value.Selector != "" && value.Selector != "path" {
		return value.Selector
	}
	if value.ResolvedSHA != "" {
		return value.ResolvedSHA
	}
	return value.ResolvedTag
}

func resolveActionVersion(override string) (string, error) {
	value := strings.TrimSpace(override)
	if value == "" {
		value = strings.TrimSpace(os.Getenv("SLIVER_BOF_E2E_ACTION_VERSION"))
	}
	if value == "" {
		// The Linux/386 container action uses this shorter environment name at
		// its explicit container boundary.
		value = strings.TrimSpace(os.Getenv("SLIVER_BOF_ACTION_VERSION"))
	}
	repository := strings.TrimSpace(os.Getenv("GITHUB_ACTION_REPOSITORY"))
	reference := strings.TrimSpace(os.Getenv("GITHUB_ACTION_REF"))
	if value == "" && repository != "" && reference != "" {
		value = repository + "@" + reference
	}
	if value == "" && repository != "" {
		value = repository
	}
	if value == "" {
		value = "development"
	}
	if len(value) > 512 || strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("action version must be a single line of at most 512 bytes")
	}
	return value, nil
}

func verifyTargetCoverage(matrix manifest.Matrix, reports []results.Report) error {
	expected := make(map[string]struct{}, len(matrix.Include))
	for _, entry := range matrix.Include {
		expected[entry.Target] = struct{}{}
	}
	seen := make(map[string]struct{}, len(reports))
	var problems []string
	for _, report := range reports {
		if _, duplicate := seen[report.Target]; duplicate {
			problems = append(problems, "duplicate report for "+report.Target)
			continue
		}
		seen[report.Target] = struct{}{}
		if _, wanted := expected[report.Target]; !wanted {
			problems = append(problems, "unexpected report for "+report.Target)
		}
	}
	for target := range expected {
		if _, ok := seen[target]; !ok {
			problems = append(problems, "missing report for "+target)
		}
	}
	sort.Strings(problems)
	if len(problems) != 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

type executionKey struct {
	target string
	id     string
	mode   string
	repeat int
}

type expectedExecution struct {
	suite        string
	test         string
	executor     string
	objectSHA256 string
}

func verifyCaseCoverage(plans []manifest.Plan, reports []results.Report) error {
	expected := make(map[executionKey]expectedExecution)
	validModes := make(map[executionKey]struct{})
	var problems []string
	for _, plan := range plans {
		object, err := os.ReadFile(plan.Object)
		if err != nil {
			problems = append(problems, fmt.Sprintf("read planned object %s: %v", plan.ID, err))
			continue
		}
		digest := sha256.Sum256(object)
		metadata := expectedExecution{
			suite: plan.Suite, test: plan.Name, executor: plan.Settings.BOFExecutor,
			objectSHA256: hex.EncodeToString(digest[:]),
		}
		for _, mode := range plan.Settings.Modes {
			validModes[executionKey{target: plan.Target.Key(), id: plan.ID, mode: mode}] = struct{}{}
			for repeat := 1; repeat <= plan.Settings.Repeat; repeat++ {
				expected[executionKey{target: plan.Target.Key(), id: plan.ID, mode: mode, repeat: repeat}] = metadata
			}
		}
	}
	seen := make(map[executionKey]struct{})
	for _, report := range reports {
		for _, result := range report.Cases {
			if result.Repeat == 0 && result.Stage == "cleanup-implant" && result.Status != "pass" {
				if _, ok := validModes[executionKey{target: report.Target, id: result.ID, mode: result.Mode}]; !ok {
					problems = append(problems, fmt.Sprintf("unexpected cleanup result %s/%s", report.Target, result.ID))
				}
				continue
			}
			key := executionKey{target: report.Target, id: result.ID, mode: result.Mode, repeat: result.Repeat}
			metadata, ok := expected[key]
			if !ok {
				problems = append(problems, fmt.Sprintf("unexpected case %s/%s/%s#%d", report.Target, result.ID, result.Mode, result.Repeat))
				continue
			}
			if _, duplicate := seen[key]; duplicate {
				problems = append(problems, fmt.Sprintf("duplicate case %s/%s/%s#%d", report.Target, result.ID, result.Mode, result.Repeat))
				continue
			}
			seen[key] = struct{}{}
			if result.Suite != metadata.suite || result.Test != metadata.test || result.Executor != metadata.executor || result.ObjectSHA256 != metadata.objectSHA256 {
				problems = append(problems, fmt.Sprintf("metadata mismatch for %s/%s/%s#%d", report.Target, result.ID, result.Mode, result.Repeat))
			}
		}
	}
	for key := range expected {
		if _, ok := seen[key]; !ok {
			problems = append(problems, fmt.Sprintf("missing case %s/%s/%s#%d", key.target, key.id, key.mode, key.repeat))
		}
	}
	sort.Strings(problems)
	if len(problems) != 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func verifyReportProvenance(reports []results.Report) error {
	if len(reports) == 0 {
		return errors.New("no target provenance to compare")
	}
	wantSHA := reports[0].Sliver.ResolvedSHA
	wantSelector := reports[0].Sliver.Selector
	wantAction := reports[0].ActionVersion
	var problems []string
	for _, report := range reports {
		if report.Sliver.ResolvedSHA == "" {
			problems = append(problems, "missing resolved Sliver SHA for "+report.Target)
		} else if wantSHA != "" && report.Sliver.ResolvedSHA != wantSHA {
			problems = append(problems, "Sliver SHA mismatch for "+report.Target)
		}
		if report.Sliver.Selector == "" {
			problems = append(problems, "missing Sliver selector for "+report.Target)
		} else if wantSelector != "" && report.Sliver.Selector != wantSelector {
			problems = append(problems, "Sliver selector mismatch for "+report.Target)
		}
		if report.ActionVersion == "" {
			problems = append(problems, "missing action version for "+report.Target)
		} else if wantAction != "" && report.ActionVersion != wantAction {
			problems = append(problems, "action version mismatch for "+report.Target)
		}
	}
	sort.Strings(problems)
	if len(problems) != 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func appendOutputs(path string, values map[string]string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open GitHub output file: %w", err)
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := values[key]
		if strings.ContainsAny(key, "=\r\n") || strings.ContainsAny(value, "\r\n") {
			_ = file.Close()
			return fmt.Errorf("GitHub output %q contains a newline", key)
		}
		if _, err := fmt.Fprintf(file, "%s=%s\n", key, value); err != nil {
			_ = file.Close()
			return err
		}
	}
	return file.Close()
}

func appendSummary(summary string) error {
	path := strings.TrimSpace(os.Getenv("GITHUB_STEP_SUMMARY"))
	if path == "" {
		return nil
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if !strings.HasSuffix(summary, "\n") {
		summary += "\n"
	}
	_, writeErr := file.WriteString(summary)
	return errors.Join(writeErr, file.Close())
}
