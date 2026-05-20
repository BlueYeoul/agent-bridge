package shellwrap

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/BlueYeoul/agent-bridge/internal/remotesync"
)

func TestTranslateCommand(t *testing.T) {
	workspace := filepath.Join("C:\\Users\\me", ".agtbge", "work_abcd")
	if runtime.GOOS != "windows" {
		workspace = "/Users/me/.agtbge/work_abcd"
	}

	got := TranslateCommand("pytest "+workspace+"/tests/test_core.py", workspace, filepath.Dir(filepath.Dir(workspace)))
	if !strings.Contains(got, "pytest ./tests/test_core.py") {
		t.Fatalf("TranslateCommand() = %q", got)
	}
}

func TestTranslateCommandWindowsPathLiteral(t *testing.T) {
	workspace := `C:\Users\me\.agtbge\work_abcd`
	got := TranslateCommand(`pytest C:\Users\me\.agtbge\work_abcd\tests\test_core.py`, workspace, `C:\Users\me`)
	if got != "pytest ./tests/test_core.py" {
		t.Fatalf("TranslateCommand() = %q", got)
	}
}

func TestWriteGitShim(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteGitShim(dir, "/tmp/agent-bridge")
	if err != nil {
		t.Fatalf("WriteGitShim() error = %v", err)
	}
	if runtime.GOOS == "windows" && filepath.Base(path) != "git.cmd" {
		t.Fatalf("windows git shim = %q, want git.cmd", path)
	}
	if runtime.GOOS != "windows" && filepath.Base(path) != "git" {
		t.Fatalf("unix git shim = %q, want git", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat git shim: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %v, want 0700", info.Mode().Perm())
	}
}

func TestBashEnvShimInterceptsAbsoluteBash(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not available")
	}
	dir := t.TempDir()
	capturePath := filepath.Join(dir, "bridge-args")
	bridgePath := filepath.Join(dir, "agent-bridge")
	if err := os.WriteFile(bridgePath, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CAPTURE_PATH\"\n"), 0o700); err != nil {
		t.Fatalf("write fake bridge: %v", err)
	}
	bashEnvPath, err := WriteBashEnvShim(dir, bridgePath)
	if err != nil {
		t.Fatalf("WriteBashEnvShim() error = %v", err)
	}

	cmd := exec.Command(bashPath, "-c", "echo local-only")
	cmd.Env = append(os.Environ(),
		"BASH_ENV="+bashEnvPath,
		"AGENT_BRIDGE_BINARY="+bridgePath,
		"CAPTURE_PATH="+capturePath,
	)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("bash with BASH_ENV failed: %v: %s", err, stderr.String())
	}
	if strings.Contains(stdout.String(), "local-only") {
		t.Fatalf("bash command ran locally; stdout = %q", stdout.String())
	}

	gotBytes, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	got := string(gotBytes)
	if !strings.Contains(got, "__bash-env\necho local-only\n") {
		t.Fatalf("captured bridge args = %q", got)
	}
}

func TestApplyEnvInjectsNodeFSShim(t *testing.T) {
	env := ApplyEnv([]string{"NODE_OPTIONS=--trace-warnings"}, Config{
		BridgePath:     "/tmp/agent-bridge",
		WorkspacePath:  "/tmp/work",
		HomePath:       "/tmp",
		StatePath:      "/tmp/state.json",
		NodeFSShimPath: "/tmp/node-fs-shim.cjs",
		RemoteRoot:     "/srv/app",
	})
	var got string
	for _, item := range env {
		if strings.HasPrefix(item, "NODE_OPTIONS=") {
			got = item
			break
		}
	}
	if !strings.Contains(got, "--require=/tmp/node-fs-shim.cjs --trace-warnings") {
		t.Fatalf("NODE_OPTIONS = %q", got)
	}
}

func TestNodeFSShimMapsRemoteAbsolutePaths(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not available")
	}

	dir := t.TempDir()
	workspace := filepath.Join(dir, "mirror")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "pyproject.toml"), []byte("[project]\nname = \"demo\"\n"), 0o600); err != nil {
		t.Fatalf("write pyproject: %v", err)
	}
	shimPath, err := WriteNodeFSShim(dir)
	if err != nil {
		t.Fatalf("WriteNodeFSShim() error = %v", err)
	}

	const script = `
const fs = require('fs');
const childProcess = require('child_process');
const remoteRoot = process.env.AGENT_BRIDGE_REMOTE_ROOT;
if (process.cwd() !== remoteRoot) {
  throw new Error('cwd not virtualized: ' + process.cwd());
}
const data = fs.readFileSync(remoteRoot + '/pyproject.toml', 'utf8');
if (!data.includes('name = "demo"')) {
  throw new Error('readFileSync did not map remote path');
}
fs.accessSync(remoteRoot + '/pyproject.toml');
const names = fs.readdirSync(remoteRoot);
if (!names.includes('pyproject.toml')) {
  throw new Error('readdirSync did not map remote path');
}
const childCwd = childProcess.execFileSync(process.execPath, ['-e', 'process.stdout.write(process.cwd())'], { cwd: remoteRoot }).toString();
if (childCwd !== remoteRoot) {
  throw new Error('child cwd not virtualized: ' + childCwd);
}
`
	cmd := exec.Command(nodePath, "-e", script)
	cmd.Dir = workspace
	cmd.Env = ApplyEnv(os.Environ(), Config{
		BridgePath:     "/tmp/agent-bridge",
		WorkspacePath:  workspace,
		HomePath:       dir,
		StatePath:      filepath.Join(dir, "state.json"),
		NodeFSShimPath: shimPath,
		RemoteRoot:     "/home/perseverance/2026/test",
	})
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("node shim failed: %v\n%s", err, stderr.String())
	}
}

func TestRunFakeShellTranslatesWorkspacePath(t *testing.T) {
	dir := t.TempDir()
	workspace := filepath.Join(dir, "work_abcd")
	statePath := filepath.Join(dir, "state.json")
	capturePath := filepath.Join(dir, "ssh-args")
	fakeBin := filepath.Join(dir, "fake-bin")
	if err := os.MkdirAll(filepath.Join(workspace, "tests"), 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "tests", "test_core.py"), []byte("def test_ok(): pass\n"), 0o600); err != nil {
		t.Fatalf("write workspace file: %v", err)
	}
	if err := os.Mkdir(fakeBin, 0o700); err != nil {
		t.Fatalf("mkdir fake bin: %v", err)
	}
	writeFakeSSH(t, filepath.Join(fakeBin, "ssh"))
	saveSnapshot(t, workspace, statePath)

	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CAPTURE_PATH", capturePath)
	setBridgeEnv(t, Config{
		BridgePath:     "/tmp/agent-bridge",
		SocketPath:     "/tmp/socket",
		Target:         "devbox",
		Port:           2222,
		PortSet:        true,
		ControlEnabled: true,
		WorkspacePath:  workspace,
		HomePath:       dir,
		StatePath:      statePath,
		RemoteRoot:     "/srv/app",
	})

	var stderr bytes.Buffer
	code := RunFakeShell([]string{"-lc", "pytest " + workspace + "/tests/test_core.py"}, bytes.NewReader(nil), &bytes.Buffer{}, &stderr)
	if code != 0 {
		t.Fatalf("RunFakeShell() code = %d, stderr = %s", code, stderr.String())
	}

	gotBytes, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	got := string(gotBytes)
	if !strings.Contains(got, "-S\n/tmp/socket\n") {
		t.Fatalf("captured ssh args missing control socket:\n%s", got)
	}
	if !strings.Contains(got, "-p\n2222\n") {
		t.Fatalf("captured ssh args missing explicit port:\n%s", got)
	}
	if !strings.Contains(got, "cd '/srv/app' && pytest ./tests/test_core.py") {
		t.Fatalf("captured ssh args missing translated command:\n%s", got)
	}
}

func TestRunFakeShellOmitsDefaultPortForSSHConfigAliases(t *testing.T) {
	dir := t.TempDir()
	workspace := filepath.Join(dir, "work_abcd")
	statePath := filepath.Join(dir, "state.json")
	capturePath := filepath.Join(dir, "ssh-args")
	fakeBin := filepath.Join(dir, "fake-bin")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	if err := os.Mkdir(fakeBin, 0o700); err != nil {
		t.Fatalf("mkdir fake bin: %v", err)
	}
	writeFakeSSH(t, filepath.Join(fakeBin, "ssh"))
	saveSnapshot(t, workspace, statePath)

	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CAPTURE_PATH", capturePath)
	setBridgeEnv(t, Config{
		BridgePath:    "/tmp/agent-bridge",
		Target:        "devbox",
		Port:          22,
		WorkspacePath: workspace,
		HomePath:      dir,
		StatePath:     statePath,
		RemoteRoot:    "/srv/app",
	})

	var stderr bytes.Buffer
	code := RunFakeShell([]string{"-c", "pwd"}, bytes.NewReader(nil), &bytes.Buffer{}, &stderr)
	if code != 0 {
		t.Fatalf("RunFakeShell() code = %d, stderr = %s", code, stderr.String())
	}

	gotBytes, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	if strings.Contains(string(gotBytes), "-p\n") {
		t.Fatalf("captured ssh args unexpectedly included -p:\n%s", gotBytes)
	}
}

func setBridgeEnv(t *testing.T, cfg Config) {
	t.Helper()
	env := ApplyEnv(nil, cfg)
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			t.Setenv(key, value)
		}
	}
}

func saveSnapshot(t *testing.T, workspace, statePath string) {
	t.Helper()
	state, err := remotesync.Snapshot(workspace)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := remotesync.SaveState(statePath, state); err != nil {
		t.Fatalf("save state: %v", err)
	}
}

func writeFakeSSH(t *testing.T, path string) {
	t.Helper()
	const script = `#!/usr/bin/env bash
set -euo pipefail
args="$*"
if [[ "$args" == *"tar -cf - ."* ]]; then
  dd if=/dev/zero bs=512 count=2 2>/dev/null
  exit 0
fi
printf '%s\n' "$@" >> "$CAPTURE_PATH"
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake ssh: %v", err)
	}
}
