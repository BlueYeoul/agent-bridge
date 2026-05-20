package sshx

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

type Config struct {
	SocketPath     string
	Target         string
	Port           int
	PortSet        bool
	ControlEnabled bool
}

func (c Config) Command(remoteCommand string) *exec.Cmd {
	args := c.Args()
	args = append(args, c.Target, remoteCommand)
	return exec.Command("ssh", args...)
}

func (c Config) Args() []string {
	var args []string
	if c.ControlEnabled && c.SocketPath != "" {
		args = append(args, "-S", c.SocketPath)
	}
	if c.PortSet {
		args = append(args, "-p", strconv.Itoa(c.Port))
	}
	return args
}

func (c Config) Run(remoteCommand string, stdin io.Reader, stdout io.Writer, stderr io.Writer) (int, error) {
	cmd := c.Command(remoteCommand)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return ExitCode(cmd.Run())
}

func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func ExitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return code, nil
		}
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal()), nil
		}
		return 1, nil
	}
	return 1, err
}

func RequireFields(c Config) error {
	if c.Target == "" {
		return fmt.Errorf("missing SSH target")
	}
	if c.ControlEnabled && c.SocketPath == "" {
		return fmt.Errorf("missing SSH control socket")
	}
	return nil
}
