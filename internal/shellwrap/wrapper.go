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
	NodeFSShimPath string
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
	if cfg.NodeFSShimPath != "" {
		env = prependNodeRequire(env, cfg.NodeFSShimPath)
	}
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

func WriteNodeFSShim(dir string) (string, error) {
	path := filepath.Join(dir, "node-fs-shim.cjs")
	const script = `'use strict';

const fs = require('fs');
const path = require('path');
const childProcess = require('child_process');

const remoteRootRaw = process.env.AGENT_BRIDGE_REMOTE_ROOT || '';
const workspaceRaw = process.env.AGENT_BRIDGE_WORKSPACE || '';

if (remoteRootRaw && workspaceRaw) {
  const remoteRoot = trimSlash(toSlash(remoteRootRaw));
  const workspace = trimSlash(toSlash(workspaceRaw));
  const workspaceNative = fromSlash(workspace);
  let logicalCwd = remoteRoot || '/';

  function toSlash(value) {
    return String(value).replace(/\\/g, '/');
  }

  function trimSlash(value) {
    if (value === '/') return value;
    return value.replace(/\/+$/g, '');
  }

  function fromSlash(value) {
    return process.platform === 'win32' ? value.replace(/\//g, '\\') : value;
  }

  function stripDrive(value) {
    return value.replace(/^[A-Za-z]:(?=\/)/, '');
  }

  function localizeString(value) {
    const slashed = toSlash(value);
    const noDrive = stripDrive(slashed);
    if (noDrive === remoteRoot) {
      return workspaceNative;
    }
    if (noDrive.startsWith(remoteRoot + '/')) {
      return fromSlash(workspace + '/' + noDrive.slice(remoteRoot.length + 1));
    }
    return value;
  }

  function remoteizeString(value) {
    const slashed = trimSlash(toSlash(value));
    if (slashed === workspace) {
      return remoteRoot;
    }
    if (slashed.startsWith(workspace + '/')) {
      return remoteRoot + '/' + slashed.slice(workspace.length + 1);
    }
    return value;
  }

  function localizePath(value) {
    if (typeof value === 'string') {
      return localizeString(value);
    }
    return value;
  }

  function mapIndexes(args, indexes) {
    const mapped = Array.prototype.slice.call(args);
    for (const index of indexes) {
      if (index < mapped.length) {
        mapped[index] = localizePath(mapped[index]);
      }
    }
    return mapped;
  }

  function wrapMethod(target, name, indexes) {
    const original = target && target[name];
    if (typeof original !== 'function') return;
    target[name] = function agentBridgePathShim() {
      return Reflect.apply(original, this, mapIndexes(arguments, indexes));
    };
  }

  function wrapRealpath(target, name) {
    const original = target && target[name];
    if (typeof original !== 'function') return;
    target[name] = function agentBridgeRealpathShim() {
      const args = mapIndexes(arguments, [0]);
      const last = args[args.length - 1];
      if (typeof last === 'function') {
        args[args.length - 1] = function(err, resolved) {
          if (err) return last.apply(this, arguments);
          return last.call(this, err, remoteizeString(resolved));
        };
      }
      return Reflect.apply(original, this, args);
    };
  }

  function wrapRealpathSync(target, name) {
    const original = target && target[name];
    if (typeof original !== 'function') return;
    target[name] = function agentBridgeRealpathSyncShim() {
      return remoteizeString(Reflect.apply(original, this, mapIndexes(arguments, [0])));
    };
  }

  const onePath = [
    'access', 'accessSync', 'appendFile', 'appendFileSync', 'chmod', 'chmodSync',
    'chown', 'chownSync', 'createReadStream', 'createWriteStream', 'exists',
    'existsSync', 'lchmod', 'lchmodSync', 'lchown', 'lchownSync', 'lstat',
    'lstatSync', 'lutimes', 'lutimesSync', 'mkdir', 'mkdirSync', 'mkdtemp',
    'mkdtempSync', 'open', 'openSync', 'opendir', 'opendirSync', 'readdir',
    'readdirSync', 'readFile', 'readFileSync', 'readlink', 'readlinkSync',
    'rm', 'rmSync', 'rmdir', 'rmdirSync', 'stat', 'statSync', 'statfs',
    'statfsSync', 'truncate', 'truncateSync', 'unlink', 'unlinkSync',
    'utimes', 'utimesSync', 'watch', 'watchFile', 'writeFile', 'writeFileSync'
  ];
  for (const name of onePath) wrapMethod(fs, name, [0]);

  for (const name of ['copyFile', 'copyFileSync', 'cp', 'cpSync', 'link', 'linkSync', 'rename', 'renameSync']) {
    wrapMethod(fs, name, [0, 1]);
  }
  for (const name of ['symlink', 'symlinkSync']) {
    wrapMethod(fs, name, [1]);
  }
  wrapRealpath(fs, 'realpath');
  wrapRealpathSync(fs, 'realpathSync');
  if (fs.realpath && fs.realpath.native) wrapRealpath(fs.realpath, 'native');
  if (fs.realpathSync && fs.realpathSync.native) wrapRealpathSync(fs.realpathSync, 'native');

  if (fs.promises) {
    for (const name of [
      'access', 'appendFile', 'chmod', 'chown', 'copyFile', 'cp', 'lchmod',
      'lchown', 'link', 'lstat', 'lutimes', 'mkdir', 'mkdtemp', 'open',
      'opendir', 'readdir', 'readFile', 'readlink', 'rm', 'rename', 'rmdir',
      'stat', 'statfs', 'symlink', 'truncate', 'unlink', 'utimes', 'writeFile'
    ]) {
      const indexes = ['copyFile', 'cp', 'link', 'rename'].includes(name) ? [0, 1] : (name === 'symlink' ? [1] : [0]);
      wrapMethod(fs.promises, name, indexes);
    }
    const realpathPromise = fs.promises.realpath;
    if (typeof realpathPromise === 'function') {
      fs.promises.realpath = async function agentBridgeRealpathPromiseShim() {
        const resolved = await Reflect.apply(realpathPromise, this, mapIndexes(arguments, [0]));
        return remoteizeString(resolved);
      };
    }
  }

  const realCwd = process.cwd.bind(process);
  const realChdir = process.chdir.bind(process);
  process.cwd = function agentBridgeCwdShim() {
    return logicalCwd;
  };
  process.chdir = function agentBridgeChdirShim(dir) {
    const localized = localizeString(dir);
    realChdir(localized);
    const remote = stripDrive(toSlash(dir));
    if (remote === remoteRoot || remote.startsWith(remoteRoot + '/')) {
      logicalCwd = trimSlash(remote);
    } else {
      logicalCwd = remoteizeString(realCwd());
    }
  };

  function mapChildOptions(options) {
    if (!options || typeof options !== 'object' || !options.cwd) return options;
    return Object.assign({}, options, { cwd: localizeString(options.cwd) });
  }

  function wrapChild(name, optionsIndex) {
    const original = childProcess[name];
    if (typeof original !== 'function') return;
    childProcess[name] = function agentBridgeChildShim() {
      const args = Array.prototype.slice.call(arguments);
      if (optionsIndex < args.length) args[optionsIndex] = mapChildOptions(args[optionsIndex]);
      return Reflect.apply(original, this, args);
    };
  }

  wrapChild('spawn', 2);
  wrapChild('spawnSync', 2);
  wrapChild('execFile', 2);
  wrapChild('execFileSync', 2);
  wrapChild('fork', 2);

  const exec = childProcess.exec;
  if (typeof exec === 'function') {
    childProcess.exec = function agentBridgeExecShim(command, options, callback) {
      if (typeof options === 'function') return exec.call(this, command, options);
      return exec.call(this, command, mapChildOptions(options), callback);
    };
  }
  const execSync = childProcess.execSync;
  if (typeof execSync === 'function') {
    childProcess.execSync = function agentBridgeExecSyncShim(command, options) {
      return execSync.call(this, command, mapChildOptions(options));
    };
  }
}
`
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
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

func prependNodeRequire(env []string, shimPath string) []string {
	requireOpt := "--require=" + nodeOptionValue(shimPath)
	for i, item := range env {
		if strings.HasPrefix(item, "NODE_OPTIONS=") {
			current := strings.TrimPrefix(item, "NODE_OPTIONS=")
			if strings.Contains(current, shimPath) {
				return env
			}
			if current == "" {
				env[i] = "NODE_OPTIONS=" + requireOpt
			} else {
				env[i] = "NODE_OPTIONS=" + requireOpt + " " + current
			}
			return env
		}
	}
	return append(env, "NODE_OPTIONS="+requireOpt)
}

func nodeOptionValue(value string) string {
	if !strings.ContainsAny(value, " \t\"") {
		return value
	}
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
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
