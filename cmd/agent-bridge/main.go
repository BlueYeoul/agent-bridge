package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/BlueYeoul/agent-bridge/internal/cli"
	"github.com/BlueYeoul/agent-bridge/internal/remotesync"
	"github.com/BlueYeoul/agent-bridge/internal/session"
	"github.com/BlueYeoul/agent-bridge/internal/shellwrap"
	"github.com/BlueYeoul/agent-bridge/internal/sshx"
)

func main() {
	if os.Getenv("AGENT_BRIDGE_FAKE_SHELL") == "1" && (len(os.Args) == 1 || !isInternalMode(os.Args[1])) {
		os.Exit(shellwrap.RunFakeShell(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "__sync-upload":
			os.Exit(runSync(os.Args[2:], true))
		case "__sync-download":
			os.Exit(runSync(os.Args[2:], false))
		case "__git-shim":
			os.Exit(shellwrap.RunGitShim(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
		case "__bash-env":
			os.Exit(shellwrap.RunCommand(strings.Join(os.Args[2:], " "), os.Stdin, os.Stdout, os.Stderr))
		}
	}

	cfg, err := cli.Parse(os.Args[1:])
	if errors.Is(err, cli.ErrHelp) {
		fmt.Fprint(os.Stdout, cli.Usage())
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-bridge: %v\n\n%s", err, cli.Usage())
		os.Exit(2)
	}

	runner, err := session.New(cfg, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-bridge: %v\n", err)
		os.Exit(1)
	}

	code, err := runner.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-bridge: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func isInternalMode(arg string) bool {
	switch arg {
	case "__sync-upload", "__sync-download", "__git-shim", "__bash-env":
		return true
	default:
		return false
	}
}

func runSync(args []string, upload bool) int {
	fs := flag.NewFlagSet("agent-bridge internal sync", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	socket := fs.String("socket", "", "SSH control socket")
	target := fs.String("target", "", "SSH target")
	port := fs.Int("port", 22, "SSH port")
	portSet := fs.Bool("port-set", false, "whether port was explicitly set")
	controlEnabled := fs.Bool("control", false, "whether ControlMaster is enabled")
	workspace := fs.String("workspace", "", "local workspace")
	remoteRoot := fs.String("remote-root", "", "remote root")
	state := fs.String("state", "", "sync state path")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg := sshx.Config{
		SocketPath:     *socket,
		Target:         *target,
		Port:           *port,
		PortSet:        *portSet,
		ControlEnabled: *controlEnabled,
	}
	var err error
	if upload {
		err = remotesync.UploadChanges(cfg, *workspace, *remoteRoot, *state)
	} else {
		err = remotesync.Download(cfg, *workspace, *remoteRoot, *state)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-bridge sync: %v\n", err)
		return 1
	}
	return 0
}
