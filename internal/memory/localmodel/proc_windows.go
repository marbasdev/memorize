//go:build windows

package localmodel

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

func runtimeBinaryName() string {
	return "llama-server.exe"
}

func serverSysProcAttr() *syscall.SysProcAttr {
	return nil
}

func isLlamaProcess(pid int) bool {
	if pid <= 0 {
		return false
	}
	out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH").Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), "llama")
}

func terminateProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	if p, err := os.FindProcess(pid); err == nil && p != nil {
		_ = p.Kill()
	}
}
