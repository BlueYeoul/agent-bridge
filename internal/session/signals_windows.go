//go:build windows

package session

import (
	"os"
	"os/exec"
)

func interruptSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

func terminateChild(cmd *exec.Cmd, sig os.Signal) error {
	return cmd.Process.Signal(sig)
}

func forceKillChild(cmd *exec.Cmd) error {
	return cmd.Process.Kill()
}
