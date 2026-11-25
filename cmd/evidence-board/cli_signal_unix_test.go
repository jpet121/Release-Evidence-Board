//go:build linux || darwin

package main

import (
	"os/exec"
	"syscall"
)

func configureCLIShutdown(_ *exec.Cmd) {}

func signalCLIShutdown(cmd *exec.Cmd) error {
	return cmd.Process.Signal(syscall.SIGTERM)
}
