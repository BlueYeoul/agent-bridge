package shellwrap

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/BlueYeoul/agent-bridge/internal/remotesync"
	"github.com/BlueYeoul/agent-bridge/internal/sshx"
)

type Config struct {
	BridgePath     string
	BashEnvPath    string
	SocketPath     string
	Target         string
	Port           int
	PortSet        bool
	ControlEnabled bool
	WorkspacePath  string
	HomePath       string
	StatePath      string
	RemoteRoot     string
}

func ApplyEnv(env []string, cfg Config) []string {
	env = upsertEnv(env, "SHELL", cfg.BridgePath)
	if cfg.BashEnvPath != "" {
		env = upsertEnv(env, "BASH_ENV", cfg.BashEnvPath)
	}
	env = upsertEnv(env, "PWD", cfg.WorkspacePath)
	env = upsertEnv(env, "CWD", cfg.WorkspacePath)
	env = upsertEnv(env, "AGENT_BRIDGE", "1")
	env = upsertEnv(env, "AGENT_BRIDGE_FAKE_SHELL", "1")
	env = upsertEnv(env, "AGENT_BRIDGE_BINARY", cfg.BridgePath)
	env = upsertEnv(env, "AGENT_BRIDGE_SOCKET", cfg.SocketPath)
	env = upsertEnv(env, "AGENT_BRIDGE_CONTROL_ENABLED", boolString(cfg.ControlEnabled))
	env = upsertEnv(env, "AGENT_BRIDGE_TARGET", cfg.Target)
	env = upsertEnv(env, "AGENT_BRIDGE_PORT", strconv.Itoa(cfg.Port))
	env = upsertEnv(env, "AGENT_BRIDGE_PORT_SET", boolString(cfg.PortSet))
	env = upsertEnv(env, "AGENT_BRIDGE_WORKSPACE", cfg.WorkspacePath)
	env = upsertEnv(env, "AGENT_BRIDGE_HOME", cfg.HomePath)
	env = upsertEnv(env, "AGENT_BRIDGE_STATE", cfg.StatePath)
	env = upsertEnv(env, "AGENT_BRIDGE_REMOTE_ROOT", cfg.RemoteRoot)
	return env
}

func WriteBashEnvShim(dir, bridgePath string) (string, error) {
	path := filepath.Join(dir, "bash-env")
	script := fmt.Sprintf(`if [[ -n "${AGENT_BRIDGE_BASH_ENV_BYPASS:-}" ]]; then
  return 0 2>/dev/null || exit 0
fi

if [[ -n "${BASH_EXECUTION_STRING:-}" && -n "${AGENT_BRIDGE_BINARY:-}" ]]; then
  export AGENT_BRIDGE_BASH_ENV_BYPASS=1
  exec %s __bash-env "$BASH_EXECUTION_STRING"
fi
`, sshx.ShellQuote(bridgePath))
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		return "", err
	}
	return path, nil
}

func WriteGitShim(binDir, bridgePath string) (string, error) {
	if runtime.GOOS == "windows" {
		path := filepath.Join(binDir, "git.cmd")
		script := fmt.Sprintf("@echo off\r\n\"%s\" __git-shim %%*\r\n", bridgePath)
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
			return "", err
		}
		return path, nil
	}

	path := filepath.Join(binDir, "git")
	script := fmt.Sprintf("#!/bin/sh\nexec %s __git-shim \"$@\"\n", sshx.ShellQuote(bridgePath))
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		return "", err
	}
	return path, nil
}

func RunFakeShell(args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	cfg, err := configFromEnv()
	if err != nil {
		fmt.Fprintf(stderr, "agent-bridge shell: %v\n", err)
		return 2
	}

	command, ok := shellCommand(args)
	if !ok {
		return runInteractiveShell(cfg, stdin, stdout, stderr)
	}

	return RunCommand(command, stdin, stdout, stderr)
}

func RunCommand(command string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	cfg, err := configFromEnv()
	if err != nil {
		fmt.Fprintf(stderr, "agent-bridge shell: %v\n", err)
		return 2
	}

	if err := remotesync.UploadChanges(cfg.SSH(), cfg.WorkspacePath, cfg.RemoteRoot, cfg.StatePath); err != nil {
		fmt.Fprintf(stderr, "agent-bridge shell: sync upload failed: %v\n", err)
		return 1
	}

	translated := TranslateCommand(command, cfg.WorkspacePath, cfg.HomePath)
	remoteCommand := "cd " + sshx.ShellQuote(cfg.RemoteRoot) + " && " + translated
	code, runErr := cfg.SSH().Run(remoteCommand, stdin, stdout, stderr)

	if err := remotesync.DownloadChanges(cfg.SSH(), cfg.WorkspacePath, cfg.RemoteRoot, cfg.StatePath); err != nil {
		fmt.Fprintf(stderr, "agent-bridge shell: sync download failed: %v\n", err)
		if runErr == nil && code == 0 {
			return 1
		}
	}
	if runErr != nil {
		fmt.Fprintf(stderr, "agent-bridge shell: remote command failed: %v\n", runErr)
	}
	return code
}

func RunGitShim(args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	cfg, err := configFromEnv()
	if err != nil {
		fmt.Fprintf(stderr, "agent-bridge git: %v\n", err)
		return 2
	}
	if err := remotesync.UploadChanges(cfg.SSH(), cfg.WorkspacePath, cfg.RemoteRoot, cfg.StatePath); err != nil {
		fmt.Fprintf(stderr, "agent-bridge git: sync upload failed: %v\n", err)
		return 1
	}

	parts := []string{"cd " + sshx.ShellQuote(cfg.RemoteRoot) + " && git"}
	for _, arg := range args {
		parts = append(parts, sshx.ShellQuote(TranslateCommand(arg, cfg.WorkspacePath, cfg.HomePath)))
	}
	code, runErr := cfg.SSH().Run(strings.Join(parts, " "), stdin, stdout, stderr)

	if err := remotesync.DownloadChanges(cfg.SSH(), cfg.WorkspacePath, cfg.RemoteRoot, cfg.StatePath); err != nil {
		fmt.Fprintf(stderr, "agent-bridge git: sync download failed: %v\n", err)
		if runErr == nil && code == 0 {
			return 1
		}
	}
	if runErr != nil {
		fmt.Fprintf(stderr, "agent-bridge git: remote git failed: %v\n", runErr)
	}
	return code
}

func TranslateCommand(command, workspace, home string) string {
	replacements := []string{
		workspace,
		filepath.ToSlash(workspace),
		tildePath(home, workspace),
		filepath.ToSlash(tildePath(home, workspace)),
	}
	result := command
	seen := map[string]bool{}
	for _, pattern := range replacements {
		if pattern == "" || seen[pattern] {
			continue
		}
		seen[pattern] = true
		result = replacePathPrefix(result, pattern)
	}
	return result
}

func replacePathPrefix(command, pattern string) string {
	var b strings.Builder
	rest := command
	for {
		idx := strings.Index(rest, pattern)
		if idx < 0 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:idx])
		b.WriteByte('.')

		tail := rest[idx+len(pattern):]
		cut := 0
		for cut < len(tail) {
			r := rune(tail[cut])
			if isCommandDelimiter(r) {
				break
			}
			if tail[cut] == '\\' {
				b.WriteByte('/')
			} else {
				b.WriteByte(tail[cut])
			}
			cut++
		}
		rest = tail[cut:]
	}
}

func isCommandDelimiter(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '\'', '"', '`', ';', '|', '&', '<', '>':
		return true
	default:
		return false
	}
}

func (c Config) SSH() sshx.Config {
	return sshx.Config{
		SocketPath:     c.SocketPath,
		Target:         c.Target,
		Port:           c.Port,
		PortSet:        c.PortSet,
		ControlEnabled: c.ControlEnabled,
	}
}

func runInteractiveShell(cfg Config, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	remoteCommand := "cd " + sshx.ShellQuote(cfg.RemoteRoot) + " && exec ${SHELL:-/bin/sh}"
	args := cfg.SSH().Args()
	if isTerminal(os.Stdin) && isTerminal(os.Stdout) {
		args = append([]string{"-tt"}, args...)
	} else {
		args = append([]string{"-T"}, args...)
	}
	args = append(args, cfg.Target, remoteCommand)
	cmd := exec.Command("ssh", args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	code, err := sshx.ExitCode(cmd.Run())
	if err != nil {
		fmt.Fprintf(stderr, "agent-bridge shell: remote shell failed: %v\n", err)
	}
	return code
}

func shellCommand(args []string) (string, bool) {
	for i, arg := range args {
		if strings.HasPrefix(arg, "-") && strings.Contains(arg, "c") {
			next := i + 1
			if next < len(args) {
				return args[next], true
			}
			return "", true
		}
	}
	return "", false
}

func configFromEnv() (Config, error) {
	port := 22
	if raw := os.Getenv("AGENT_BRIDGE_PORT"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid AGENT_BRIDGE_PORT: %w", err)
		}
		port = parsed
	}
	cfg := Config{
		BridgePath:     os.Getenv("AGENT_BRIDGE_BINARY"),
		SocketPath:     os.Getenv("AGENT_BRIDGE_SOCKET"),
		Target:         os.Getenv("AGENT_BRIDGE_TARGET"),
		Port:           port,
		PortSet:        os.Getenv("AGENT_BRIDGE_PORT_SET") == "1",
		ControlEnabled: os.Getenv("AGENT_BRIDGE_CONTROL_ENABLED") == "1",
		WorkspacePath:  os.Getenv("AGENT_BRIDGE_WORKSPACE"),
		HomePath:       os.Getenv("AGENT_BRIDGE_HOME"),
		StatePath:      os.Getenv("AGENT_BRIDGE_STATE"),
		RemoteRoot:     os.Getenv("AGENT_BRIDGE_REMOTE_ROOT"),
	}
	if cfg.Target == "" || cfg.WorkspacePath == "" || cfg.StatePath == "" || cfg.RemoteRoot == "" {
		return Config{}, fmt.Errorf("missing agent-bridge environment")
	}
	return cfg, nil
}

func tildePath(home, path string) string {
	if home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(os.PathSeparator)) {
		return "~" + strings.TrimPrefix(path, home)
	}
	return path
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

func boolString(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
