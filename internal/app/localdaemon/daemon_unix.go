//go:build !windows

package localdaemon

import "syscall"

func daemonSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

func execInPlace(binPath string, args []string, env []string) error {
	return syscall.Exec(binPath, args, env)
}
