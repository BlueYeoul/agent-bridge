package session

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/BlueYeoul/agent-bridge/internal/cli"
	"github.com/BlueYeoul/agent-bridge/internal/remotesync"
	"github.com/BlueYeoul/agent-bridge/internal/shellwrap"
	"github.com/BlueYeoul/agent-bridge/internal/sshx"
)

type Runner struct {
	cfg cli.Config
	out io.Writer

	sessionID      string
	home           string
	baseDir        string
	sessionDir     string
	binDir         string
	socketPath     string
	workspaceRoot  string
	workspace      string
	statePath      string
	bridgePath     string
	bashEnvPath    string
	nodeFSShimPath string
	remoteRoot     string

	controlEnabled bool
	masterStarted  bool
	cleanupOnce    sync.Once
	cleanupErr     error
}

func New(cfg cli.Config, out io.Writer) (*Runner, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	id, err := newSessionID()
	if err != nil {
		return nil, fmt.Errorf("generate session id: %w", err)
	}
	base := filepath.Join(home, ".agtbge")
	sessionDir := filepath.Join(base, "session_"+id)

	return &Runner{
		cfg:           cfg,
		out:           out,
		sessionID:     id,
		home:          home,
		baseDir:       base,
		sessionDir:    sessionDir,
		binDir:        filepath.Join(sessionDir, "bin"),
		socketPath:    filepath.Join(base, "sockets", "sock_"+id),
		workspaceRoot: filepath.Join(base, "mirrors", "session_"+id),
		statePath:     filepath.Join(sessionDir, "state.json"),
	}, nil
}

func (r *Runner) Run() (code int, err error) {
	defer func() {
		if cleanupErr := r.Cleanup(); cleanupErr != nil {
			if err == nil {
				err = cleanupErr
			} else {
				err = fmt.Errorf("%w; cleanup: %v", err, cleanupErr)
			}
			if code == 0 {
				code = 1
			}
		}
	}()

	if err := r.setup(); err != nil {
		return 1, err
	}
	return r.launch()
}

func (r *Runner) setup() error {
	if err := r.preflight(); err != nil {
		return err
	}
	if err := r.prepareDirs(); err != nil {
		return err
	}

	r.logf("agent-bridge: opening SSH control master for %s", r.cfg.Target)
	if err := r.startMaster(); err != nil {
		return err
	}

	r.logf("agent-bridge: resolving remote workspace %s", r.cfg.RemotePath)
	remoteRoot, err := r.resolveRemoteRoot()
	if err != nil {
		return err
	}
	r.remoteRoot = remoteRoot
	r.workspace = r.localMirrorPath(remoteRoot)

	r.logf("agent-bridge: downloading %s:%s into %s", r.cfg.Target, r.remoteRoot, r.workspace)
	if err := r.downloadRemote(); err != nil {
		return err
	}

	if err := r.writeWrappers(); err != nil {
		return err
	}
	return nil
}

func (r *Runner) preflight() error {
	for _, name := range []string{"ssh"} {
		if _, err := exec.LookPath(name); err != nil {
			return fmt.Errorf("missing dependency %q in PATH", name)
		}
	}
	if _, err := exec.LookPath(r.cfg.Launch); err != nil {
		return fmt.Errorf("launch binary %q was not found in PATH", r.cfg.Launch)
	}
	return nil
}

func (r *Runner) prepareDirs() error {
	dirs := []string{
		filepath.Join(r.baseDir, "sockets"),
		r.sessionDir,
		r.binDir,
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}

func (r *Runner) startMaster() error {
	args := []string{
		"-M",
		"-S", r.socketPath,
		"-fN",
	}
	args = append(args, r.sshPortArgs()...)
	args = append(args, r.cfg.Target)
	cmd := exec.Command("ssh", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	var stderr bytes.Buffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if controlMasterUnsupported(msg) {
			r.logf("agent-bridge: SSH ControlMaster is unsupported by this client; continuing with direct ssh")
			return nil
		}
		return fmt.Errorf("start SSH control master: %w: %s", err, msg)
	}
	r.masterStarted = true
	r.controlEnabled = true
	return nil
}

func (r *Runner) resolveRemoteRoot() (string, error) {
	remoteCmd := "cd " + sshx.ShellQuote(r.cfg.RemotePath) + " && pwd -P"
	cmd := r.sshConfig().Command(remoteCmd)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("resolve remote path: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	root := strings.TrimSpace(stdout.String())
	if root == "" {
		return "", fmt.Errorf("resolve remote path: remote pwd returned empty output")
	}
	return root, nil
}

func (r *Runner) downloadRemote() error {
	if err := remotesync.RemoteTarAvailable(r.sshConfig(), r.remoteRoot); err != nil {
		return err
	}
	return remotesync.Download(r.sshConfig(), r.workspace, r.remoteRoot, r.statePath)
}

func (r *Runner) writeWrappers() error {
	bridgePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve agent-bridge executable: %w", err)
	}
	r.bridgePath = bridgePath

	if _, err := shellwrap.WriteGitShim(r.binDir, bridgePath); err != nil {
		return fmt.Errorf("write git shim: %w", err)
	}
	bashEnvPath, err := shellwrap.WriteBashEnvShim(r.sessionDir, bridgePath)
	if err != nil {
		return fmt.Errorf("write bash env shim: %w", err)
	}
	r.bashEnvPath = bashEnvPath
	nodeFSShimPath, err := shellwrap.WriteNodeFSShim(r.sessionDir)
	if err != nil {
		return fmt.Errorf("write node fs shim: %w", err)
	}
	r.nodeFSShimPath = nodeFSShimPath
	return nil
}

func (r *Runner) launch() (int, error) {
	args := r.profileLaunchArgs()
	r.logf("agent-bridge: launching %s inside %s", r.cfg.Launch, r.workspace)

	cmd := exec.Command(r.cfg.Launch, args...)
	cmd.Dir = r.workspace
	cmd.Env = r.agentEnv()
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return 1, fmt.Errorf("launch %s: %w", r.cfg.Launch, err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, interruptSignals()...)
	defer signal.Stop(signals)

	for forwarded := 0; ; {
		select {
		case sig := <-signals:
			forwarded++
			r.logf("agent-bridge: forwarding %s to %s", sig, r.cfg.Launch)
			if forwarded > 1 {
				_ = forceKillChild(cmd)
				return 130, nil
			}
			_ = terminateChild(cmd, sig)
		case err := <-done:
			code, waitErr := sshx.ExitCode(err)
			if syncErr := r.syncLocalChanges(); syncErr != nil {
				if waitErr == nil && code == 0 {
					return 1, syncErr
				}
				if waitErr != nil {
					return code, fmt.Errorf("%w; final sync: %v", waitErr, syncErr)
				}
				return code, syncErr
			}
			return code, waitErr
		}
	}
}

func (r *Runner) agentEnv() []string {
	env := os.Environ()
	env = shellwrap.ApplyEnv(env, shellwrap.Config{
		BridgePath:     r.bridgePath,
		BashEnvPath:    r.bashEnvPath,
		SocketPath:     r.socketPath,
		Target:         r.cfg.Target,
		Port:           r.cfg.Port,
		PortSet:        r.cfg.PortSet,
		ControlEnabled: r.controlEnabled,
		WorkspacePath:  r.workspace,
		HomePath:       r.home,
		StatePath:      r.statePath,
		NodeFSShimPath: r.nodeFSShimPath,
		RemoteRoot:     r.remoteRoot,
	})
	env = r.applyAgentProfileEnv(env)
	env = upsertEnv(env, "PATH", r.binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return env
}

func (r *Runner) Cleanup() error {
	r.cleanupOnce.Do(func() {
		var errs []error
		if r.masterStarted {
			r.logf("agent-bridge: closing SSH control master")
			if err := r.closeMaster(); err != nil {
				errs = append(errs, err)
			}
		}

		if err := os.RemoveAll(r.workspaceRoot); err != nil {
			errs = append(errs, fmt.Errorf("remove workspace directory: %w", err))
		}
		if err := os.RemoveAll(r.sessionDir); err != nil {
			errs = append(errs, fmt.Errorf("remove session directory: %w", err))
		}
		if err := os.Remove(r.socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove socket: %w", err))
		}

		r.cleanupErr = errors.Join(errs...)
	})
	return r.cleanupErr
}

func (r *Runner) closeMaster() error {
	if !r.controlEnabled {
		return nil
	}
	args := []string{"-S", r.socketPath, "-O", "exit"}
	args = append(args, r.sshPortArgs()...)
	args = append(args, r.cfg.Target)
	cmd := exec.Command("ssh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "No such file") || strings.Contains(msg, "No such process") {
			return nil
		}
		return fmt.Errorf("close SSH control master: %w: %s", err, msg)
	}
	return nil
}

func (r *Runner) sshPortArgs() []string {
	if !r.cfg.PortSet {
		return nil
	}
	return []string{"-p", strconv.Itoa(r.cfg.Port)}
}

func (r *Runner) sshConfig() sshx.Config {
	return sshx.Config{
		SocketPath:     r.socketPath,
		Target:         r.cfg.Target,
		Port:           r.cfg.Port,
		PortSet:        r.cfg.PortSet,
		ControlEnabled: r.controlEnabled,
	}
}

func (r *Runner) syncLocalChanges() error {
	if r.remoteRoot == "" || r.statePath == "" {
		return nil
	}
	r.logf("agent-bridge: syncing local edits back to %s:%s", r.cfg.Target, r.remoteRoot)
	return remotesync.UploadChanges(r.sshConfig(), r.workspace, r.remoteRoot, r.statePath)
}

func (r *Runner) profileLaunchArgs() []string {
	args := append([]string{}, r.cfg.LaunchArgs...)
	switch filepath.Base(r.cfg.Launch) {
	case "aider":
		if !hasArg(args, "--no-git") {
			args = append([]string{"--no-git"}, args...)
		}
	}
	return args
}

func hasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func (r *Runner) localMirrorPath(remoteRoot string) string {
	trimmed := strings.TrimPrefix(filepath.ToSlash(remoteRoot), "/")
	if trimmed == "" {
		trimmed = "root"
	}
	parts := []string{r.workspaceRoot, sanitizePathSegment(r.cfg.Target)}
	parts = append(parts, strings.Split(trimmed, "/")...)
	return filepath.Join(parts...)
}

func sanitizePathSegment(segment string) string {
	var b strings.Builder
	for _, r := range segment {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "target"
	}
	return b.String()
}

func (r *Runner) applyAgentProfileEnv(env []string) []string {
	switch filepath.Base(r.cfg.Launch) {
	case "pi":
		profileDir, err := r.preparePiProfile()
		if err != nil {
			r.logf("agent-bridge: warning: pi profile setup failed: %v", err)
			return env
		}
		env = upsertEnv(env, "PI_CODING_AGENT_DIR", profileDir)
		env = upsertEnv(env, "PI_CODING_AGENT_SESSION_DIR", filepath.Join(profileDir, "sessions"))
	}
	return env
}

func (r *Runner) preparePiProfile() (string, error) {
	profileDir := filepath.Join(r.sessionDir, "profiles", "pi-agent")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		return "", err
	}

	sourceDir := os.Getenv("PI_CODING_AGENT_DIR")
	if sourceDir == "" {
		sourceDir = filepath.Join(r.home, ".pi", "agent")
	}
	for _, name := range []string{"auth.json", "models.json"} {
		if err := copyFileIfExists(filepath.Join(sourceDir, name), filepath.Join(profileDir, name)); err != nil {
			return "", err
		}
	}

	settings := map[string]any{}
	sourceSettings := filepath.Join(sourceDir, "settings.json")
	if data, err := os.ReadFile(sourceSettings); err == nil {
		_ = json.Unmarshal(data, &settings)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	settings["shellPath"] = r.bridgePath

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(profileDir, "settings.json"), data, 0o600); err != nil {
		return "", err
	}
	return profileDir, nil
}

func copyFileIfExists(src, dst string) error {
	input, err := os.Open(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer input.Close()

	info, err := input.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	output, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

func upsertEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, item := range env {
		if strings.HasPrefix(item, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func newSessionID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func controlMasterUnsupported(stderr string) bool {
	msg := strings.ToLower(stderr)
	if strings.Contains(msg, "controlmaster") && (strings.Contains(msg, "not support") || strings.Contains(msg, "unsupported")) {
		return true
	}
	if strings.Contains(msg, "mux") && (strings.Contains(msg, "not support") || strings.Contains(msg, "unsupported")) {
		return true
	}
	if strings.Contains(msg, "getsockname failed") && strings.Contains(msg, "not a socket") {
		return true
	}
	return false
}

func (r *Runner) logf(format string, args ...any) {
	if r.out == nil {
		return
	}
	fmt.Fprintf(r.out, format+"\n", args...)
}
