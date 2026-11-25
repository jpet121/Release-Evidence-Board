package main

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func configureCLIShutdown(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

func signalCLIShutdown(cmd *exec.Cmd) error {
	// The executable is the leader of a dedicated console group. Never pass group zero.
	send := windows.NewLazySystemDLL("kernel32.dll").NewProc("GenerateConsoleCtrlEvent")
	result, _, err := send.Call(windows.CTRL_BREAK_EVENT, uintptr(cmd.Process.Pid))
	if result == 0 {
		return err
	}
	return nil
}
