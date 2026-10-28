//go:build windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

func currentRecoveryHostMarker() string {
	process, err := syscall.GetCurrentProcess()
	if err != nil {
		return ""
	}
	var created, exited, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(process, &created, &exited, &kernel, &user); err != nil {
		return ""
	}
	return fmt.Sprintf("windows:%d:%d:%d", os.Getpid(), created.HighDateTime, created.LowDateTime)
}
