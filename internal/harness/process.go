package harness

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	cleanupGraceTimeout   = 2 * time.Second
	cleanupProcessTimeout = 10 * time.Second
	logTailBytes          = 256 << 10
)

type managedProcess struct {
	cmd     *exec.Cmd
	done    chan struct{}
	err     error
	logPath string
	tree    processTree
	once    sync.Once
	stopErr error
}

func startProcess(path string, args []string, dir string, env []string, logPath string) (*managedProcess, error) {
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(path, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	prepareCommand(cmd)
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, err
	}
	tree, err := attachProcessTree(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = logFile.Close()
		return nil, fmt.Errorf("attach process %d to managed tree: %w", cmd.Process.Pid, err)
	}
	process := &managedProcess{cmd: cmd, done: make(chan struct{}), logPath: logPath, tree: tree}
	go func() {
		process.err = cmd.Wait()
		_ = logFile.Close()
		close(process.done)
	}()
	return process, nil
}

func (process *managedProcess) PID() int { return process.cmd.Process.Pid }

func (process *managedProcess) stop() error {
	process.once.Do(func() {
		process.stopErr = process.stopOnce()
	})
	return process.stopErr
}

func (process *managedProcess) stopOnce() (stopErr error) {
	defer func() {
		if err := closeProcessTree(process.tree); err != nil {
			stopErr = errors.Join(stopErr, fmt.Errorf("close process tree: %w", err))
		}
	}()
	select {
	case <-process.done:
		stopErr = killProcessTree(process.tree, process.cmd)
		return
	default:
	}
	if err := terminateProcessTree(process.tree, process.cmd); err != nil {
		stopErr = err
		return
	}
	select {
	case <-process.done:
		stopErr = killProcessTree(process.tree, process.cmd)
		return
	case <-time.After(cleanupGraceTimeout):
	}
	if err := killProcessTree(process.tree, process.cmd); err != nil {
		stopErr = err
		return
	}
	select {
	case <-process.done:
	case <-time.After(cleanupProcessTimeout):
		stopErr = fmt.Errorf("process %d remained alive after kill", process.PID())
	}
	return stopErr
}

func (process *managedProcess) failure(message string) error {
	err := process.err
	if err == nil {
		err = errors.New("process exited")
	}
	return fmt.Errorf("%s: %w\n%s", message, err, readLogTail(process.logPath))
}

func (process *managedProcess) running() bool {
	select {
	case <-process.done:
		return false
	default:
		return true
	}
}

func readLogTail(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("(could not read %s: %v)", path, err)
	}
	defer file.Close()
	end, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Sprintf("(could not inspect %s: %v)", path, err)
	}
	start := end - logTailBytes
	if start < 0 {
		start = 0
	}
	_, _ = file.Seek(start, io.SeekStart)
	data, err := io.ReadAll(io.LimitReader(file, logTailBytes))
	if err != nil {
		return fmt.Sprintf("(could not read %s: %v)", path, err)
	}
	return strings.TrimSpace(string(data))
}

func envWith(env []string, name string, value string) []string {
	updated := make([]string, 0, len(env)+1)
	for _, entry := range env {
		entryName, _, found := strings.Cut(entry, "=")
		if !found || !strings.EqualFold(entryName, name) {
			updated = append(updated, entry)
		}
	}
	return append(updated, name+"="+value)
}

func sanitizedImplantEnv(hostEnv []string, home string, temp string, marker string) []string {
	allowed := map[string]bool{
		"COMSPEC": true, "PATH": true, "PATHEXT": true, "SYSTEMDRIVE": true,
		"SYSTEMROOT": true, "WINDIR": true,
	}
	result := make([]string, 0, len(allowed)+7)
	for _, entry := range hostEnv {
		name, value, found := strings.Cut(entry, "=")
		if found && allowed[strings.ToUpper(name)] {
			result = envWith(result, strings.ToUpper(name), value)
		}
	}
	for name, value := range map[string]string{
		"HOME": home, "USERPROFILE": home, "TMP": temp, "TEMP": temp, "TMPDIR": temp,
		"SLIVER_BOF_E2E_MARKER": marker,
	} {
		result = envWith(result, name, value)
	}
	sort.Strings(result)
	return result
}

// sanitizedServerEnv deliberately excludes GitHub Actions credentials and
// caller-defined build controls from the environment inherited by Sliver. The
// server only receives the host process-launch essentials and private roots for
// every file it creates during the test.
func sanitizedServerEnv(hostEnv []string, home, temp, serverRoot, clientRoot string) []string {
	allowed := map[string]bool{
		"COMSPEC": true, "LANG": true, "LC_ALL": true, "LC_CTYPE": true,
		"PATH": true, "PATHEXT": true, "SYSTEMDRIVE": true, "SYSTEMROOT": true,
		"TZ": true, "WINDIR": true,
	}
	result := make([]string, 0, len(allowed)+9)
	for _, entry := range hostEnv {
		name, value, found := strings.Cut(entry, "=")
		if found && allowed[strings.ToUpper(name)] {
			result = envWith(result, strings.ToUpper(name), value)
		}
	}
	for name, value := range map[string]string{
		"HOME": home, "USERPROFILE": home,
		"TMP": temp, "TEMP": temp, "TMPDIR": temp,
		"SLIVER_ROOT_DIR": serverRoot, "SLIVER_CLIENT_ROOT_DIR": clientRoot,
	} {
		result = envWith(result, name, value)
	}
	sort.Strings(result)
	return result
}
