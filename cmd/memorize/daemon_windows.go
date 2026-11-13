//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func daemonSysProcAttr() *syscall.SysProcAttr {
	return nil
}

func execInPlace(binPath string, args []string, env []string) error {
	cmd := exec.Command(binPath, args[1:]...)
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Start()
}
