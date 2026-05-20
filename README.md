# agent-bridge

`agent-bridge` is a zero-install CLI that lets a local coding agent work against a remote workspace through a sparse local projection and a command-translating shell wrapper.

## Install

macOS / Linux:

```bash
curl -fsSL https://raw.githubusercontent.com/BlueYeoul/agent-bridge/main/install.sh | sh
```

Windows PowerShell:

```powershell
irm https://raw.githubusercontent.com/BlueYeoul/agent-bridge/main/install.ps1 | iex
```

The installer downloads the latest prebuilt binary from GitHub Releases and installs it into `~/.local/bin`. If a release binary is not available yet, it falls back to `go install` when Go is installed.

To install from another fork:

```bash
AGENT_BRIDGE_REPO=owner/repo curl -fsSL https://raw.githubusercontent.com/owner/repo/main/install.sh | sh
```

## Usage

```bash
agent-bridge <remote_server_path> --target <user@hostname> --launch <agent_name> [--port <ssh_port>] [-- <agent_args>...]
```

Examples:

```bash
agent-bridge ./workspace/my-project --target devbox --launch claude
agent-bridge /srv/app --target user@example.com --port 2222 --launch aider -- --model sonnet
```

When `--port` is omitted, `ssh` uses its normal defaults, so SSH config aliases can still supply a custom port.

## Lifecycle

1. Creates an isolated session under `~/.agtbge`.
2. Opens one SSH ControlMaster socket for the target when the local SSH client supports it.
3. Resolves the remote workspace path and creates a sparse local projection under `~/.agtbge/mirrors/session_<id>/<target>/<remote_path>`.
4. Uses the `agent-bridge` binary itself as the fake shell, so Windows does not need Bash or SSHFS.
5. Launches the requested agent with `SHELL`, `PWD`, and `CWD` pointed at the local mirror.
6. Before each intercepted command, uploads local edits to the remote host.
7. Executes the translated command remotely through SSH.
8. After each command, reads a remote manifest and refreshes only changed projected files.
9. On exit or signal, performs a final upload, closes the ControlMaster socket, and removes session files.

The sync transport uses remote `find`/`tar` over SSH streams and Go's built-in archive handling locally. It does not require `sshfs`, FUSE, `rsync`, Bash, or local `tar`.

The local projection is intentionally not a full clone of the remote workspace. Large-data directories such as `data`, `datasets`, `outputs`, `checkpoints`, `runs`, `wandb`, `mlruns`, `node_modules`, virtualenvs, and `.git` are skipped by default. Individual files larger than 5 MiB are also left on the remote host. Shell commands still run in the real remote workspace, so big data remains available to Python, training scripts, tests, and other remote commands without being copied to the local machine.

If a Windows OpenSSH build does not support ControlMaster multiplexing, `agent-bridge` falls back to direct `ssh` execution instead of failing at startup.

TUI agents are launched attached to the same foreground terminal session so raw-mode input, redraws, and Ctrl+C behavior continue to work normally.

For shell execution, `agent-bridge` uses multiple interception paths:

- `SHELL` points at the bridge binary for agents that respect the environment.
- `BASH_ENV` redirects non-interactive `/bin/bash -c ...` calls, including agents that hardcode Bash.
- Known agent profiles can receive session-local settings; for `pi`, `shellPath` is set to the bridge binary without modifying the user's real `~/.pi/agent/settings.json`.
- Known agents can receive an additional system prompt explaining that the canonical project directory is the remote path and the local mirror is an implementation detail.

## Profiles

`claude` keeps the normal local environment, including local Claude credentials under `~/.config`.

`aider` is launched with `--no-git` by default and receives a PATH-local `git` shim that executes Git operations on the remote host through the multiplexed SSH socket.

## Development

```bash
make test
make build
make install
```

Create local release archives:

```bash
make snapshot
```

Publish a GitHub Release by pushing a version tag:

```bash
git tag v0.1.0
git push origin main --tags
```

The release workflow builds macOS, Linux, and Windows binaries for `amd64` and `arm64`, uploads archives, and publishes `checksums.txt`.
