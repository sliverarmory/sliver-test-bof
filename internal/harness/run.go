package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	clientassets "github.com/bishopfox/sliver/client/assets"
	consts "github.com/bishopfox/sliver/client/constants"
	clienttransport "github.com/bishopfox/sliver/client/transport"
	"github.com/bishopfox/sliver/protobuf/clientpb"
	"github.com/bishopfox/sliver/protobuf/commonpb"
	"github.com/bishopfox/sliver/protobuf/rpcpb"
	"github.com/bishopfox/sliver/protobuf/sliverpb"
	"github.com/sliverarmory/sliver-test-bof/internal/results"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

const (
	operatorName        = "sliver-bof-e2e"
	listenerPoll        = 250 * time.Millisecond
	listenerDialTimeout = 500 * time.Millisecond
	eventSyncTimeout    = 30 * time.Second
	listenerHealthTime  = 3 * time.Second
	taskDiagnosticTime  = 5 * time.Second
)

type generatedImplant struct {
	mode string
	name string
	path string
}

type connectedTarget struct {
	session *clientpb.Session
	beacon  *clientpb.Beacon
}

func (target connectedTarget) ID() string {
	if target.session != nil {
		return target.session.ID
	}
	if target.beacon != nil {
		return target.beacon.ID
	}
	return ""
}

func (target connectedTarget) Capabilities() uint64 {
	if target.session != nil {
		return target.session.Capabilities
	}
	if target.beacon != nil {
		return target.beacon.Capabilities
	}
	return 0
}

func (target connectedTarget) request(timeout time.Duration) *commonpb.Request {
	request := &commonpb.Request{Timeout: int64(timeout)}
	if target.session != nil {
		request.SessionID = target.session.ID
	} else if target.beacon != nil {
		request.Async = true
		request.BeaconID = target.beacon.ID
	}
	return request
}

type runtimeHarness struct {
	ctx       context.Context
	opts      Options
	report    *results.Report
	tracker   *stateTracker
	workDir   string
	serverEnv []string
	serverLog string
	server    *managedProcess
	rpc       rpcpb.SliverRPCClient
	conn      *grpc.ClientConn
	cancel    context.CancelFunc
	hub       *eventHub
	listener  *clientpb.ListenerJob
	httpPort  int
}

func Run(ctx context.Context, opts Options) (report *results.Report, runErr error) {
	setOptionDefaults(&opts)
	report = results.New(opts.Target.String())
	report.Sliver = opts.Sliver
	report.ActionVersion = opts.ActionVersion
	if err := validateRunOptions(opts); err != nil {
		report.InfrastructureError = err.Error()
		return report, err
	}
	ctx, cancel := context.WithTimeout(ctx, opts.OverallTimeout)
	defer cancel()
	runner := &runtimeHarness{ctx: ctx, opts: opts, report: report, tracker: newStateTracker(opts.StateFile)}
	defer func() {
		if cleanupErr := runner.close(); cleanupErr != nil {
			runErr = errors.Join(runErr, cleanupErr)
			if report.InfrastructureError == "" {
				report.InfrastructureError = "cleanup: " + cleanupErr.Error()
			} else {
				report.InfrastructureError = errors.Join(errors.New(report.InfrastructureError), fmt.Errorf("cleanup: %w", cleanupErr)).Error()
			}
		}
		if runErr != nil && report.InfrastructureError == "" && len(report.Cases) == 0 {
			report.InfrastructureError = runErr.Error()
		}
	}()
	if err := runner.setup(); err != nil {
		report.InfrastructureError = err.Error()
		return report, err
	}
	implants := map[string]generatedImplant{}
	for _, mode := range requestedModes(opts.Cases) {
		implant, err := runner.generate(mode)
		if err != nil {
			report.InfrastructureError = fmt.Sprintf("generate %s implant: %v", mode, err)
			return report, err
		}
		implants[mode] = implant
	}
	for _, testCase := range opts.Cases {
		if err := runner.ctx.Err(); err != nil {
			report.InfrastructureError = "run context ended: " + err.Error()
			return report, err
		}
		for _, mode := range testCase.Modes {
			if err := runner.ctx.Err(); err != nil {
				report.InfrastructureError = "run context ended: " + err.Error()
				return report, err
			}
			// Capture the event cursor before probing listener health. A listener
			// that stopped before the cursor fails the TCP probe; one that stops
			// after it is visible to waitForImplant.
			cursor := runner.hub.cursor()
			if err := runner.ensureHTTPListenerHealthy(); err != nil {
				if contextErr := runner.ctx.Err(); contextErr != nil {
					report.InfrastructureError = "run context ended: " + contextErr.Error()
					return report, contextErr
				}
				report.InfrastructureError = "HTTP listener health check: " + err.Error()
				return report, err
			}
			runner.runCase(testCase, implants[mode], cursor)
			if err := runner.ctx.Err(); err != nil {
				report.InfrastructureError = "run context ended: " + err.Error()
				return report, err
			}
		}
	}
	if report.Failed() {
		return report, errors.New("one or more BOF E2E cases failed")
	}
	return report, nil
}

func setOptionDefaults(opts *Options) {
	if opts.OverallTimeout <= 0 {
		opts.OverallTimeout = 4 * time.Hour
	}
	if opts.StartupTimeout <= 0 {
		opts.StartupTimeout = 10 * time.Minute
	}
	if opts.ConnectTimeout <= 0 {
		opts.ConnectTimeout = 5 * time.Minute
	}
	if opts.BeaconInterval <= 0 {
		opts.BeaconInterval = 10 * time.Second
	}
	if opts.Logf == nil {
		opts.Logf = func(format string, args ...any) { fmt.Printf(format+"\n", args...) }
	}
}

func validateRunOptions(opts Options) error {
	if opts.Workspace == "" || opts.ServerPath == "" || opts.ResultsDir == "" {
		return errors.New("workspace, server path, and results directory are required")
	}
	if opts.Target.OS == "" || opts.Target.Arch == "" {
		return errors.New("target OS and architecture are required")
	}
	if len(opts.Cases) == 0 {
		return errors.New("target has no BOF cases")
	}
	if stat, err := os.Stat(opts.ServerPath); err != nil {
		return fmt.Errorf("stat Sliver server: %w", err)
	} else if stat.IsDir() {
		return errors.New("Sliver server path is a directory")
	}
	return nil
}

func (runner *runtimeHarness) setup() error {
	tempRoot := strings.TrimSpace(os.Getenv("RUNNER_TEMP"))
	workDir, err := os.MkdirTemp(tempRoot, "sliver-bof-e2e-")
	if err != nil {
		return fmt.Errorf("create work directory: %w", err)
	}
	if err := runner.tracker.addTempDir(workDir); err != nil {
		removeErr := os.RemoveAll(workDir)
		return errors.Join(fmt.Errorf("persist work directory cleanup state: %w", err), removeErr)
	}
	runner.workDir = workDir
	serverRoot := filepath.Join(workDir, "server")
	clientRoot := filepath.Join(workDir, "client")
	home := filepath.Join(workDir, "home")
	temp := filepath.Join(workDir, "tmp")
	for _, path := range []string{serverRoot, clientRoot, home, temp, runner.opts.ResultsDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create isolated directory %s: %w", path, err)
		}
	}
	runner.serverEnv = sanitizedServerEnv(os.Environ(), home, temp, serverRoot, clientRoot)
	if runner.opts.Target.OS == "linux" && runner.opts.Target.Arch == "386" {
		const compiler = "/usr/bin/i686-linux-gnu-gcc"
		info, statErr := os.Stat(compiler)
		if statErr != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			return fmt.Errorf("linux/386 requires an executable native compiler at %s inside a 32-bit userspace", compiler)
		}
		// Do not inherit compiler overrides from a caller-controlled environment.
		// The Linux/386 workflow image installs and validates this exact native
		// Debian compiler to avoid Zig/LLD's malformed i386 TLS relocation.
		runner.serverEnv = envWith(runner.serverEnv, "SLIVER_LINUX_CC_32", compiler)
	}
	grpcPort, err := unusedTCPPort()
	if err != nil {
		return fmt.Errorf("select multiplayer port: %w", err)
	}
	runner.serverLog = filepath.Join(runner.opts.ResultsDir, "sliver-server.log")
	runner.server, err = startProcess(runner.opts.ServerPath,
		[]string{"daemon", "--lhost", "127.0.0.1", "--lport", fmt.Sprint(grpcPort), "--force"},
		runner.opts.Workspace, runner.serverEnv, runner.serverLog)
	if err != nil {
		return fmt.Errorf("start Sliver daemon: %w", err)
	}
	if err := runner.tracker.addPID(runner.server.PID()); err != nil {
		stopErr := runner.server.stop()
		if stopErr == nil {
			runner.server = nil
		}
		return errors.Join(
			fmt.Errorf("persist Sliver server cleanup state: %w", err),
			wrapError("stop untracked Sliver server", stopErr),
		)
	}
	runner.opts.Logf("Started Sliver daemon on 127.0.0.1:%d", grpcPort)
	startupCtx, cancel := context.WithTimeout(runner.ctx, runner.opts.StartupTimeout)
	err = waitForTCP(startupCtx, fmt.Sprintf("127.0.0.1:%d", grpcPort), runner.server)
	cancel()
	if err != nil {
		return fmt.Errorf("wait for Sliver daemon: %w", err)
	}
	profile := filepath.Join(workDir, "operator.cfg")
	profileCtx, profileCancel := context.WithTimeout(runner.ctx, 2*time.Minute)
	output, err := runCommand(profileCtx, runner.opts.Workspace, runner.serverEnv, runner.opts.ServerPath,
		"operator", "--name", operatorName, "--lhost", "127.0.0.1", "--lport", fmt.Sprint(grpcPort),
		"--permissions", "all", "--save", profile)
	profileCancel()
	if err != nil {
		return fmt.Errorf("generate operator profile: %w\n%s", err, output)
	}
	config, err := clientassets.ReadConfig(profile)
	if err != nil {
		return fmt.Errorf("read operator profile: %w", err)
	}
	if config.Operator != operatorName || config.LHost != "127.0.0.1" || config.LPort != grpcPort {
		return fmt.Errorf("operator profile mismatch: got %q at %s:%d", config.Operator, config.LHost, config.LPort)
	}
	runner.opts.Logf("Generated and validated localhost operator profile")
	runner.rpc, runner.conn, err = clienttransport.MTLSConnect(config)
	if err != nil {
		return fmt.Errorf("connect multiplayer gRPC: %w", err)
	}
	version, err := runner.rpc.GetVersion(runner.ctx, &commonpb.Empty{})
	if err != nil {
		return fmt.Errorf("query Sliver version: %w", err)
	}
	runner.opts.Logf("Connected custom Go client to Sliver %s/%s", version.OS, version.Arch)
	eventsCtx, eventsCancel := context.WithCancel(runner.ctx)
	runner.cancel = eventsCancel
	stream, err := runner.rpc.Events(eventsCtx, &commonpb.Empty{})
	if err != nil {
		return fmt.Errorf("subscribe to events: %w", err)
	}
	runner.hub = newEventHub(stream)
	return runner.startHTTPListener()
}

func (runner *runtimeHarness) startHTTPListener() error {
	port, err := unusedTCPPort()
	if err != nil {
		return err
	}
	job, err := runner.rpc.StartHTTPListener(runner.ctx, &clientpb.HTTPListenerReq{
		Domain: "localhost", Host: "127.0.0.1", Port: uint32(port), Secure: false,
		EnforceOTP: false, LongPollTimeout: int64(time.Second),
	})
	if err != nil {
		return fmt.Errorf("start HTTP listener: %w", err)
	}
	runner.listener = job
	runner.httpPort = port
	readyCtx, cancel := context.WithTimeout(runner.ctx, eventSyncTimeout)
	defer cancel()
	if err := waitForTCP(readyCtx, fmt.Sprintf("127.0.0.1:%d", port), runner.server); err != nil {
		return fmt.Errorf("wait for HTTP listener TCP readiness: %w", err)
	}
	runner.opts.Logf("Started HTTP listener on 127.0.0.1:%d (job %d)", port, job.JobID)
	return nil
}

func (runner *runtimeHarness) ensureHTTPListenerHealthy() error {
	if runner.listener == nil || runner.httpPort == 0 || runner.server == nil {
		return errors.New("HTTP listener is not initialized")
	}
	ctx, cancel := context.WithTimeout(runner.ctx, listenerHealthTime)
	defer cancel()
	address := fmt.Sprintf("127.0.0.1:%d", runner.httpPort)
	if err := waitForTCP(ctx, address, runner.server); err != nil {
		return fmt.Errorf("listener job %d is not accepting connections on %s: %w", runner.listener.JobID, address, err)
	}
	return nil
}

func (runner *runtimeHarness) generate(mode string) (generatedImplant, error) {
	name := safeName(fmt.Sprintf("bof-e2e-%s-%s-%s", mode, runner.opts.Target.OS, runner.opts.Target.Arch))
	beacon := mode == "beacon"
	generated, err := runner.rpc.Generate(runner.ctx, &clientpb.GenerateReq{
		Name: name,
		Config: &clientpb.ImplantConfig{
			GOOS: runner.opts.Target.OS, GOARCH: runner.opts.Target.Arch, TemplateName: "sliver",
			Debug: false, ObfuscateSymbols: false, IsBeacon: beacon,
			BeaconInterval: int64(runner.opts.BeaconInterval), BeaconJitter: 0,
			Format:      clientpb.OutputFormat_EXECUTABLE,
			C2:          []*clientpb.ImplantC2{{URL: fmt.Sprintf("http://127.0.0.1:%d?force-http=true&poll-timeout=1s", runner.httpPort)}},
			IncludeHTTP: true, HTTPC2ConfigName: consts.DefaultC2Profile, ConnectionStrategy: "s",
			ReconnectInterval: int64(time.Second), PollTimeout: int64(time.Second), MaxConnectionErrors: 20,
			NetGoEnabled: true,
		},
	})
	if err != nil {
		return generatedImplant{}, err
	}
	if generated.File == nil || generated.File.Name == "" || len(generated.File.Data) == 0 {
		return generatedImplant{}, errors.New("Sliver returned an empty implant")
	}
	path := filepath.Join(runner.workDir, "generated", mode, filepath.Base(generated.File.Name))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return generatedImplant{}, err
	}
	if err := os.WriteFile(path, generated.File.Data, 0o700); err != nil {
		return generatedImplant{}, err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return generatedImplant{}, err
	}
	implantName := generated.ImplantName
	if implantName == "" {
		implantName = name
	}
	runner.opts.Logf("Generated %s %s/%s implant %s", mode, runner.opts.Target.OS, runner.opts.Target.Arch, filepath.Base(path))
	return generatedImplant{mode: mode, name: implantName, path: path}, nil
}

func (runner *runtimeHarness) runCase(testCase Case, implant generatedImplant, cursor int) {
	artifactKey := caseArtifactKey(testCase)
	caseRoot := filepath.Join(runner.workDir, "cases", artifactKey+"-"+implant.mode)
	home := filepath.Join(caseRoot, "home")
	temp := filepath.Join(caseRoot, "tmp")
	for _, path := range []string{home, temp} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			runner.addFailure(testCase, implant.mode, 1, "prepare-implant", err, "")
			return
		}
	}
	fixtureVariables, err := prepareFixtures(filepath.Join(caseRoot, "fixtures"), runner.opts.Fixtures)
	if err != nil {
		runner.addFailure(testCase, implant.mode, 1, "prepare-fixtures", err, "")
		return
	}
	logPath := filepath.Join(runner.opts.ResultsDir, "logs", artifactKey+"-"+implant.mode+".log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		runner.addFailure(testCase, implant.mode, 1, "prepare-implant", err, "")
		return
	}
	marker := testCase.ID
	if marker == "" {
		marker = testCase.Suite + "/" + testCase.Name
	}
	process, err := startProcess(implant.path, nil, caseRoot,
		sanitizedImplantEnv(os.Environ(), home, temp, marker), logPath)
	if err != nil {
		runner.addFailure(testCase, implant.mode, 1, "start-implant", err, logPath)
		return
	}
	if err := runner.tracker.addPID(process.PID()); err != nil {
		stopErr := process.stop()
		runner.addFailure(testCase, implant.mode, 1, "track-implant", errors.Join(
			fmt.Errorf("persist implant cleanup state: %w", err),
			wrapError("stop untracked implant", stopErr),
		), logPath)
		return
	}
	connectCtx, cancel := context.WithTimeout(runner.ctx, runner.opts.ConnectTimeout)
	go func() {
		select {
		case <-process.done:
			cancel()
		case <-connectCtx.Done():
		}
	}()
	target, err := runner.waitForImplant(connectCtx, cursor, process, implant)
	cancel()
	if err != nil {
		runner.addFailure(testCase, implant.mode, 1, "connect-implant", err, logPath)
		runner.recordProcessCleanup(testCase, implant.mode, process, logPath)
		return
	}
	if testCase.Executor == ExecutorReflektor && target.Capabilities()&sliverpb.CapabilityBOFV1 == 0 {
		err := fmt.Errorf("connected %s does not advertise bof_v1; selected Sliver cannot execute Reflektor BOFs", implant.mode)
		runner.addFailure(testCase, implant.mode, 1, "capability-preflight", err, logPath)
		runner.recordProcessCleanup(testCase, implant.mode, process, logPath)
		return
	}
	arguments, err := substituteFixtureArguments(testCase.Arguments, fixtureVariables)
	if err != nil {
		runner.addFailure(testCase, implant.mode, 1, "prepare-fixtures", err, logPath)
		runner.recordProcessCleanup(testCase, implant.mode, process, logPath)
		return
	}
	var tcpFixture *loopbackTCPFixture
	if testCase.TCPListener != nil {
		tcpFixture, err = startLoopbackTCPFixture()
		if err != nil {
			runner.addFailure(testCase, implant.mode, 1, "prepare-tcp-listener", err, logPath)
			runner.recordProcessCleanup(testCase, implant.mode, process, logPath)
			return
		}
		defer func() {
			if err := tcpFixture.Close(); err != nil {
				runner.addFailure(testCase, implant.mode, 0, "cleanup-tcp-listener", err, logPath)
			}
		}()
		arguments, err = applyTCPListenerArgument(arguments, testCase.TCPListener, tcpFixture.Port())
		if err != nil {
			runner.addFailure(testCase, implant.mode, 1, "prepare-tcp-listener", err, logPath)
			runner.recordProcessCleanup(testCase, implant.mode, process, logPath)
			return
		}
	}
	rawArguments := testCase.RawArguments
	if testCase.HasRawArguments && rawArguments == nil {
		rawArguments = []byte{}
	}
	inner, err := PackArguments(arguments, rawArguments)
	if err != nil {
		runner.addFailure(testCase, implant.mode, 1, "pack-arguments", err, logPath)
		runner.recordProcessCleanup(testCase, implant.mode, process, logPath)
		return
	}
	if testCase.Executor == ExecutorCOFFLoader {
		if runner.opts.Loader == nil {
			runner.addFailure(testCase, implant.mode, 1, "acquire-coff-loader", errors.New("coff-loader executor selected without a loader"), logPath)
			runner.recordProcessCleanup(testCase, implant.mode, process, logPath)
			return
		}
		if err := runner.registerLoader(target, process, testCase.CommandTimeout); err != nil {
			runner.addFailure(testCase, implant.mode, 1, "register-coff-loader", err, logPath)
			runner.recordProcessCleanup(testCase, implant.mode, process, logPath)
			return
		}
	}
	for repeat := 1; repeat <= testCase.Repeat; repeat++ {
		if runner.ctx.Err() != nil {
			break
		}
		started := time.Now()
		output, err := runner.execute(target, process, testCase, inner)
		duration := time.Since(started)
		outputFile, outputErr := runner.writeOutput(testCase, implant.mode, repeat, output)
		if err == nil {
			err = AssertOutput(output, testCase.Expectation)
		}
		if outputErr != nil {
			err = errors.Join(err, fmt.Errorf("persist BOF output: %w", outputErr))
		}
		result := results.CaseResult{
			ID: testCase.ID, Suite: testCase.Suite, Test: testCase.Name, Collection: testCase.Collection,
			Object:       reportObjectPath(testCase),
			ObjectSHA256: testCase.ObjectSHA256, Mode: implant.mode, Executor: testCase.Executor,
			Repeat: repeat, Status: "pass", Duration: duration, OutputFile: outputFile,
		}
		if err != nil {
			result.Status = "fail"
			result.Stage = "execute-or-assert"
			result.Error = runner.diagnostics(err, logPath)
		}
		runner.report.Add(result)
		if !process.running() {
			if repeat < testCase.Repeat {
				runner.addFailure(testCase, implant.mode, repeat+1, "implant-exited-before-repeat", process.failure("implant exited before all repeats completed"), logPath)
			}
			break
		}
	}
	runner.recordProcessCleanup(testCase, implant.mode, process, logPath)
}

func (runner *runtimeHarness) recordProcessCleanup(testCase Case, mode string, process *managedProcess, logPath string) {
	if err := runner.stopTrackedProcess(process); err != nil {
		runner.addFailure(testCase, mode, 0, "cleanup-implant", err, logPath)
	}
}

func (runner *runtimeHarness) stopTrackedProcess(process *managedProcess) error {
	err := process.stop()
	if err == nil {
		if stateErr := runner.tracker.removePID(process.PID()); stateErr != nil {
			err = fmt.Errorf("remove process %d from cleanup state: %w", process.PID(), stateErr)
		}
	}
	if err != nil {
		runner.opts.Logf("Post cleanup will retry recorded process %d: %v", process.PID(), err)
	}
	return err
}

func (runner *runtimeHarness) waitForImplant(ctx context.Context, cursor int, process *managedProcess, implant generatedImplant) (connectedTarget, error) {
	expectedPID := int32(process.PID())
	for {
		event, next, err := runner.hub.wait(ctx, cursor, func(event *clientpb.Event) bool {
			if event.EventType == consts.JobStoppedEvent && event.Job != nil && runner.listener != nil && event.Job.ID == runner.listener.JobID {
				return true
			}
			if implant.mode == "session" {
				return event.EventType == consts.SessionOpenedEvent && event.Session != nil &&
					event.Session.Name == implant.name && event.Session.PID == expectedPID
			}
			if event.EventType != consts.BeaconRegisteredEvent {
				return false
			}
			beacon := &clientpb.Beacon{}
			return proto.Unmarshal(event.Data, beacon) == nil && beacon.Name == implant.name && beacon.PID == expectedPID
		})
		cursor = next
		if err != nil {
			if !process.running() {
				return connectedTarget{}, process.failure("implant exited before connecting")
			}
			return connectedTarget{}, err
		}
		if event.EventType == consts.JobStoppedEvent {
			return connectedTarget{}, fmt.Errorf("HTTP listener stopped: %s", event.Err)
		}
		if implant.mode == "session" {
			if err := validateTarget(event.Session.OS, event.Session.Arch, event.Session.Transport, event.Session.ActiveC2, runner.opts.Target); err != nil {
				return connectedTarget{}, err
			}
			return connectedTarget{session: event.Session}, nil
		}
		beacon := &clientpb.Beacon{}
		if err := proto.Unmarshal(event.Data, beacon); err != nil {
			return connectedTarget{}, err
		}
		if err := validateTarget(beacon.OS, beacon.Arch, beacon.Transport, beacon.ActiveC2, runner.opts.Target); err != nil {
			return connectedTarget{}, err
		}
		return connectedTarget{beacon: beacon}, nil
	}
}

func validateTarget(goos, goarch, transport, activeC2 string, expected Target) error {
	if goos != expected.OS || goarch != expected.Arch {
		return fmt.Errorf("connected target is %s/%s, want %s", goos, goarch, expected.String())
	}
	if !strings.EqualFold(strings.TrimSpace(transport), "http(s)") {
		return fmt.Errorf("connected transport is %q, want http(s)", transport)
	}
	if activeC2 != "" && !strings.HasPrefix(strings.ToLower(activeC2), "http://") {
		return fmt.Errorf("active C2 is %q, want http://", activeC2)
	}
	return nil
}

func (runner *runtimeHarness) execute(target connectedTarget, process *managedProcess, testCase Case, inner []byte) ([]byte, error) {
	object := testCase.ObjectData
	if len(object) == 0 {
		data, err := os.ReadFile(testCase.ObjectPath)
		if err != nil {
			return nil, err
		}
		object = data
	}
	digest := sha256.Sum256(object)
	request := &sliverpb.CallExtensionReq{
		Name: hex.EncodeToString(digest[:]), Export: testCase.Entrypoint, Args: inner,
	}
	if testCase.Executor == ExecutorReflektor {
		request.BOFData = object
		request.IsBOF = true
	} else {
		outer, err := PackCOFFLoaderArguments(testCase.Entrypoint, object, inner)
		if err != nil {
			return nil, err
		}
		request.Name = runner.opts.Loader.Digest
		request.Export = runner.opts.Loader.Export
		request.Args = outer
	}
	response := &sliverpb.CallExtension{}
	if err := runner.invoke(target, process, "CallExtension", testCase.CommandTimeout, response, func(ctx context.Context, common *commonpb.Request) (proto.Message, *commonpb.Response, error) {
		request.Request = common
		got, err := runner.rpc.CallExtension(ctx, request)
		if got == nil {
			return nil, nil, err
		}
		return got, got.Response, err
	}); err != nil {
		return response.Output, err
	}
	return response.Output, nil
}

func (runner *runtimeHarness) registerLoader(target connectedTarget, process *managedProcess, timeout time.Duration) error {
	loader := runner.opts.Loader
	register := &sliverpb.RegisterExtension{}
	err := runner.invoke(target, process, "RegisterExtension", timeout, register, func(ctx context.Context, request *commonpb.Request) (proto.Message, *commonpb.Response, error) {
		got, err := runner.rpc.RegisterExtension(ctx, &sliverpb.RegisterExtensionReq{
			Name: loader.Digest, Data: loader.Data, OS: runner.opts.Target.OS, Init: loader.Init, Request: request,
		})
		if got == nil {
			return nil, nil, err
		}
		return got, got.Response, err
	})
	if err != nil {
		return err
	}
	listed := &sliverpb.ListExtensions{}
	err = runner.invoke(target, process, "ListExtensions", timeout, listed, func(ctx context.Context, request *commonpb.Request) (proto.Message, *commonpb.Response, error) {
		got, err := runner.rpc.ListExtensions(ctx, &sliverpb.ListExtensionsReq{Request: request})
		if got == nil {
			return nil, nil, err
		}
		return got, got.Response, err
	})
	if err != nil {
		return err
	}
	for _, name := range listed.Names {
		if name == loader.Digest {
			return nil
		}
	}
	return fmt.Errorf("registered COFFLoader %s not returned by ListExtensions", loader.Digest)
}

type rpcCall func(context.Context, *commonpb.Request) (proto.Message, *commonpb.Response, error)

func (runner *runtimeHarness) invoke(target connectedTarget, process *managedProcess, operation string, timeout time.Duration, destination proto.Message, call rpcCall) error {
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(runner.ctx, timeout)
	defer cancel()
	if process != nil {
		go func() {
			select {
			case <-process.done:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	cursor := runner.hub.cursor()
	requestTimeout := timeout - time.Second
	if requestTimeout <= 0 {
		requestTimeout = timeout
	}
	message, metadata, err := call(ctx, target.request(requestTimeout))
	if err != nil {
		return processAwareError(process, "implant exited during "+operation, err)
	}
	if message == nil || metadata == nil {
		return errors.New("Sliver returned empty response metadata")
	}
	if metadata.Err != "" {
		return errors.New(metadata.Err)
	}
	if target.beacon == nil {
		if metadata.Async {
			return errors.New("session response was unexpectedly asynchronous")
		}
		proto.Reset(destination)
		data, err := proto.Marshal(message)
		if err != nil {
			return err
		}
		return proto.Unmarshal(data, destination)
	}
	if !metadata.Async || metadata.TaskID == "" || metadata.BeaconID != target.beacon.ID {
		return errors.New("beacon response did not contain matching asynchronous task metadata")
	}
	taskID := metadata.TaskID
	_, _, err = runner.hub.wait(ctx, cursor, func(event *clientpb.Event) bool {
		if event.EventType != consts.BeaconTaskResultEvent {
			return false
		}
		task := &clientpb.BeaconTask{}
		return proto.Unmarshal(event.Data, task) == nil && task.ID == taskID && task.BeaconID == target.beacon.ID
	})
	if err != nil {
		task, fetchErr := runner.fetchBeaconTaskForDiagnostic(taskID)
		if fetchErr == nil && beaconTaskCompleted(task, taskID, target.beacon.ID) {
			// The server persists task completion before publishing the event. Its
			// event broker is deliberately lossy, so the persisted response is the
			// authoritative recovery path when that notification is missed.
			return decodeBeaconTask(task, taskID, target.beacon.ID, destination)
		}
		waitErr := fmt.Errorf("wait for beacon task %s: %w", taskID, err)
		if fetchErr != nil {
			waitErr = fmt.Errorf("%w (fetch task state: %v)", waitErr, fetchErr)
		} else if task == nil {
			waitErr = fmt.Errorf("%w (task lookup returned empty response)", waitErr)
		} else {
			waitErr = fmt.Errorf("%w (task beacon=%q state=%q sent_at=%d completed_at=%d response_bytes=%d)",
				waitErr, task.BeaconID, task.State, task.SentAt, task.CompletedAt, len(task.Response))
		}
		return processAwareError(process, "implant exited while waiting for "+operation+" beacon task", waitErr)
	}
	task, err := runner.rpc.GetBeaconTaskContent(ctx, &clientpb.BeaconTask{ID: taskID})
	if err != nil {
		diagnosticTask, diagnosticErr := runner.fetchBeaconTaskForDiagnostic(taskID)
		if diagnosticErr == nil && beaconTaskCompleted(diagnosticTask, taskID, target.beacon.ID) {
			return decodeBeaconTask(diagnosticTask, taskID, target.beacon.ID, destination)
		}
		return processAwareError(process, "implant exited while fetching "+operation+" beacon task", err)
	}
	return decodeBeaconTask(task, taskID, target.beacon.ID, destination)
}

func (runner *runtimeHarness) fetchBeaconTaskForDiagnostic(taskID string) (*clientpb.BeaconTask, error) {
	ctx, cancel := context.WithTimeout(runner.ctx, taskDiagnosticTime)
	defer cancel()
	return runner.rpc.GetBeaconTaskContent(ctx, &clientpb.BeaconTask{ID: taskID})
}

func beaconTaskCompleted(task *clientpb.BeaconTask, taskID, beaconID string) bool {
	return task != nil && task.ID == taskID && task.BeaconID == beaconID && task.State == "completed" && task.CompletedAt > 0
}

func decodeBeaconTask(task *clientpb.BeaconTask, taskID, beaconID string, destination proto.Message) error {
	if task == nil || task.ID != taskID || task.BeaconID != beaconID {
		return fmt.Errorf("beacon task %s correlation mismatch", taskID)
	}
	if task.State != "completed" || task.CompletedAt <= 0 {
		return fmt.Errorf("beacon task %s incomplete (state=%q)", taskID, task.State)
	}
	proto.Reset(destination)
	if err := proto.Unmarshal(task.Response, destination); err != nil {
		return fmt.Errorf("decode beacon task %s: %w", taskID, err)
	}
	response := responseMetadata(destination)
	if response == nil {
		return errors.New("decoded beacon response has no metadata")
	}
	if response.Err != "" {
		return errors.New(response.Err)
	}
	return nil
}

func processAwareError(process *managedProcess, message string, err error) error {
	if process == nil || process.running() {
		return err
	}
	return errors.Join(err, process.failure(message))
}

func wrapError(message string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", message, err)
}

func responseMetadata(message proto.Message) *commonpb.Response {
	switch value := message.(type) {
	case *sliverpb.CallExtension:
		return value.Response
	case *sliverpb.RegisterExtension:
		return value.Response
	case *sliverpb.ListExtensions:
		return value.Response
	default:
		return nil
	}
}

func (runner *runtimeHarness) addFailure(testCase Case, mode string, repeat int, stage string, err error, implantLog string) {
	runner.report.Add(results.CaseResult{
		ID: testCase.ID, Suite: testCase.Suite, Test: testCase.Name, Collection: testCase.Collection,
		Object:       reportObjectPath(testCase),
		ObjectSHA256: testCase.ObjectSHA256, Mode: mode, Executor: testCase.Executor,
		Repeat: repeat, Status: "fail", Stage: stage, Error: runner.diagnostics(err, implantLog),
	})
}

func reportObjectPath(testCase Case) string {
	if testCase.ObjectReportPath != "" {
		return testCase.ObjectReportPath
	}
	return testCase.ObjectPath
}

func (runner *runtimeHarness) diagnostics(err error, implantLog string) string {
	parts := []string{err.Error()}
	if implantLog != "" {
		parts = append(parts, "implant log:\n"+readLogTail(implantLog))
	}
	if runner.serverLog != "" {
		parts = append(parts, "server log:\n"+readLogTail(runner.serverLog))
	}
	return strings.Join(parts, "\n")
}

func (runner *runtimeHarness) writeOutput(testCase Case, mode string, repeat int, output []byte) (string, error) {
	relative := filepath.Join("outputs", caseArtifactKey(testCase), fmt.Sprintf("%s-%03d.bin", mode, repeat))
	path := filepath.Join(runner.opts.ResultsDir, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, output, 0o600); err != nil {
		return "", err
	}
	return filepath.ToSlash(relative), nil
}

func caseArtifactKey(testCase Case) string {
	identity := testCase.ID
	if identity == "" {
		identity = testCase.Suite + "/" + testCase.Collection + "/" + testCase.Name
	}
	digest := sha256.Sum256([]byte(identity))
	readable := safeName(identity)
	if len(readable) > 80 {
		readable = strings.TrimRight(readable[:80], "-_")
	}
	return readable + "-" + hex.EncodeToString(digest[:16])
}

func (runner *runtimeHarness) close() error {
	var cleanupErrors []error
	if runner.rpc != nil && runner.listener != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := runner.rpc.KillJob(ctx, &clientpb.KillJobReq{ID: runner.listener.JobID})
		cancel()
		if err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("stop HTTP listener: %w", err))
		}
	}
	if runner.cancel != nil {
		runner.cancel()
	}
	if runner.conn != nil {
		clienttransport.CloseGRPCConnection(runner.conn)
	}
	if runner.server != nil {
		if err := runner.server.stop(); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("stop Sliver server: %w", err))
		} else if err := runner.tracker.removePID(runner.server.PID()); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove Sliver server from cleanup state: %w", err))
		}
	}
	if runner.workDir != "" {
		if err := os.RemoveAll(runner.workDir); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		} else if err := runner.tracker.removeTempDir(runner.workDir); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove work directory from cleanup state: %w", err))
		}
	}
	if len(cleanupErrors) == 0 && runner.tracker.empty() {
		if err := runner.tracker.clear(); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("clear cleanup state: %w", err))
		}
	}
	return errors.Join(cleanupErrors...)
}

func unusedTCPPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func waitForTCP(ctx context.Context, address string, process *managedProcess) error {
	ticker := time.NewTicker(listenerPoll)
	defer ticker.Stop()
	for {
		connection, err := net.DialTimeout("tcp", address, listenerDialTimeout)
		if err == nil {
			_ = connection.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-process.done:
			return process.failure("process exited before accepting TCP connections")
		case <-ticker.C:
		}
	}
}

func runCommand(ctx context.Context, dir string, env []string, path string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, path, args...)
	command.Dir = dir
	command.Env = env
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func requestedModes(cases []Case) []string {
	wanted := map[string]bool{}
	for _, testCase := range cases {
		for _, mode := range testCase.Modes {
			wanted[mode] = true
		}
	}
	ordered := []string{}
	for _, mode := range []string{"session", "beacon"} {
		if wanted[mode] {
			ordered = append(ordered, mode)
		}
	}
	return ordered
}

func safeName(value string) string {
	value = strings.ToLower(value)
	var builder strings.Builder
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' || character == '_' {
			builder.WriteRune(character)
		} else {
			builder.WriteByte('-')
		}
	}
	result := strings.Trim(builder.String(), "-")
	if result == "" {
		return "case"
	}
	return result
}
